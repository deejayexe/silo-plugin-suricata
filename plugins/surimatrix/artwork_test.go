package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfileArtworkFallback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer one-key" {
			t.Error("different credential used")
		}
		switch r.URL.Path {
		case "/api/v2/account/me":
			w.Write([]byte(`{"id":"2","username":"admin-normal","role":"admin"}`))
			return
		case "/api/v2/profiles":
			w.Write([]byte(`{"items":[{"id":"secondary","name":"Kids","is_primary":false},{"id":"internal-primary","name":"Administrador","is_primary":true}]}`))
			return
		case "/api/v2/catalog/items/movie-42":
			if r.Header.Get("X-Profile-Id") != "internal-primary" {
				t.Error("primary profile not automatically resolved")
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"item":{"content_id":"movie-42","poster_url":"/api/v2/artwork/poster?signature=opaque","overview":"Sinopsis"}}}`))
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	c := config{SiloURL: srv.URL, SiloKey: "one-key", SiloUsername: "admin-normal"}
	i, err := enrich(context.Background(), c, item{ID: "movie-42", Title: "Example", Type: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if i.Poster != srv.URL+"/api/v2/artwork/poster?signature=opaque" || i.Overview != "Sinopsis" {
		t.Fatalf("wrong artwork: %+v", i)
	}
}
func TestProfileArtworkFailureIsRetryable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	_, err := enrich(context.Background(), config{SiloURL: srv.URL, SiloKey: "bad-key"}, item{ID: "movie-1"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatal(err)
	}
}
func TestArtworkURL(t *testing.T) {
	got, e := artworkURL("https://silo.example", "/api/v2/artwork/x?sig=1")
	if e != nil || got != "https://silo.example/api/v2/artwork/x?sig=1" {
		t.Fatal(got, e)
	}
	for _, s := range []string{"http://example/image", "https://user:pass@example/image", "file:///tmp/test"} {
		if _, e = artworkURL("https://silo.example", s); e == nil {
			t.Fatal("accepted", s)
		}
	}
}
func TestEmptyQualityOmitted(t *testing.T) {
	var i item
	if e := json.Unmarshal([]byte(`{"title":"Movie","versions":[{"resolution":"","codec_video":"","codec_audio":""}]}`), &i); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(formatBatch(&batch{}, i, nil, "https://silo.example"), "Resolution:") {
		t.Fatal("empty quality included")
	}
}
