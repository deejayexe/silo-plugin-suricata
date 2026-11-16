package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Admin alerts. Each session.* event goes to every admin who switched on an
// alert it triggers, whose filters, noise mode and limits let it through,
// and whom Silo still lists as an enabled admin. Alerts need the read-only
// Silo key: it's how the plugin re-checks each recipient's role, recounts
// live streams, and learns each account's devices and addresses.
//
// One event can reach several admins, and several alerts can reach one
// admin, so each is recorded as handled on its own (event ID plus account ID,
// plus the alert for watchdog alerts). A retry after a partial failure sends
// only what didn't go out.
//
// Everything runs inside the event's deadline: the Silo lookups run side by
// side within lookupBudget, and the sends get the rest.

const (
	// lookupBudget bounds the Silo lookups one event needs (role checks, the
	// stream recount, device and address seeding, the library lookup),
	// leaving the rest of the event deadline for sending.
	lookupBudget = 3 * time.Second
	// liveSessionTTL is how long a session with no stop seen still counts as
	// playing.
	liveSessionTTL = 12 * time.Hour
)

// recipient is one alert to one admin.
type recipient struct {
	admin *AdminSettings
	kind  string // the alert type sent to this admin
	key   string // handled-event ID for this alert, "" when not deduped
	// vars fills the alert's placeholders; nil means the event's own.
	vars    map[string]string
	profile *ProfileSettings
	token   string
	// poster is the image to attach, fetched once per event; nil for none.
	poster     []byte
	posterType string
}

// newRecipient builds a recipient whose handled-event ID extends baseKey.
// A playback alert is one per admin per event; other alerts add their type.
func newRecipient(a *AdminSettings, kind, baseKey string) *recipient {
	r := &recipient{admin: a, kind: kind}
	if baseKey != "" {
		r.key = baseKey + "|admin:" + strconv.Itoa(a.UserID)
		if !isPlaybackAlert(kind) {
			r.key += "|" + kind
		}
	}
	return r
}

// handlePlaybackEvent tracks one session.* event and sends the admin alerts
// it triggers. It returns an error only when trying the event again could
// help.
func (p *Plugin) handlePlaybackEvent(ctx context.Context, eventID, eventName string, raw []byte) error {
	pb, ok := parseLifecycle(eventName, raw, time.Now())
	if !ok {
		return nil
	}
	cfg := p.config()
	if cfg.MonitorAPIKey == "" {
		return nil
	}
	key := playbackDedupeKey(eventID, pb)
	log := p.logger.With("event", key, "event_name", eventName, "session_id", pb.SessionID)

	admins, err := waitForHost(ctx, func() ([]*AdminSettings, error) { return p.admins.List(ctx) })
	if err != nil {
		log.Warn("could not load admin alert settings; asking Silo to retry", "error", err)
		return fmt.Errorf("load admin alert settings: %w", err)
	}
	// With nobody opted in there's nothing to track for.
	if !slices.ContainsFunc(admins, (*AdminSettings).wantsEvents) {
		return nil
	}

	var candidates []*recipient
	// A replaced session goes on playing in a new one, which sends its own
	// start; its stop is only tracked.
	if pb.Event != eventSessionStopped || pb.EndReason != endReasonReplaced {
		for _, a := range admins {
			for _, kind := range a.sessionAlerts(pb) {
				candidates = append(candidates, newRecipient(a, kind, key))
			}
		}
	}

	ev := &watchEvent{pb: &pb, admins: admins}
	retry := false
	if len(candidates) > 0 || pb.Event == eventSessionStarted {
		var stop bool
		candidates, stop, retry = p.lookUp(ctx, log, cfg.MonitorAPIKey, key, ev, candidates)
		if stop {
			return nil
		}
	}

	candidates, again, err := p.dropHandled(ctx, log, candidates)
	if err != nil {
		log.Warn("could not read handled events; asking Silo to retry", "error", err)
		return fmt.Errorf("read handled events: %w", err)
	}
	retry = retry || again

	recipients, err := p.watch.decide(ctx, log, ev, candidates)
	if err != nil {
		log.Warn("could not read alert noise-control state; asking Silo to retry", "error", err)
		return fmt.Errorf("read alert noise-control state: %w", err)
	}
	vars := pb.vars() // after decide, which may fill in a stop's details
	for _, r := range recipients {
		if r.vars == nil {
			r.vars = vars
		}
	}
	p.attachPosters(ctx, log, recipients, pb.PosterURL)
	if p.fanOut(ctx, log, recipients, pb.At) || retry {
		return errors.New("some admin alerts couldn't be sent yet")
	}
	return nil
}

