package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Silo's item added_at does not change when another file is added to a movie.
// Walk the movie catalog in bounded pages, resuming where the last run stopped.
func scanMovieFiles(ctx context.Context, c config, st *progress, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	q := url.Values{"type": {"movie"}, "limit": {"200"}}
	for range 5 {
		q.Set("cursor", st.MovieFileCursor)
		var page catalogPage
		if err := getJSON(ctx, c, "/api/v2/admin/catalog/items?"+q.Encode(), &page); err != nil {
			if errors.Is(err, siloHTTPStatus(http.StatusBadRequest)) {
				st.MovieFileCursor = ""
			}
			return fmt.Errorf("Read Silo movie files: %w", err)
		}
		if page.Page.HasMore && (page.Page.Next == "" || page.Page.Next == st.MovieFileCursor) {
			st.MovieFileCursor = ""
			return errors.New("Invalid Silo movie catalog cursor")
		}
		for _, i := range page.Items {
			st.observeMovieFiles(i, c.Libraries, now)
		}
		if !page.Page.HasMore {
			st.MovieFileCursor = ""
			break
		}
		st.MovieFileCursor = page.Page.Next
	}
	return nil
}

func (st *progress) observeMovieFiles(i item, libraries []string, now time.Time) {
	if i.Type != "movie" || i.ID == "" || len(i.Versions) == 0 {
		return
	}
	known := st.MovieFiles[i.ID]
	firstObservation := len(known) == 0
	for _, file := range i.Versions {
		if file.ID == "" || slices.Contains(known, file.ID) {
			continue
		}
		known = append(known, file.ID)
		if firstObservation || (len(libraries) > 0 && !slices.Contains(libraries, file.LibraryID)) {
			continue
		}
		notification := i
		notification.FileID = file.ID
		notification.Versions = []catalogFile{file}
		if file.LibraryID != "" {
			notification.Libraries = []string{file.LibraryID}
		}
		enqueue(st, notification, now)
	}
	if len(known) > 0 {
		if st.MovieFiles == nil {
			st.MovieFiles = make(map[string][]string)
		}
		// Keep IDs for temporarily missing files so they do not trigger another alert.
		st.MovieFiles[i.ID] = known
	}
}
