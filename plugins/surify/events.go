package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/imagevariant"
)

// eventNotificationCreated is published by Silo once for every delivery it
// adds to a profile's notification inbox.
const eventNotificationCreated = "notification.created"

// delivery is the notification.created payload (Silo's DeliveryRowPayload).
type delivery struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	ProfileID     string          `json:"profile_id"`
	SeriesID      *string         `json:"series_id"`
	EpisodeID     *string         `json:"episode_id"`
	SeriesTitle   string          `json:"series_title"`
	EpisodeTitle  string          `json:"episode_title"`
	SeasonNumber  *int            `json:"season_number"`
	EpisodeNumber *int            `json:"episode_number"`
	PosterPath    string          `json:"poster_path"`
	PosterURL     string          `json:"poster_url"`
	ReasonFlags   json.RawMessage `json:"reason_flags"`
	CreatedAt     time.Time       `json:"created_at"`
}

// deliveryFlags decodes reason_flags: the four reason booleans on
// episode.available, request identifiers on request.* deliveries.
type deliveryFlags struct {
	Favorite         bool   `json:"favorite"`
	Watchlist        bool   `json:"watchlist"`
	ContinueWatching bool   `json:"continue_watching"`
	NextUp           bool   `json:"next_up"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	MediaType        string `json:"media_type"`
	Reason           string `json:"reason"`
}

// has reports whether Silo gave the reason (one of the reason… constants).
func (f deliveryFlags) has(reason string) bool {
	switch reason {
	case reasonFavorite:
		return f.Favorite
	case reasonWatchlist:
		return f.Watchlist
	case reasonContinueWatching:
		return f.ContinueWatching
	case reasonNextUp:
		return f.NextUp
	}
	return false
}

// reasonLabels name the reasons in messages, in the order they are listed.
var reasonLabels = []struct{ key, label string }{
	{reasonFavorite, "Favorite"}, {reasonWatchlist, "Watchlist"},
	{reasonContinueWatching, "Continue Watching"}, {reasonNextUp, "Next Up"},
}

type eventServer struct {
	pluginv1.UnimplementedEventConsumerServer
	plugin *Plugin
}

// HandleEvent sends the notification before answering. Silo delivers each
// event at least once and retries when the call returns an error or takes
// longer than 10 s, so a retryable failure is returned as an error and
// everything else (sent, ignored, or rejected for good) is acknowledged.
func (s *eventServer) HandleEvent(ctx context.Context, req *pluginv1.HandleEventRequest) (*pluginv1.HandleEventResponse, error) {
	name := req.GetEventName()
	if (name != eventNotificationCreated && !isSessionEvent(name) && !isServerEvent(name)) || req.GetPayload() == nil {
		return &pluginv1.HandleEventResponse{}, nil
	}
	raw, err := json.Marshal(req.GetPayload().AsMap())
	if err != nil {
		return &pluginv1.HandleEventResponse{}, nil
	}
	if isSessionEvent(name) || isServerEvent(name) {
		ctx, cancel := context.WithTimeout(ctx, s.plugin.eventTimeout)
		defer cancel()
		handle := s.plugin.handlePlaybackEvent
		if isServerEvent(name) {
			handle = s.plugin.handleServerEvent
		}
		if err := handle(ctx, req.GetEventId(), name, raw); err != nil {
			return nil, err
		}
		return &pluginv1.HandleEventResponse{}, nil
	}
	var d delivery
	if err := json.Unmarshal(raw, &d); err != nil || d.ID == "" || d.ProfileID == "" {
		return &pluginv1.HandleEventResponse{}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.plugin.eventTimeout)
	defer cancel()
	if err := s.plugin.deliver(ctx, dedupeKey(req.GetEventId(), d), d); err != nil {
		return nil, err
	}
	return &pluginv1.HandleEventResponse{}, nil
}

// dedupeKey identifies an event across retries. Older hosts send no event_id
// (and never retry), so fall back to the delivery's own ID.
func dedupeKey(eventID string, d delivery) string {
	if eventID != "" {
		return eventID
	}
	return "delivery:" + d.ID
}

// deliver sends one delivery to the profile's Gotify account, if the
// profile has Gotify set up and the type turned on. It returns an error
// only when trying again later could succeed.
func (p *Plugin) deliver(ctx context.Context, key string, d delivery) error {
	log := p.logger.With("event", key, "delivery_id", d.ID, "type", d.Type, "profile_id", d.ProfileID)

	settings, found, err := p.loadSettingsForDelivery(ctx, d.ProfileID)
	if err != nil {
		log.Warn("could not load Gotify settings; asking Silo to retry", "error", err)
		return fmt.Errorf("load Gotify settings: %w", err)
	}
	if !found || !settings.Enabled || settings.ServerURL == "" {
		return nil
	}
	allowed, err := p.adminDestinationAllowed(ctx, settings)
	if err != nil {
		return fmt.Errorf("verify administrator destination: %w", err)
	}
	if !allowed {
		return nil
	}
	cfg := p.config()
	token := effectiveToken(settings, cfg)
	if token == "" {
		log.Warn("profile has no Gotify application token and the server has no shared one")
		return nil
	}

	typeKey := notificationTypeByKey(d.Type).Key
	ts := settings.typeSettings(typeKey)
	if !ts.Enabled {
		return nil
	}
	if typeKey == episodeAvailableKey {
		var flags deliveryFlags
		_ = json.Unmarshal(d.ReasonFlags, &flags)
		if !settings.wantsEpisode(flags) {
			log.Debug("episode skipped: none of its reasons is one the profile asked for")
			return nil
		}
	}

	handled, err := p.handled.Seen(ctx, key)
	if err != nil {
		log.Warn("could not read handled events; asking Silo to retry", "error", err)
		return fmt.Errorf("read handled events: %w", err)
	}
	if handled {
		log.Debug("event already handled")
		return nil
	}

	// A profile that groups season drops holds the episode in state and lets the
	// flush send it with the others. Writing to state is all that happens here,
	// so the event is acknowledged well inside its deadline.
	if window := settings.groupWindow(); window > 0 && typeKey == episodeAvailableKey && seriesKey(d) != "" {
		return p.holdEpisode(ctx, log, key, d, window)
	}

	msg := p.buildMessage(ctx, settings, ts, d)
	msg.Token = token
	msg.Priority = settings.priorityAt(msg.Priority, p.now())
	if err := p.gotify.Send(ctx, msg); err != nil {
		var perr *gotifyError
		if !errors.As(err, &perr) || perr.Retryable {
			log.Warn("Gotify send failed; asking Silo to retry", "error", err)
			return fmt.Errorf("send to Gotify: %w", err)
		}
		// Bad key, monthly limit and other 4xx answers fail again unchanged.
		log.Warn("Gotify rejected the notification; not retrying", "error", err, "http_status", perr.HTTPStatus)
	} else {
		log.Info("sent Gotify notification")
	}
	p.recordHandled(ctx, log, key)
	return nil
}

// recordHandled stores the event ID so a repeat of the event is skipped. The
// send already happened, so this gets a short budget of its own even when the
// event's deadline has run out, and a failure is only logged.
func (p *Plugin) recordHandled(ctx context.Context, log *slog.Logger, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := p.handled.Record(ctx, key); err != nil {
		log.Warn("could not record handled event; a repeat may be sent twice", "error", err)
	}
}

// recordTimeout is the budget for recording a handled event. Together with
// the event deadline it stays under Silo's 10 s per attempt.
const recordTimeout = 1500 * time.Millisecond

// loadSettingsForDelivery waits briefly for the host connection, which is
// bound just after the plugin starts. If it isn't ready in time the event
// fails and Silo retries it.
func (p *Plugin) loadSettingsForDelivery(ctx context.Context, profileID string) (*ProfileSettings, bool, error) {
	for attempt := 0; ; attempt++ {
		settings, found, err := p.store.Get(ctx, profileID)
		if !errors.Is(err, errHostUnavailable) || attempt == 8 {
			return settings, found, err
		}
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return nil, false, err
		}
	}
}

func effectiveToken(s *ProfileSettings, cfg adminConfig) string {
	if s.AppToken != "" {
		return s.AppToken
	}
	if validServerURL(cfg.DefaultServerURL) && strings.TrimRight(s.ServerURL, "/") == strings.TrimRight(cfg.DefaultServerURL, "/") {
		return cfg.DefaultAppToken
	}
	return ""
}

// deliveryVars fills the template placeholders for one delivery.
func deliveryVars(d delivery, profileName string) map[string]string {
	var flags deliveryFlags
	_ = json.Unmarshal(d.ReasonFlags, &flags)

	vars := map[string]string{
		"profile":       profileName,
		"type":          d.Type,
		"type_label":    humanizeType(d.Type),
		"series":        d.SeriesTitle,
		"episode_title": d.EpisodeTitle,
		"season":        intString(d.SeasonNumber),
		"episode":       intString(d.EpisodeNumber),
		"sxe":           seasonEpisode(d.SeasonNumber, d.EpisodeNumber),
	}

	var reasons []string
	for _, r := range reasonLabels {
		if flags.has(r.key) {
			reasons = append(reasons, r.label)
		}
	}
	vars["reasons"] = strings.Join(reasons, ", ")

	// Approved and declined requests carry their title in reason_flags; a
	// fulfilled request's catalog item title arrives as series_title.
	title := flags.Title
	if title == "" {
		title = d.SeriesTitle
	}
	vars["title"] = title
	vars["year"] = ""
	vars["title_year"] = title
	if flags.Year > 0 {
		vars["year"] = strconv.Itoa(flags.Year)
		vars["title_year"] = title + " (" + vars["year"] + ")"
	}
	switch flags.MediaType {
	case "movie":
		vars["media_type"] = "Movie"
	case "tv", "series", "show":
		vars["media_type"] = "Series"
	default:
		vars["media_type"] = flags.MediaType
	}
	vars["decline_reason"] = flags.Reason
	return vars
}

func (p *Plugin) buildMessage(ctx context.Context, s *ProfileSettings, ts TypeSettings, d delivery) gotifyMessage {
	return p.composeMessage(ctx, s, ts, d, ts.Title, ts.Message, deliveryVars(d, s.ProfileName))
}

// composeMessage renders a title and message template with vars into the push
// for a profile, adding what d gives it: the link, the poster and the time.
func (p *Plugin) composeMessage(ctx context.Context, s *ProfileSettings, ts TypeSettings, d delivery, title, message string, vars map[string]string) gotifyMessage {
	msg := gotifyMessage{
		User:      s.ServerURL,
		Title:     renderTemplate(title, vars, s.HTML),
		Message:   renderTemplate(message, vars, s.HTML),
		Device:    s.Device,
		Sound:     s.Sound,
		Priority:  s.Priority,
		HTML:      s.HTML,
		Timestamp: d.CreatedAt,
	}
	if ts.Priority != nil {
		msg.Priority = *ts.Priority
	}
	if ts.Sound != "" {
		msg.Sound = ts.Sound
	}
	if s.IncludeLink {
		msg.URL, msg.URLTitle = p.itemLink(ctx, d)
	}
	if s.AttachPoster && (d.PosterPath != "" || d.PosterURL != "") {
		pctx, cancel := context.WithTimeout(ctx, p.posterTimeout)
		msg.Attachment, msg.AttachmentType = p.poster(pctx, d)
		cancel()
	}
	return msg
}

// poster returns the delivery's poster image, or nothing if it can't be had
// within ctx. It prefers the small card variant of poster_path, which the
// host resolves, and falls back to poster_url.
func (p *Plugin) poster(ctx context.Context, d delivery) ([]byte, string) {
	var tried string
	if d.PosterPath != "" && p.resolveImagesFn != nil {
		if urls, err := p.resolveImagesFn(ctx, []string{d.PosterPath}, imagevariant.Card); err == nil {
			if u := urls[d.PosterPath]; u != "" {
				if data, contentType := p.fetchPoster(ctx, u); data != nil {
					return data, contentType
				}
				tried = u
			}
		}
	}
	if d.PosterURL != "" && d.PosterURL != tried && ctx.Err() == nil {
		return p.fetchPoster(ctx, d.PosterURL)
	}
	return nil, ""
}

// itemLink points at the same page Silo's own inbox opens: the episode when
// there is one, otherwise the series or movie.
func (p *Plugin) itemLink(ctx context.Context, d delivery) (string, string) {
	base := p.publicBaseURL(ctx)
	if base == "" {
		return "", ""
	}
	switch {
	case d.EpisodeID != nil && *d.EpisodeID != "":
		return base + "/item/" + *d.EpisodeID, "Open in Silo"
	case d.SeriesID != nil && *d.SeriesID != "":
		return base + "/item/" + *d.SeriesID, "Open in Silo"
	}
	return "", ""
}

func (p *Plugin) publicBaseURL(ctx context.Context) string {
	if override := p.config().PublicURL; override != "" {
		return override
	}
	info, err := p.host(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimRight(info.PublicBaseURL, "/")
}

// fetchPoster downloads the poster to attach it. Failures just mean no image.
func (p *Plugin) fetchPoster(ctx context.Context, posterURL string) ([]byte, string) {
	if strings.HasPrefix(posterURL, "/") {
		info, err := p.host(ctx)
		if err != nil || info.InternalBaseURL == "" {
			return nil, ""
		}
		posterURL = strings.TrimRight(info.InternalBaseURL, "/") + posterURL
	}
	if !strings.HasPrefix(posterURL, "http://") && !strings.HasPrefix(posterURL, "https://") {
		return nil, ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, posterURL, nil)
	if err != nil {
		return nil, ""
	}
	resp, err := p.fetch.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(contentType, "image/") {
		return nil, ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachment+1))
	if err != nil || len(data) > maxAttachment {
		return nil, ""
	}
	return data, contentType
}

func intString(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// recentIDs remembers the last n IDs in memory.
type recentIDs struct {
	mu    sync.Mutex
	order []string
	set   map[string]struct{}
	next  int
}

func newRecentIDs(n int) *recentIDs {
	return &recentIDs{order: make([]string, n), set: make(map[string]struct{}, n)}
}

// Add records id and reports whether it was new.
func (r *recentIDs) Add(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.set[id]; ok {
		return false
	}
	if old := r.order[r.next]; old != "" {
		delete(r.set, old)
	}
	r.order[r.next] = id
	r.set[id] = struct{}{}
	r.next = (r.next + 1) % len(r.order)
	return true
}

// Has reports whether id was added and not yet pushed out.
func (r *recentIDs) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.set[id]
	return ok
}
