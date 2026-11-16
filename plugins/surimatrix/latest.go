package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	runtime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

const defaultNotificationTitle = "Silo"

// The old hard-coded default is treated as automatic after an upgrade.
// Store only its fingerprint; personal server names do not belong in the source.
const legacyTitleFingerprint = "50f006b6a3df9866268f213d099568721da80adf55cfdedda0bae3bb90a34969"

func notificationTitle(ctx context.Context, c config) string {
	if s := strings.TrimSpace(c.MessageTitle); s != "" && fmt.Sprintf("%x", sha256.Sum256([]byte(s))) != legacyTitleFingerprint {
		return s
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// This is the same public branding endpoint used by Silo's sign-in page.
	// Its server_name resolves Settings > Identity, including Silo's own default.
	var branding struct {
		ServerName string `json:"server_name"`
	}
	if c.SiloURL != "" && siloJSON(ctx, c, "/api/v2/theme/branding", "", "", &branding) == nil {
		if name := strings.TrimSpace(branding.ServerName); name != "" {
			return name
		}
	}
	// Older hosts may not serve the branding endpoint but expose the name via RPC.
	if host := runtime.Host(); host != nil {
		if info, err := host.GetHostInfo(ctx); err == nil && info.HostRole != "proxy" {
			if name := strings.TrimSpace(info.HostName); name != "" {
				return name
			}
		}
	}
	return defaultNotificationTitle
}

type libraryOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func libraryOptions(ctx context.Context, c config) ([]libraryOption, error) {
	h := runtime.Host()
	if h == nil {
		return nil, errors.New("Silo host connection is not available")
	}
	libs, err := h.ListLibraries(ctx, "")
	if err != nil {
		return nil, errors.New("Unable to list Silo libraries")
	}
	out := []libraryOption{}
	for _, l := range libs {
		if len(c.Libraries) == 0 || slices.Contains(c.Libraries, l.GetId()) {
			out = append(out, libraryOption{ID: l.GetId(), Name: l.GetName()})
		}
	}
	return out, nil
}
func (p *plugin) handleLibraries(ctx context.Context) *pb.HandleHTTPResponse {
	c, _ := loadConfig(ctx)
	libs, err := libraryOptions(ctx, c)
	if err != nil {
		return jsonResponse(502, map[string]string{"error": err.Error()})
	}
	return jsonResponse(200, map[string]any{"libraries": libs})
}
func manualLibrary(req *pb.HandleHTTPRequest, c config) (string, error) {
	var body struct {
		Library string `json:"library_id"`
	}
	if len(req.GetBody()) > 4096 {
		return "", errors.New("Request body is too large")
	}
	if len(req.GetBody()) > 0 {
		if err := json.Unmarshal(req.GetBody(), &body); err != nil {
			return "", errors.New("Invalid request body")
		}
	}
	if body.Library != "" && len(c.Libraries) > 0 && !slices.Contains(c.Libraries, body.Library) {
		return "", errors.New("This library is outside the configured library filter")
	}
	return body.Library, nil
}

// Silo's admin catalog is ordered by content ID, not addition time. Inspect
// every page in the newest nonempty time window; never assume its first row
// is the newest. Manual sends do not read or write the automatic queue.
func latestItem(ctx context.Context, c config, kind, library string, now time.Time) (item, error) {
	if kind != "movie" && kind != "episode" {
		return item{}, errors.New("Unsupported media type")
	}
	windows := []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 365 * 24 * time.Hour, 0}
	for _, window := range windows {
		q := url.Values{"type": {kind}, "limit": {"200"}}
		if library != "" {
			q.Set("library_id", library)
		}
		if window > 0 {
			q.Set("added_after", now.Add(-window).UTC().Format(time.RFC3339Nano))
		}
		best := item{}
		cursor := ""
		visited := map[string]bool{}
		for pageNumber := 0; ; pageNumber++ {
			if pageNumber >= 200 {
				return item{}, errors.New("Too many catalog pages to safely determine the latest item; select one library and try again")
			}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var page struct {
				Items []item `json:"items"`
				Page  struct {
					More bool   `json:"has_more"`
					Next string `json:"next_cursor"`
				} `json:"page"`
			}
			if err := getJSON(ctx, c, "/api/v2/admin/catalog/items?"+q.Encode(), &page); err != nil {
				return item{}, fmt.Errorf("Read latest content: %w", err)
			}
			for _, i := range page.Items {
				if i.Type != kind || i.ID == "" || i.Added.IsZero() || i.Added.After(now) || window > 0 && i.Added.Before(now.Add(-window)) {
					continue
				}
				if len(c.Libraries) > 0 && !slices.ContainsFunc(i.Libraries, func(id string) bool { return slices.Contains(c.Libraries, id) }) {
					continue
				}
				if best.ID == "" || i.Added.After(best.Added) || i.Added.Equal(best.Added) && i.ID > best.ID {
					best = i
				}
			}
			if !page.Page.More {
				break
			}
			if page.Page.Next == "" || page.Page.Next == cursor || visited[page.Page.Next] {
				return item{}, errors.New("Invalid catalog continuation cursor")
			}
			cursor = page.Page.Next
			visited[cursor] = true
		}
		if best.ID != "" {
			return best, nil
		}
	}
	return item{}, errors.New("No matching content was found in the selected libraries")
}