// lookUp makes the Silo calls an event needs, side by side within
// lookupBudget: each recipient's role, the live stream recount (for limit
// alerts), the account's known devices and addresses (on a start), and the
// item's libraries (for library filters). It returns the recipients that are
// still in; stop is true when Silo rejected the key, and again when a lookup
// failed for a reason a retry could fix.
func (p *Plugin) lookUp(ctx context.Context, log *slog.Logger, apiKey, eventKey string, ev *watchEvent, candidates []*recipient) (kept []*recipient, stop, again bool) {
	base, err := p.siloBase(ctx)
	if err != nil {
		if len(candidates) == 0 {
			return nil, false, false
		}
		log.Warn("Silo's internal URL isn't known yet; asking Silo to retry", "error", err)
		return nil, false, true
	}
	lctx, cancel := context.WithTimeout(ctx, lookupBudget)
	defer cancel()
	pb := ev.pb
	wants := func(match func(*recipient) bool) bool { return slices.ContainsFunc(candidates, match) }
	needCount := wants(func(r *recipient) bool { return isLimitAlert(r.kind) })
	needLibs := pb.ContentID != "" && wants(func(r *recipient) bool {
		return isPlaybackAlert(r.kind) && len(r.admin.Filters.Libraries) > 0
	})
	ip := seenIP(*pb)
	needSeen := pb.Event == eventSessionStarted && (pb.DeviceID != "" || ip != "")

	var wg sync.WaitGroup
	var roleStop, roleAgain bool
	confirmed := candidates
	if len(candidates) > 0 {
		wg.Go(func() { confirmed, roleStop, roleAgain = p.confirmAdmins(lctx, log, base, apiKey, candidates) })
	}
	var countErr, libsErr, seenErr error
	if needCount {
		wg.Go(func() {
			var rows []adminSession
			if rows, countErr = p.silo.Sessions(lctx, base, apiKey); countErr == nil {
				ev.live = make([]playback, 0, len(rows))
				for _, row := range rows {
					s := playback{SessionID: row.SessionID}
					s.fromSession(&row)
					ev.live = append(ev.live, s)
				}
			}
		})
	}
	if needLibs {
		wg.Go(func() {
			ev.libs, libsErr = p.silo.ContentLibraries(lctx, base, apiKey, pb.ContentID)
			ev.libsKnown = libsErr == nil
		})
	}
	if needSeen {
		wg.Go(func() {
			seed := func(ctx context.Context) ([]string, []string, error) {
				var devices, ips []string
				var derr, ierr error
				var sw sync.WaitGroup
				sw.Go(func() { devices, derr = p.silo.UserDevices(ctx, base, apiKey, pb.UserID) })
				sw.Go(func() { ips, ierr = p.silo.UserRemoteIPs(ctx, base, apiKey, pb.UserID) })
				sw.Wait()
				return devices, ips, cmp.Or(derr, ierr)
			}
			ev.newDevice, ev.newIP, seenErr = p.seen.check(lctx, pb.UserID, pb.DeviceID, ip, eventKey, seed)
		})
	}
	wg.Wait()
	if roleStop {
		return nil, true, false
	}
	again = roleAgain

	// failed reports whether a lookup an alert needs failed, logging it
	// once, and notes whether a retry could help.
	logged := map[string]bool{}
	failed := func(what string, err error) bool {
		if err == nil {
			return false
		}
		keyErr := errors.Is(err, errSiloKeyRejected) || errors.Is(err, errSiloKeyScope)
		if !logged[what] {
			logged[what] = true
			if keyErr {
				log.Warn("the read-only Silo key can't "+what+"; skipping the alerts that need it", "error", err)
			} else {
				log.Warn("could not "+what+"; will retry the alerts that need it", "error", err)
			}
		}
		again = again || !keyErr
		return true
	}
	for _, r := range confirmed {
		switch {
		case isLimitAlert(r.kind):
			if failed("recount live streams", countErr) {
				continue
			}
		case r.kind == alertNewDevice, r.kind == alertNewIP:
			if failed("look up the account's known devices and addresses", seenErr) {
				continue
			}
			if (r.kind == alertNewDevice && !ev.newDevice) || (r.kind == alertNewIP && !ev.newIP) {
				continue
			}
		case isPlaybackAlert(r.kind) && len(r.admin.Filters.Libraries) > 0:
			if failed("look up the item's libraries", libsErr) {
				continue
			}
		}
		kept = append(kept, r)
	}
	return kept, false, again
}

