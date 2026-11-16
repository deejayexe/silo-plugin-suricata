package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type siloIdentity struct {
	Username    string `json:"username"`
	ProfileName string `json:"profile_name"`
	profileID   string
}

func resolveSiloIdentity(ctx context.Context, c config) (siloIdentity, error) {
	var result siloIdentity
	if strings.TrimSpace(c.SiloKey) == "" {
		return result, errors.New("Configure a Silo API key")
	}
	var account struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := getJSON(ctx, c, "/api/v2/account/me", &account); err != nil {
		return result, fmt.Errorf("Identify Silo account: %w; use an administrator API key without scope restrictions. admin:monitor:read cannot read item details", err)
	}
	if account.ID == "" || account.Username == "" || account.Role != "admin" {
		return result, errors.New("The Silo API key must belong to an administrator")
	}
	if expected := strings.TrimSpace(c.SiloUsername); expected != "" && !strings.EqualFold(expected, account.Username) {
		return result, errors.New("The configured username does not match the Silo API key owner")
	}
	var profiles struct {
		Items []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Primary bool   `json:"is_primary"`
		} `json:"items"`
	}
	if err := getJSON(ctx, c, "/api/v2/profiles", &profiles); err != nil {
		return result, fmt.Errorf("Resolve the primary Silo profile: %w", err)
	}
	count := 0
	for _, p := range profiles.Items {
		if p.Primary && p.ID != "" {
			count++
			result = siloIdentity{Username: account.Username, ProfileName: p.Name, profileID: p.ID}
		}
	}
	if count != 1 {
		return siloIdentity{}, errors.New("Silo did not return exactly one primary profile for the API key account")
	}
	return result, nil
}
func siloSample(ctx context.Context, c config) (item, error) {
	var page struct {
		Items []item `json:"items"`
	}
	if err := getJSON(ctx, c, "/api/v2/admin/catalog/items?limit=1", &page); err != nil {
		return item{}, fmt.Errorf("Read the administrative catalog: %w", err)
	}
	if len(page.Items) == 0 || page.Items[0].ID == "" {
		return item{}, errors.New("The catalog does not contain an item for the poster test")
	}
	return enrich(ctx, c, page.Items[0])
}
func jsonResponse(code int, value any) *pb.HandleHTTPResponse {
	body, _ := json.Marshal(value)
	return response(code, "application/json", body)
}
func (p *plugin) handleSiloTest(ctx context.Context, req *pb.HandleHTTPRequest) *pb.HandleHTTPResponse {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c, err := loadConfig(ctx)
	if err != nil {
		return jsonResponse(502, map[string]string{"error": err.Error()})
	}
	identity, err := resolveSiloIdentity(ctx, c)
	if err != nil {
		return jsonResponse(502, map[string]string{"error": err.Error()})
	}
	c.profileID = identity.profileID
	var sample item
	if req.GetPath() == "/api/send-latest-movie" || req.GetPath() == "/api/send-latest-episode" {
		library, parseErr := manualLibrary(req, c)
		if parseErr != nil {
			return jsonResponse(400, map[string]string{"error": parseErr.Error()})
		}
		kind := "movie"
		if req.GetPath() == "/api/send-latest-episode" {
			kind = "episode"
		}
		sample, err = latestItem(ctx, c, kind, library, time.Now().UTC())
		if err == nil {
			sample, err = enrich(ctx, c, sample)
		}
	} else {
		sample, err = siloSample(ctx, c)
	}
	if err != nil {
		return jsonResponse(502, map[string]string{"error": err.Error()})
	}
	if req.GetPath() == "/api/check-silo" {
		return jsonResponse(200, map[string]any{"username": identity.Username, "profile_name": identity.ProfileName, "poster_available": sample.Poster != "", "sample_title": sample.Title})
	}
	if sample.Poster == "" && req.GetPath() == "/api/test-poster" {
		return jsonResponse(422, map[string]string{"error": "Silo did not return a poster for the sample item. Check its details in Silo."})
	}
	err = withMatrix(ctx, c, func(m *matrixClient) error {
		var data []byte
		mime := ""
		if sample.Poster != "" {
			var downloadErr error
			data, mime, downloadErr = downloadPoster(ctx, sample.Poster)
			if downloadErr != nil {
				return errors.New("Unable to download the Silo poster; a reachable public HTTPS URL is required")
			}
		}
		names := map[string]string{}
		if libs, e := libraryOptions(ctx, c); e == nil {
			for _, l := range libs {
				names[l.ID] = l.Name
			}
		}
		text := formatBatch(&batch{Items: []item{sample}}, sample, names, notificationTitle(c))
		return m.send(ctx, "poster-test-"+strconv.FormatInt(time.Now().UnixNano(), 10), text, data, mime)
	})
	if err != nil {
		return jsonResponse(502, map[string]string{"error": err.Error()})
	}
	return jsonResponse(200, map[string]bool{"ok": true, "poster_available": sample.Poster != ""})
}
