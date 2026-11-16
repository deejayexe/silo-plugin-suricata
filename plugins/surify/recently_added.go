package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

const capRecentlyAdded = "recently_added"
const alertRecentlyAdded = "library.added"
const recentStateKey = "recently_added"
const recentPageSize = 20

type catalogAddition struct {
	Genres        []string  `json:"genres,omitempty"`
	RatingTMDB    *float64  `json:"rating_tmdb,omitempty"`
	RatingIMDb    *float64  `json:"rating_imdb,omitempty"`
	ContentRating string    `json:"content_rating,omitempty"`
	ContentID     string    `json:"content_id"`
	Type          string    `json:"type"`
	Title         string    `json:"title"`
	Year          int       `json:"year"`
	SeriesTitle   string    `json:"series_title"`
	Season        *int      `json:"season_number"`
	Episode       *int      `json:"episode_number"`
	LibraryIDs    []string  `json:"library_ids"`
	AddedAt       time.Time `json:"added_at"`
	Versions      []struct {
		Resolution string `json:"resolution"`
		Video      string `json:"codec_video"`
		Audio      string `json:"codec_audio"`
	} `json:"versions"`
	// Optional on newer hosts. Current admin catalog rows omit artwork and overview.
	PosterURL string `json:"poster_url,omitempty"`
	Overview  string `json:"overview,omitempty"`
}
type recentProgress struct {
	Enrollment time.Time `json:"enrollment"`
	Since      time.Time `json:"since"`
	Until      time.Time `json:"until,omitempty"`
	Cursor     string    `json:"cursor,omitempty"`
}
type recentState struct {
	Lease      string                    `json:"lease,omitempty"`
	LeaseUntil time.Time                 `json:"lease_until,omitempty"`
	Admins     map[string]recentProgress `json:"admins"`
}

func (p *Plugin) readRecent(ctx context.Context) (recentState, error) {
	st := recentState{Admins: map[string]recentProgress{}}
	raw, ok, err := p.store.state.Read(ctx, recentStateKey)
	if err != nil {
		return st, err
	}
	if ok {
		if err = json.Unmarshal(raw, &st); err != nil {
			return st, fmt.Errorf("read recently-added progress: %w", err)
		}
	}
	if st.Admins == nil {
		st.Admins = map[string]recentProgress{}
	}
	return st, nil
}
func (p *Plugin) writeRecent(ctx context.Context, st recentState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(raw) > 200<<10 {
		return errors.New("recently-added progress is full")
	}
	return p.store.state.Write(ctx, recentStateKey, raw)
}