// playbackDedupeKey identifies an event across retries. Older hosts send no
// event_id (and never retry); the session and sequence number stand in.
func playbackDedupeKey(eventID string, pb playback) string {
	switch {
	case eventID != "":
		return eventID
	case pb.Seq > 0:
		return "playback:" + pb.SessionID + ":" + strconv.FormatInt(pb.Seq, 10)
	}
	return ""
}

// sessionAlerts lists the alerts pb may send this admin. Filters that need
// the event's lookups (a stop's location and method, libraries) and the
// noise mode and limits are applied later, in watchStore.decide.
func (a *AdminSettings) sessionAlerts(pb playback) []string {
	var kinds []string
	if kind, ok := a.alertFor(pb); ok {
		kinds = append(kinds, kind)
	}
	if pb.Event != eventSessionStarted {
		return kinds
	}
	on := func(kind string) bool { return a.alert(kind).Enabled }
	ignored := slices.Contains(a.Filters.IgnoreUsers, pb.UserID)
	for _, kind := range limitAlerts {
		if on(kind) && a.Limits.limit(kind) > 0 && !(kind == alertStreamsUser && ignored) {
			kinds = append(kinds, kind)
		}
	}
	if on(alertNewDevice) && !ignored && pb.DeviceID != "" {
		kinds = append(kinds, alertNewDevice)
	}
	if on(alertNewIP) && !ignored && seenIP(pb) != "" {
		kinds = append(kinds, alertNewIP)
	}
	return kinds
}

// alertFor picks the playback alert an admin gets for pb, if any. A failed
// play goes out as "stream stopped" to admins who want stops but not
// failures.
func (a *AdminSettings) alertFor(pb playback) (string, bool) {
	kind := pb.alertKind()
	if kind == "" {
		return "", false
	}
	// A stop's location and method are only known for sure once its start
	// has been looked up (watchStore.decide), which checks them then.
	f := a.Filters
	if pb.Event == eventSessionStopped {
		f.OnlyRemote, f.OnlyTranscodes = false, false
	}
	if !f.match(pb) {
		return "", false
	}
	if a.alert(kind).Enabled {
		return kind, true
	}
	if kind == alertPlaybackFailed && a.alert(alertStreamStopped).Enabled {
		return alertStreamStopped, true
	}
	return "", false
}

// match applies the filters known from the event itself. Unknown location
// or method counts as not remote and not a transcode. Libraries are checked
// in watchStore.decide, once looked up.
func (f AdminFilters) match(pb playback) bool {
	switch {
	case slices.Contains(f.IgnoreUsers, pb.UserID):
		return false
	case f.OnlyRemote && pb.Location != "remote":
		return false
	case f.OnlyTranscodes && !pb.isTranscode():
		return false
	case len(f.MediaTypes) > 0 && !slices.Contains(f.MediaTypes, pb.MediaType):
		return false
	}
	return true
}

// siloBase is Silo's internal URL, for the read-only client.
func (p *Plugin) siloBase(ctx context.Context) (string, error) {
	info, err := p.host(ctx)
	if err != nil {
		return "", err
	}
	if info.InternalBaseURL == "" {
		return "", errors.New("Silo's internal URL is not known yet")
	}
	return strings.TrimRight(info.InternalBaseURL, "/"), nil
}

