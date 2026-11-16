package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

func artworkURL(base, poster string) (string, error) {
	if strings.TrimSpace(poster) == "" {
		return "", nil
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(poster)
	if err != nil {
		return "", errors.New("Invalid poster reference")
	}
	resolved := b.ResolveReference(ref).String()
	if err = httpsURL(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}
func detailObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	for n := 0; n < 5; n++ {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		if obj == nil {
			return nil, errors.New("Empty catalog detail")
		}
		if nested, ok := obj["item"]; ok {
			raw = nested
			continue
		}
		if nested, ok := obj["data"]; ok {
			raw = nested
			continue
		}
		return obj, nil
	}
	return nil, errors.New("The catalog response is nested too deeply")
}

// Silo's administrator listing omits posters. Read the native catalog detail
// with the same API key and the account's automatically resolved primary profile.
func enrich(ctx context.Context, c config, i item) (item, error) {
	if i.Poster == "" {
		if c.profileID == "" {
			identity, err := resolveSiloIdentity(ctx, c)
			if err != nil {
				return i, err
			}
			c.profileID = identity.profileID
		}
		lookupID := i.ID
		if i.Type == "episode" && i.SeriesID != "" {
			lookupID = i.SeriesID
		}
		var raw json.RawMessage
		if err := siloJSON(ctx, c, "/api/v2/catalog/items/"+url.PathEscape(lookupID), c.SiloKey, c.profileID, &raw); err != nil {
			return i, fmt.Errorf("Read Silo poster detail: %w", err)
		}
		obj, err := detailObject(raw)
		if err != nil {
			return i, err
		}
		var detail item
		b, _ := json.Marshal(obj)
		if err = json.Unmarshal(b, &detail); err != nil {
			return i, err
		}
		for _, key := range []string{"poster_url", "season_poster_url", "series_poster_url"} {
			var value string
			if json.Unmarshal(obj[key], &value) == nil && strings.TrimSpace(value) != "" {
				i.Poster = value
				break
			}
		}
		if i.Overview == "" {
			i.Overview = detail.Overview
		}
		if len(i.Genres) == 0 {
			i.Genres = detail.Genres
		}
		if i.Rating == nil {
			i.Rating = detail.Rating
		}
	}
	if i.Poster != "" {
		resolved, err := artworkURL(c.SiloURL, i.Poster)
		if err != nil {
			return i, fmt.Errorf("Poster URL: %w", err)
		}
		i.Poster = resolved
	}
	if i.Poster == "" {
		slog.Warn("Surimatrix: no poster available; sending encrypted text", "content_id", i.ID)
	}
	return i, nil
}