// One bounded catalog page per administrator/run, resumable across restarts.
// The watermark advances only after the whole cursor window succeeds.
func (p *Plugin) runRecentlyAdded(ctx context.Context, now time.Time) (map[string]any, error) {
	out := map[string]any{"pages": 0}
	if !p.recentMu.TryLock() {
		out["status"] = "already running"
		return out, nil
	}
	defer p.recentMu.Unlock()
	cfg := p.config()
	if cfg.MonitorAPIKey == "" {
		out["status"] = "read-only Silo key required"
		return out, nil
	}
	admins, err := p.admins.List(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(admins, func(a *AdminSettings) bool { return a.alert(alertRecentlyAdded).Enabled }) {
		out["status"] = "no administrator opted in"
		return out, nil
	}
	st, err := p.readRecent(ctx)
	if err != nil {
		return nil, err
	}
	if st.Lease != "" && st.LeaseUntil.After(now) {
		out["status"] = "another process is running"
		return out, nil
	}
	st.Lease, st.LeaseUntil = p.pending.id, now.Add(leaseTTL)
	if err = p.writeRecent(ctx, st); err != nil {
		return nil, err
	}
	if err = sleepCtx(ctx, claimSettle); err != nil {
		return nil, err
	}
	st, err = p.readRecent(ctx)
	if err != nil {
		return nil, err
	}
	if st.Lease != p.pending.id {
		return map[string]any{"status": "another process claimed the run"}, nil
	}
	defer func() {
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		latest, e := p.readRecent(done)
		if e == nil && latest.Lease == p.pending.id {
			latest.Lease = ""
			latest.LeaseUntil = time.Time{}
			_ = p.writeRecent(done, latest)
		}
	}()
	base, err := p.siloBase(ctx)
	if err != nil {
		return nil, err
	}
	pages := 0
	for _, a := range admins {
		id := strconv.Itoa(a.UserID)
		if !a.alert(alertRecentlyAdded).Enabled {
			delete(st.Admins, id)
			continue
		}
		prof, found, e := p.store.Get(ctx, a.ProfileID)
		if e != nil {
			return nil, e
		}
		if !found || prof.UserID != a.UserID {
			continue
		}
		allowed, e := p.adminDestinationAllowed(ctx, prof)
		if e != nil {
			return nil, e
		}
		if !allowed {
			continue
		}
		enrollment := a.RecentSince
		progress, ok := st.Admins[id]
		if enrollment.IsZero() {
			enrollment = now
			if ok {
				enrollment = progress.Enrollment
			}
		}
		if !ok || !progress.Enrollment.Equal(enrollment) {
			progress = recentProgress{Enrollment: enrollment, Since: enrollment}
		}
		if progress.Until.IsZero() {
			progress.Until = now
		}
		// Inclusive overlap covers equal timestamps and scan commits near a boundary.
		q := url.Values{"limit": {strconv.Itoa(recentPageSize)}, "added_after": {progress.Since.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)}}
		if progress.Cursor != "" {
			q.Set("cursor", progress.Cursor)
		}
		var page struct {
			Items []catalogAddition `json:"items"`
			Page  *pageInfo         `json:"page"`
		}
		if e = p.silo.get(ctx, base, cfg.MonitorAPIKey, "/api/v2/admin/catalog/items?"+q.Encode(), &page); e != nil {
			return nil, e
		}
		if page.Page != nil && page.Page.HasMore && (page.Page.NextCursor == "" || page.Page.NextCursor == progress.Cursor) {
			return nil, errors.New("catalog returned an invalid continuation cursor")
		}
		var recipients []*recipient
		enrichCtx, enrichCancel := context.WithTimeout(ctx, time.Second)
		defer enrichCancel()
		for _, item := range page.Items {
			if item.ContentID == "" || item.AddedAt.IsZero() || item.AddedAt.Before(enrollment) || item.AddedAt.After(progress.Until) || item.Type != "movie" && item.Type != "episode" {
				continue
			}
			if len(a.Filters.MediaTypes) > 0 && !slices.Contains(a.Filters.MediaTypes, item.Type) {
				continue
			}
			if len(a.Filters.Libraries) > 0 && !slices.ContainsFunc(item.LibraryIDs, func(id string) bool { return slices.Contains(a.Filters.Libraries, id) }) {
				continue
			}
			key := "recent:" + item.ContentID + ":" + item.AddedAt.UTC().Format(time.RFC3339Nano)
			r := newRecipient(a, alertRecentlyAdded, key)
			// Check before optional metadata lookups; overlapping scans should be cheap.
			if seen, e := p.handled.Seen(ctx, r.key); e != nil {
				return nil, e
			} else if seen {
				continue
			}
			item = p.enrichAddition(enrichCtx, item)
			r.vars = item.variables()
			recipients = append(recipients, r)
		}
		enrichCancel()
		ready, again, e := p.dropHandled(ctx, p.logger, recipients)
		if e != nil {
			return nil, e
		}
		if again {
			return nil, errors.New("could not load an administrator destination")
		}
		for _, retry := range p.sendAll(ctx, p.logger, ready, now, 1) {
			if retry {
				return nil, errors.New("recently-added delivery will retry")
			}
		}
		progress.Cursor = page.Page.next()
		if progress.Cursor == "" {
			progress.Since = progress.Until
			progress.Until = time.Time{}
		}
		st.Admins[id] = progress
		latest, e := p.readRecent(ctx)
		if e != nil {
			return nil, e
		}
		if latest.Lease != p.pending.id {
			return nil, errors.New("recently-added lease changed")
		}
		if e = p.writeRecent(ctx, st); e != nil {
			return nil, e
		}
		pages++
	}
	out["pages"] = pages
	return out, nil
}
func (p *Plugin) enrichAddition(ctx context.Context, item catalogAddition) catalogAddition {
	if ctx.Err() != nil || p.catalogMediaFn == nil || item.PosterURL != "" && item.Overview != "" {
		return item
	}
	lookup, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	rows, err := p.catalogMediaFn(lookup, runtimehost.ListLibraryMediaRequest{Query: item.Title, MediaTypes: []string{item.Type}, PageSize: 20})
	if err != nil || rows == nil {
		return item
	}
	for _, row := range rows.Items {
		if row.MediaID == item.ContentID {
			if item.Overview == "" {
				item.Overview = row.Overview
			}
			if item.PosterURL == "" && row.PosterURL != "" {
				if safeMessageURL(row.PosterURL) != "" {
					item.PosterURL = row.PosterURL
				} else if p.resolveImagesFn != nil {
					if urls, e := p.resolveImagesFn(lookup, []string{row.PosterURL}, "card"); e == nil {
						item.PosterURL = urls[row.PosterURL]
					}
				}
			}
			break
		}
	}
	return item
}
func (item catalogAddition) variables() map[string]string {
	year := ""
	if item.Year > 0 {
		year = strconv.Itoa(item.Year)
	}
	title := item.Title
	if item.SeriesTitle != "" {
		title = item.SeriesTitle + " · " + seasonEpisode(item.Season, item.Episode) + " · " + item.Title
	}
	quality := ""
	if len(item.Versions) > 0 {
		v := item.Versions[0]
		quality = strings.TrimSpace(v.Resolution + " · " + v.Video + " · " + v.Audio)
	}
	rating := ""
	if item.RatingTMDB != nil {
		rating = "TMDB " + strconv.FormatFloat(*item.RatingTMDB, 'f', 1, 64) + " / 10"
	} else if item.RatingIMDb != nil {
		rating = "IMDb " + strconv.FormatFloat(*item.RatingIMDb, 'f', 1, 64) + " / 10"
	}
	return map[string]string{"rating": rating, "genres": strings.Join(item.Genres, ", "), "content_rating": item.ContentRating, "title": title, "year": year, "media_type": titleCase(item.Type), "quality": quality, "overview": truncateRunes(item.Overview, 600), "content_id": item.ContentID, "poster_url": item.PosterURL, "libraries": strings.Join(item.LibraryIDs, ", ")}
}
