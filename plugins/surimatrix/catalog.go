package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	runtime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

type item struct {
	ID        string    `json:"content_id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Year      int       `json:"year"`
	Series    string    `json:"series_title"`
	SeriesID  string    `json:"series_id"`
	Season    int       `json:"season_number"`
	Episode   int       `json:"episode_number"`
	Libraries []string  `json:"library_ids"`
	Added     time.Time `json:"added_at"`
	Poster    string    `json:"poster_url"`
	Overview  string    `json:"overview"`
	Genres    []string  `json:"genres"`
	Rating    *float64  `json:"rating_tmdb"`
	Versions  []struct {
		Resolution string `json:"resolution"`
		Video      string `json:"codec_video"`
		Audio      string `json:"codec_audio"`
	} `json:"versions"`
}
type batch struct {
	Frozen bool      `json:"frozen"`
	First  time.Time `json:"first"`
	Due    time.Time `json:"due"`
	Items  []item    `json:"items"`
}
type progress struct {
	Since      time.Time            `json:"since"`
	Until      time.Time            `json:"until"`
	Enrollment time.Time            `json:"enrollment"`
	Cursor     string               `json:"cursor"`
	Seen       map[string]time.Time `json:"seen"`
	Pending    map[string]*batch    `json:"pending"`
}

func (i item) key() string { return i.ID + ":" + i.Added.UTC().Format(time.RFC3339Nano) }
func groupKey(i item) string {
	if i.Type == "episode" && i.SeriesID != "" {
		return "season:" + i.SeriesID + ":" + strconv.Itoa(i.Season) + ":" + strings.Join(i.Libraries, ",")
	}
	// Without a stable series ID, do not merge unrelated shows with equal titles.
	return i.key()
}
func enqueue(st *progress, i item, now time.Time) {
	if _, ok := st.Seen[i.key()]; ok {
		return
	}
	for _, pending := range st.Pending {
		for _, old := range pending.Items {
			if old.key() == i.key() {
				return
			}
		}
	}
	k := groupKey(i)
	b := st.Pending[k]
	if b != nil && b.Frozen {
		k += ":" + i.key()
		b = st.Pending[k]
	}
	if b == nil {
		b = &batch{First: now}
		st.Pending[k] = b
	}
	b.Items = append(b.Items, i)
	b.Due = now
	if i.Type == "episode" && i.SeriesID != "" {
		b.Due = now.Add(5 * time.Minute)
		if maxDue := b.First.Add(30 * time.Minute); b.Due.After(maxDue) {
			b.Due = maxDue
		}
	}
}
func getJSON(ctx context.Context, c config, path string, v any) error {
	return siloJSON(ctx, c, path, c.SiloKey, "", v)
}
func siloJSON(ctx context.Context, c config, path, key, profile string, v any) error {
	if err := httpsURL(c.SiloURL); err != nil {
		return err
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.SiloURL, "/")+path, nil)
	if e != nil {
		return e
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if profile != "" {
		req.Header.Set("X-Profile-Id", profile)
	}
	req.Header.Set("Accept", "application/json")
	cli := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	r, e := cli.Do(req)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("Silo API: HTTP %d", r.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}
func scan(ctx context.Context, c config, m *matrixClient) error {
	if e := httpsURL(c.SiloURL); e != nil {
		return e
	}
	if c.SiloKey == "" {
		return errors.New("A Silo API key is required")
	}
	identity, err := resolveSiloIdentity(ctx, c)
	if err != nil {
		return err
	}
	c.profileID = identity.profileID
	now := time.Now().UTC()
	path := filepath.Join(m.dir, "catalog.json")
	st := progress{Seen: map[string]time.Time{}, Pending: map[string]*batch{}}
	if e := loadJSON(path, &st); e != nil {
		return e
	}
	if st.Seen == nil || st.Pending == nil {
		return errors.New("Invalid catalog state")
	}
	if st.Enrollment.IsZero() {
		st.Enrollment = now
		st.Since = now
		return saveJSON(path, &st)
	}
	// Fixed upper bound, resumable cursor, inclusive overlap and per-item deduplication.
	if st.Until.IsZero() {
		st.Until = now
	}
	q := url.Values{"limit": {"40"}, "added_after": {st.Since.Add(-2 * time.Minute).Format(time.RFC3339Nano)}}
	if st.Cursor != "" {
		q.Set("cursor", st.Cursor)
	}
	var page struct {
		Items []item `json:"items"`
		Page  struct {
			HasMore bool   `json:"has_more"`
			Next    string `json:"next_cursor"`
		} `json:"page"`
	}
	if e := getJSON(ctx, c, "/api/v2/admin/catalog/items?"+q.Encode(), &page); e != nil {
		return e
	}
	if page.Page.HasMore && (page.Page.Next == "" || page.Page.Next == st.Cursor) {
		return errors.New("Invalid catalog cursor")
	}
	for _, i := range page.Items {
		if i.ID == "" || i.Added.IsZero() || i.Added.Before(st.Enrollment) || i.Added.Before(st.Since.Add(-2*time.Minute)) || i.Added.After(st.Until) {
			continue
		}
		if len(c.Libraries) > 0 && !slices.ContainsFunc(i.Libraries, func(s string) bool { return slices.Contains(c.Libraries, s) }) {
			continue
		}
		enqueue(&st, i, now)
	}
	if page.Page.HasMore {
		st.Cursor = page.Page.Next
	} else {
		st.Cursor = ""
		st.Since = st.Until
		st.Until = time.Time{}
	}
	// Persist queue before networking. Matrix transaction IDs survive a crash after send.
	if e := saveJSON(path, &st); e != nil {
		return e
	}
	names := map[string]string{}
	if h := runtime.Host(); h != nil {
		if libs, e := h.ListLibraries(ctx, ""); e == nil {
			for _, l := range libs {
				names[l.GetId()] = l.GetName()
			}
		}
	}
	keys := make([]string, 0, len(st.Pending))
	for k := range st.Pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	messageTitle := ""
	for _, k := range keys {
		b := st.Pending[k]
		if b.Due.After(now) {
			continue
		}
		// Do not flush an episode group while a catalog pagination window is incomplete.
		if st.Cursor != "" && len(b.Items) > 0 && b.Items[0].Type == "episode" {
			continue
		}
		if len(b.Items) == 0 {
			delete(st.Pending, k)
			continue
		}
		b.Frozen = true
		if e := saveJSON(path, &st); e != nil {
			return e
		}
		first, enrichErr := enrich(ctx, c, b.Items[0])
		if enrichErr != nil {
			return enrichErr
		}
		if messageTitle == "" {
			messageTitle = notificationTitle(ctx, c)
		}
		text := formatBatch(b, first, names, messageTitle)
		var data []byte
		mime := ""
		if first.Poster != "" {
			var e error
			data, mime, e = downloadPoster(ctx, first.Poster)
			if e != nil {
				return fmt.Errorf("Poster pending retry: %w", e)
			}
		}
		ids := make([]string, len(b.Items))
		for j, i := range b.Items {
			ids[j] = i.key()
		}
		sort.Strings(ids)
		if e := m.send(ctx, strings.Join(ids, "|"), text, data, mime); e != nil {
			return e
		}
		for _, i := range b.Items {
			st.Seen[i.key()] = i.Added
		}
		delete(st.Pending, k)
		if e := saveJSON(path, &st); e != nil {
			return e
		}
	}
	for k, t := range st.Seen {
		if t.Before(st.Since.Add(-24 * time.Hour)) {
			delete(st.Seen, k)
		}
	}
	return saveJSON(path, &st)
}
func formatBatch(b *batch, first item, names map[string]string, messageTitle string) string {
	libs := make([]string, 0, len(first.Libraries))
	for _, l := range first.Libraries {
		if names[l] != "" {
			libs = append(libs, names[l])
		} else {
			libs = append(libs, l)
		}
	}
	heading := "🆕 New content"
	switch first.Type {
	case "movie":
		heading = "🎬 🆕 New movie"
	case "episode":
		heading = "📺 🆕 New episodes"
	}
	title := first.Title
	if first.Series != "" {
		title = first.Series
	}
	if first.Year > 0 {
		title += fmt.Sprintf(" (%d)", first.Year)
	}
	if strings.TrimSpace(messageTitle) == "" {
		messageTitle = defaultNotificationTitle
	}
	if len(libs) > 0 {
		heading += " in " + strings.Join(libs, ", ")
	}
	lines := []string{messageTitle, heading, "Title: " + title}
	if first.Type == "episode" {
		sorted := append([]item(nil), b.Items...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].Episode < sorted[b].Episode })
		for _, i := range sorted {
			lines = append(lines, fmt.Sprintf("• %dx%02d · %s", i.Season, i.Episode, i.Title))
		}
	}
	if len(first.Genres) > 0 {
		lines = append(lines, "Genre: "+strings.Join(first.Genres, ", "))
	}
	if first.Rating != nil {
		lines = append(lines, fmt.Sprintf("Rating: TMDB %.1f / 10", *first.Rating))
	}
	if len(first.Versions) > 0 {
		v := first.Versions[0]
		parts := []string{}
		for _, value := range []string{v.Resolution, v.Video, v.Audio} {
			if value = strings.TrimSpace(value); value != "" {
				parts = append(parts, value)
			}
		}
		if len(parts) > 0 {
			if first.Rating != nil {
				lines = append(lines, "--------------------")
			}
			lines = append(lines, "Resolution: "+strings.Join(parts, " · "))
		}
	}
	if first.Overview != "" {
		r := []rune(first.Overview)
		if len(r) > 600 {
			r = append(r[:599], '…')
		}
		lines = append(lines, string(r))
	}
	text := strings.Join(lines, "\n")
	r := []rune(text)
	if len(r) > 12000 {
		text = string(r[:11999]) + "…"
	}
	return text
}