// confirmAdmins keeps the candidates Silo still lists as enabled admins,
// looking each admin up once however many alerts they're due. stop is true
// when Silo rejected the key: no admin alerts go out, and that is logged once
// until the key changes. again is true when a lookup failed for a reason a
// retry could fix.
func (p *Plugin) confirmAdmins(ctx context.Context, log *slog.Logger, base, apiKey string, candidates []*recipient) (kept []*recipient, stop, again bool) {
	type lookup struct {
		user *siloUser
		err  error
	}
	var ids []int
	for _, r := range candidates {
		if !slices.Contains(ids, r.admin.UserID) {
			ids = append(ids, r.admin.UserID)
		}
	}
	results := make([]lookup, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			u, err := p.silo.CurrentUser(ctx, base, apiKey, id)
			results[i] = lookup{u, err}
		})
	}
	wg.Wait()

	ok := map[int]bool{}
	for i, id := range ids {
		res := results[i]
		switch {
		case errors.Is(res.err, errSiloKeyRejected), errors.Is(res.err, errSiloKeyScope):
			if !p.adminKeyWarned.Swap(true) {
				log.Warn("Silo refused the read-only key, so admin alerts are paused until it's fixed. Set up the key again from the admin section of the Gotify page.", "error", res.err)
			}
			return nil, true, false
		case errors.Is(res.err, errSiloUserNotFound):
			log.Info("skipping admin alerts for an account Silo no longer has", "user_id", id)
		case res.err != nil:
			log.Warn("could not check an admin's role; will retry for them", "user_id", id, "error", res.err)
			again = true
		case !res.user.isAdmin():
			log.Info("skipping admin alerts for an account that is no longer an enabled admin", "user_id", id)
		default:
			ok[id] = true
		}
	}
	p.adminKeyWarned.Store(false)
	for _, r := range candidates {
		if ok[r.admin.UserID] {
			kept = append(kept, r)
		}
	}
	return kept, false, again
}

// dropHandled removes recipients that already got this event and loads each
// remaining one's Gotify settings. again is true when a recipient's
// settings couldn't be read.
func (p *Plugin) dropHandled(ctx context.Context, log *slog.Logger, candidates []*recipient) (kept []*recipient, again bool, err error) {
	var keys []string
	for _, r := range candidates {
		if r.key != "" {
			keys = append(keys, r.key)
		}
	}
	seen, err := p.handled.SeenMany(ctx, keys)
	if err != nil {
		return nil, false, err
	}
	cfg := p.config()
	profiles := map[int]*ProfileSettings{}
	skipped := map[int]bool{}
	for _, r := range candidates {
		if r.key != "" && seen[r.key] {
			continue
		}
		id := r.admin.UserID
		if skipped[id] {
			continue
		}
		prof := profiles[id]
		if prof == nil {
			var found bool
			prof, found, err = p.loadSettingsForDelivery(ctx, r.admin.ProfileID)
			switch {
			case err != nil:
				log.Warn("could not load an admin's Gotify settings; will retry for them", "user_id", id, "error", err)
				again, skipped[id] = true, true
				continue
			case !found || !prof.AdminManaged || prof.ServerURL == "" || prof.UserID != id:
				log.Info("admin has alerts on but their profile has no Gotify settings", "user_id", id)
				skipped[id] = true
				continue
			}
			profiles[id] = prof
		}
		r.profile, r.token = prof, effectiveToken(prof, cfg)
		if r.token == "" {
			log.Warn("admin's profile has no Gotify application token and the server has no shared one", "user_id", id)
			skipped[id] = true
			continue
		}
		kept = append(kept, r)
	}
	return kept, again, nil
}

// fanOut sends every recipient's alert at once and records the ones that
// are done. It reports whether any could go through on a retry.
func (p *Plugin) fanOut(ctx context.Context, log *slog.Logger, recipients []*recipient, at time.Time) (retry bool) {
	for _, again := range p.sendAll(ctx, log, recipients, at, 1) {
		retry = retry || again
	}
	return retry
}

