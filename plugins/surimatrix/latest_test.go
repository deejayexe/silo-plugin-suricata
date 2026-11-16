package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLatestItemUsesAddedTimeAcrossPages(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if q.Get("type") != "movie" || q.Get("library_id") != "library-1" {
			t.Error("missing media/library filter")
		}
		if r.Header.Get("Authorization") != "Bearer one-key" {
			t.Error("wrong key")
		}
		w.Header().Set("Content-Type", "application/json")
		if q.Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"items": []item{{ID: "movie-a", Type: "movie", Added: now.Add(-2 * time.Hour)}}, "page": map[string]any{"has_more": true, "next_cursor": "next"}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"items": []item{{ID: "movie-z", Type: "movie", Added: now.Add(-time.Hour)}}})
		}
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	got, err := latestItem(context.Background(), config{SiloURL: srv.URL, SiloKey: "one-key"}, "movie", "library-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "movie-z" || requests != 2 {
		t.Fatalf("wrong latest item: %+v, requests %d", got, requests)
	}
}
func TestLatestEpisodeExpandsDateWindow(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("type") != "episode" {
			t.Error("series test must select episodes")
		}
		since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("added_after"))
		if err != nil {
			t.Fatal(err)
		}
		if since.After(now.Add(-3 * 24 * time.Hour)) {
			w.Write([]byte(`{"items":[]}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"items": []item{{ID: "episode-1", Type: "episode", Added: now.Add(-3 * 24 * time.Hour)}}})
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	got, err := latestItem(context.Background(), config{SiloURL: srv.URL, SiloKey: "one-key"}, "episode", "", now)
	if err != nil || got.ID != "episode-1" || requests != 2 {
		t.Fatal(got, err, requests)
	}
}
func TestNotificationLayout(t *testing.T) {
	var i item
	err := json.Unmarshal([]byte(`{"type":"movie","title":"Example","year":2026,"rating_tmdb":6.2,"versions":[{"resolution":"1080p","codec_video":"hevc","codec_audio":"dts"}]}`), &i)
	if err != nil {
		t.Fatal(err)
	}
	text := formatBatch(&batch{Items: []item{i}}, i, nil, notificationTitle(context.Background(), config{}))
	if !strings.HasPrefix(text, "Silo\n") || !strings.Contains(text, "Rating: TMDB 6.2 / 10\n--------------------\nResolution: 1080p · hevc · dts") {
		t.Fatal(text)
	}
	for _, forbidden := range []string{"Open Silo", "https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatal(text)
		}
	}
	if notificationTitle(context.Background(), config{MessageTitle: "My server"}) != "My server" {
		t.Fatal("custom title ignored")
	}
}
func TestRetiredFieldsAreNotExposed(t *testing.T) {
	var doc struct {
		Schemas []struct {
			Schema string `json:"json_schema"`
		} `json:"global_config_schema"`
	}
	if err := json.Unmarshal(manifest, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Schemas {
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal([]byte(s.Schema), &schema); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"poster_api_key", "profile_id"} {
			if _, ok := schema.Properties[key]; ok {
				t.Fatal("retired field still rendered", key)
			}
		}
	}
}
