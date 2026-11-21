package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScanMovieFilesQueuesEachNewFileWithTheUsualFormat(t *testing.T) {
	now := time.Now().UTC()
	movie := item{ID: "movie-1", Type: "movie", Title: "Example", Added: now.Add(-24 * time.Hour), Libraries: []string{"movies", "other"}}
	original := catalogFile{ID: "1", LibraryID: "movies", Resolution: "2160p"}
	newFile := catalogFile{ID: "2", LibraryID: "movies", Resolution: "1080p", Video: "hevc"}
	sameFormat := newFile
	sameFormat.ID = "3"
	noMetadata := catalogFile{ID: "4", LibraryID: "movies"}
	excluded := catalogFile{ID: "5", LibraryID: "other"}

	var versions atomic.Pointer[[]catalogFile]
	versions.Store(new([]catalogFile{original}))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v2/admin/catalog/items" || q.Get("type") != "movie" || q.Has("added_after") {
			t.Error("the file scan must include previously added movies")
		}
		i := movie
		i.Versions = *versions.Load()
		if err := json.NewEncoder(w).Encode(catalogPage{Items: []item{i}}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })

	c := config{SiloURL: srv.URL, Libraries: []string{"movies"}}
	st := progress{Seen: map[string]time.Time{movie.key(): movie.Added}, Pending: map[string]*batch{}}
	if err := scanMovieFiles(t.Context(), c, &st, now); err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 || !slices.Equal(st.MovieFiles[movie.ID], []string{"1"}) {
		t.Fatalf("first observation queued a message or failed to record the file: %+v", st)
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := saveJSON(path, &st); err != nil {
		t.Fatal(err)
	}
	st = progress{}
	if err := loadJSON(path, &st); err != nil {
		t.Fatal(err)
	}

	versions.Store(new([]catalogFile{original, newFile, sameFormat, noMetadata, excluded}))
	if err := scanMovieFiles(t.Context(), c, &st, now); err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 3 {
		t.Fatalf("expected three new file notifications, got %d", len(st.Pending))
	}
	for _, file := range []catalogFile{newFile, sameFormat, noMetadata} {
		i := movie
		i.FileID = file.ID
		b := st.Pending[i.key()]
		if b == nil || len(b.Items) != 1 || !b.Due.Equal(now) {
			t.Fatalf("file %s did not receive its own immediate notification", file.ID)
		}
		got := b.Items[0]
		if !slices.Equal(got.Versions, []catalogFile{file}) || !slices.Equal(got.Libraries, []string{"movies"}) {
			t.Fatalf("file %s notification has incorrect format metadata or library: %+v", file.ID, got)
		}
		message := formatBatch(b, got, map[string]string{"movies": "Movies"}, "Silo")
		if !strings.HasPrefix(message, "Silo\n🎬 🆕 New movie in Movies\nTitle: Example") {
			t.Fatalf("file %s did not keep the usual movie format: %s", file.ID, message)
		}
	}

	clear(st.Pending)
	updated := original
	updated.Video = "av1"
	versions.Store(new([]catalogFile{updated}))
	if err := scanMovieFiles(t.Context(), c, &st, now); err != nil {
		t.Fatal(err)
	}
	versions.Store(new([]catalogFile{updated, newFile, sameFormat, noMetadata, excluded}))
	c.Libraries = nil
	if err := scanMovieFiles(t.Context(), c, &st, now); err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 {
		t.Fatal("already observed files were queued again")
	}
}

func TestScanMovieFilesResumesAndRecoversInvalidCursor(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor == "expired" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		page := catalogPage{}
		if len(cursor) < 5 {
			page.Page.HasMore = true
			page.Page.Next = cursor + "x"
		}
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })

	c := config{SiloURL: srv.URL}
	now := time.Now().UTC()
	st := progress{MovieFileCursor: "expired"}
	if err := scanMovieFiles(context.Background(), c, &st, now); err == nil || st.MovieFileCursor != "" {
		t.Fatalf("expected an invalid cursor to reset, got %q and %v", st.MovieFileCursor, err)
	}
	if err := scanMovieFiles(t.Context(), c, &st, now); err != nil || st.MovieFileCursor != "xxxxx" {
		t.Fatalf("expected the task to save its continuation, got %q and %v", st.MovieFileCursor, err)
	}
	if err := scanMovieFiles(t.Context(), c, &st, now); err != nil || st.MovieFileCursor != "" {
		t.Fatalf("expected the last page to finish the scan, got %q and %v", st.MovieFileCursor, err)
	}
}