// sendAll sends every recipient's alert at once and records the ones that
// are done. It returns, for each recipient, whether a retry could still get
// its alert through. A scheduled task has no Silo retry to lean on, so it can
// ask for more attempts, a second apart.
func (p *Plugin) sendAll(ctx context.Context, log *slog.Logger, recipients []*recipient, at time.Time, attempts int) map[*recipient]bool {
	var handled []string
	retry := make(map[*recipient]bool, len(recipients))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range recipients {
		wg.Go(func() {
			var done, again bool
			for attempt := 1; ; attempt++ {
				done, again = p.sendAdminAlert(ctx, log, r, at)
				if !again || attempt >= attempts || sleepCtx(ctx, time.Second) != nil {
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if done && r.key != "" {
				handled = append(handled, r.key)
			}
			retry[r] = again
		})
	}
	wg.Wait()

	if len(handled) > 0 {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		if err := p.handled.RecordMany(rctx, handled); err != nil {
			log.Warn("could not record handled admin alerts; a repeat may be sent twice", "error", err)
		}
		cancel()
	}
	return retry
}

// sendAdminAlert sends one admin's alert. done means it was sent or
// rejected for good; again means a retry could get it through.
func (p *Plugin) sendAdminAlert(ctx context.Context, log *slog.Logger, r *recipient, at time.Time) (done, again bool) {
	log = log.With("user_id", r.admin.UserID, "alert", r.kind)
	msg := p.adminPush(ctx, r.admin, r.profile, r.kind, r.vars, at)
	msg.Token = r.token
	msg.Priority = r.admin.priorityAt(r.profile, msg.Priority, p.now())
	if isPlaybackAlert(r.kind) && r.admin.AttachPoster {
		msg.Attachment, msg.AttachmentType = r.poster, r.posterType
	}
	if err := p.gotify.Send(ctx, msg); err != nil {
		var perr *gotifyError
		if !errors.As(err, &perr) || perr.Retryable {
			log.Warn("Gotify send failed; asking Silo to retry", "error", err)
			return false, true
		}
		log.Warn("Gotify rejected the admin alert; not retrying", "error", err, "http_status", perr.HTTPStatus)
		return true, false
	}
	log.Info("sent admin alert")
	return true, false
}

// attachPosters downloads the event's poster once, within posterTimeout, and
// hands it to the playback alerts whose admin switched posters on. Without a
// poster URL, or when the download fails, the alerts go out without an image.
func (p *Plugin) attachPosters(ctx context.Context, log *slog.Logger, recipients []*recipient, posterURL string) {
	if posterURL == "" || !slices.ContainsFunc(recipients, func(r *recipient) bool {
		return isPlaybackAlert(r.kind) && r.admin.AttachPoster
	}) {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, p.posterTimeout)
	defer cancel()
	data, contentType := p.fetchPoster(pctx, posterURL)
	if data == nil {
		log.Info("sending admin alerts without the poster: it couldn't be downloaded in time")
		return
	}
	for _, r := range recipients {
		r.poster, r.posterType = data, contentType
	}
}

// siloLiveStreamsPath is the page in Silo's web app that lists the streams
// playing now, with Stop and Message buttons (web/src/App.tsx in silo-server,
// route /admin/activity under the admin layout; it asks an admin to sign in).
const siloLiveStreamsPath = "/admin/activity"

// hasStreamLink reports whether an alert type gets an "Open in Silo" link: the
// playback alerts and the stream-limit alerts, which are about live streams.
func hasStreamLink(kind string) bool { return isPlaybackAlert(kind) || isLimitAlert(kind) }

// adminPush is an admin alert's message for the admin's profile, with its
// "Open in Silo" link when the admin wants one. The caller adds the token and
// whatever depends on sending it (quiet hours, the poster).
func (p *Plugin) adminPush(ctx context.Context, a *AdminSettings, prof *ProfileSettings, kind string, vars map[string]string, at time.Time) gotifyMessage {
	msg := adminMessage(prof, a.alert(kind), vars, at)
	if kind == alertRecentlyAdded {
		msg.ImageURL = vars["poster_url"]
		if a.siloLink() {
			if base := p.publicBaseURL(ctx); base != "" && vars["content_id"] != "" {
				msg.URL = base + "/item/" + url.PathEscape(vars["content_id"])
				msg.URLTitle = "Open in Silo"
			}
		}
	}
	if a.siloLink() && hasStreamLink(kind) {
		if base := p.publicBaseURL(ctx); base != "" {
			msg.URL, msg.URLTitle = base+siloLiveStreamsPath, "Open in Silo"
		}
	}
	return msg
}

// now is the clock for quiet hours and scheduled tasks.
func (p *Plugin) now() time.Time { return p.taskNow() }

// adminMessage renders an alert for an admin's profile. The profile supplies
// the key, device, HTML switch and default sound and priority.
func adminMessage(prof *ProfileSettings, a AlertSettings, vars map[string]string, at time.Time) gotifyMessage {
	msg := gotifyMessage{
		User:      prof.ServerURL,
		Title:     renderTemplate(a.Title, vars, prof.HTML),
		Message:   renderTemplate(a.Message, vars, prof.HTML),
		Device:    prof.Device,
		Sound:     prof.Sound,
		Priority:  prof.Priority,
		HTML:      prof.HTML,
		Timestamp: at,
	}
	if a.Priority != nil {
		msg.Priority = *a.Priority
	}
	if a.Sound != "" {
		msg.Sound = a.Sound
	}
	return msg
}

// waitForHost retries fn briefly while the host connection, bound just after
// the plugin starts, isn't ready.
func waitForHost[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		v, err := fn()
		if !errors.Is(err, errHostUnavailable) || attempt == 8 {
			return v, err
		}
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return v, err
		}
	}
}
