package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Season-drop grouping. A profile that turns it on doesn't get one push per new
// episode: the first episode of a show is held for a window, the ones that
// follow join it, and one push goes out when the window ends ("Severance: 3 new
// episodes (S02E01 to S02E03)").
//
// Held episodes live in the "pending" instance-state key, which every Silo API
// server shares, and the event handler only writes there before it answers.
// A timer in each plugin process (and one run at startup) sends the groups
// that are due. Any server may flush any group and a restart can come at any
// moment, so a flush goes like this:
//
//  1. Claim the group with a short lease written into pending, and read it back,
//     so two servers' timers don't both send it.
//  2. Skip the send if the handled log already has the group's ID (an earlier
//     attempt sent it and was cut off before removing it).
//  3. Send. A failure that could pass later releases the lease and leaves the
//     group pending for the next tick.
//  4. Record the group's ID in the handled log, then remove the group from
//     pending. The ID is stable for the group's life (profile, show and the
//     first episode's delivery), so a flush that repeats finds it recorded.
//
// The one gap is a crash between the send and the record, which can send a
// group twice; recording before the send would instead lose the group on a
// crash or a send that then fails, and Silo's own delivery promise is "at least
// once".

const (
	pendingKey = "pending"
	// maxGroupEpisodes starts a new group for a show once one is this full.
	maxGroupEpisodes = 100
	// flushEvery is how often a process looks for groups that are due.
	flushEvery = 20 * time.Second
	// flushTickBudget bounds one pass over the due groups.
	flushTickBudget = 45 * time.Second
	// groupSendBudget bounds one group's send, like an event's deadline.
	groupSendBudget = defaultEventTimeout
	// leaseTTL is how long a claim on a group holds if its owner disappears.
	leaseTTL = 45 * time.Second
	// maxListedEpisodes caps the episode lines in a grouped message.
	maxListedEpisodes = 8
	// pendingAttempts is how many times a change to pending is retried when a
	// read-back shows another server's write replaced it.
	pendingAttempts = 3
	// claimSettle is how long a claim waits between writing its lease and
	// reading it back. Two servers that read the group before either wrote
	// both write a lease; the settle gives the slower writer time to land, so
	// both read back the same winner and only it sends.
	claimSettle = 250 * time.Millisecond
)

var errPendingConflict = errors.New("another server kept changing the pending episodes")

const episodeAvailableKey = "episode.available"

// pendingEpisode is one held episode, kept small: a busy server can hold many.
type pendingEpisode struct {
	DeliveryID string `json:"id"`
	// EventKey is the handled-log key of the event that brought it, recorded
	// once the group is sent so a late retry of that event is skipped.
	EventKey  string    `json:"event,omitempty"`
	EpisodeID string    `json:"episode_id,omitempty"`
	Title     string    `json:"title,omitempty"`
	Season    *int      `json:"season,omitempty"`
	Episode   *int      `json:"episode,omitempty"`
	Reasons   []string  `json:"reasons,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// pendingGroup is the episodes of one show held for one profile.
type pendingGroup struct {
	// ID is stable for the group's life; the handled log records it.
	ID        string `json:"id"`
	ProfileID string `json:"profile_id"`
	SeriesKey string `json:"series_key"`
	SeriesID  string `json:"series_id,omitempty"`
	Series    string `json:"series,omitempty"`
	// PosterPath and PosterURL are the first episode's.
	PosterPath string `json:"poster_path,omitempty"`
	PosterURL  string `json:"poster_url,omitempty"`
	// DueAt is when the first episode arrived plus the window then.
	FirstAt  time.Time        `json:"first_at"`
	DueAt    time.Time        `json:"due_at"`
	Episodes []pendingEpisode `json:"episodes"`
	// Lease names the process flushing the group, until LeaseUntil.
	Lease      string    `json:"lease,omitempty"`
	LeaseUntil time.Time `json:"lease_until,omitzero"`
}

func (g *pendingGroup) leased(now time.Time) bool {
	return g.Lease != "" && g.LeaseUntil.After(now)
}

func (g *pendingGroup) clone() *pendingGroup {
	c := *g
	c.Episodes = slices.Clone(g.Episodes)
	return &c
}

// pendingState is the "pending" key, oldest group first.
type pendingState struct {
	Groups []*pendingGroup `json:"groups"`
}

func (st *pendingState) entries() int { return len(st.Groups) }

// dropOldest sheds the oldest groups to make the key fit.
func (st *pendingState) dropOldest(n int) {
	st.Groups = st.Groups[min(n, len(st.Groups)):]
}

func (st *pendingState) find(id string) *pendingGroup {
	for _, g := range st.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func (st *pendingState) hasDelivery(deliveryID string) bool {
	for _, g := range st.Groups {
		if slices.ContainsFunc(g.Episodes, func(e pendingEpisode) bool { return e.DeliveryID == deliveryID }) {
			return true
		}
	}
	return false
}

// open returns the newest group an episode of the show can join: not full and
// not being flushed.
func (st *pendingState) open(profileID, seriesKey string, now time.Time) *pendingGroup {
	for i := len(st.Groups) - 1; i >= 0; i-- {
		g := st.Groups[i]
		if g.ProfileID == profileID && g.SeriesKey == seriesKey && len(g.Episodes) < maxGroupEpisodes && !g.leased(now) {
			return g
		}
	}
	return nil
}

// seriesKey says which show a delivery is about, for grouping. Empty means it
// can't be grouped.
func seriesKey(d delivery) string {
	if d.SeriesID != nil && strings.TrimSpace(*d.SeriesID) != "" {
		return "id:" + strings.TrimSpace(*d.SeriesID)
	}
	if t := strings.ToLower(strings.TrimSpace(d.SeriesTitle)); t != "" {
		return "title:" + t
	}
	return ""
}

// groupID names a group for good: its profile, show and first delivery.
func groupID(profileID, seriesKey, firstDeliveryID string) string {
	return "group:" + profileID + "|" + seriesKey + "|" + firstDeliveryID
}

// pendingStore reads and changes the "pending" key. Changes are
// read-modify-write on one key shared by every API server, with no
// transactions, so each one re-reads first and, where it matters, reads back
// afterwards to see that another server's write didn't replace it.
type pendingStore struct {
	state  stateBackend
	logger *slog.Logger
	// id names this process in leases.
	id string
	// settle is the claim settle time; tests shorten it.
	settle time.Duration
	// mu serializes this process's changes.
	mu sync.Mutex
}

func newPendingStore(state stateBackend, logger *slog.Logger) *pendingStore {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		binaryFallback(b[:])
	}
	return &pendingStore{state: state, logger: logger, id: hex.EncodeToString(b[:]), settle: claimSettle}
}

// binaryFallback fills b from the clock when the system's random source fails,
// which is unheard of; a lease only needs to differ between processes.
func binaryFallback(b []byte) {
	n := time.Now().UnixNano()
	for i := range b {
		b[i] = byte(n >> (8 * i))
	}
}

func (s *pendingStore) read(ctx context.Context) (*pendingState, error) {
	raw, found, err := s.state.Read(ctx, pendingKey)
	if err != nil {
		return nil, err
	}
	st := &pendingState{}
	if found {
		if err := json.Unmarshal(raw, st); err != nil {
			// A damaged key would otherwise block every episode for good.
			s.logger.Error("the pending episodes could not be read; starting them over", "error", err)
			st = &pendingState{}
		}
	}
	return st, nil
}

// update applies a change to the freshly read state and writes it, capped by
// encoded size. apply returns false when there is nothing to write. When verify
// is set, the key is read back and the change is redone (up to pendingAttempts
// times) unless verify holds, which catches another server's write landing
// between this one's read and write.
func (s *pendingStore) update(ctx context.Context, apply func(*pendingState) bool, verify func(*pendingState) bool) error {
	return s.updateAfter(ctx, 0, apply, verify)
}

// updateAfter is update waiting settle between each write and its read-back.
func (s *pendingStore) updateAfter(ctx context.Context, settle time.Duration, apply func(*pendingState) bool, verify func(*pendingState) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range pendingAttempts {
		st, err := s.read(ctx)
		if err != nil {
			return err
		}
		before := len(st.Groups)
		if !apply(st) {
			return nil
		}
		raw, err := encodeCapped(st)
		if err != nil {
			return err
		}
		if dropped := before - len(st.Groups); dropped > 0 {
			s.logger.Warn("the pending episodes outgrew their storage; the oldest groups were dropped", "groups_dropped", dropped)
		}
		if err := s.state.Write(ctx, pendingKey, raw); err != nil {
			return err
		}
		if verify == nil {
			return nil
		}
		if settle > 0 {
			if err := sleepCtx(ctx, settle); err != nil {
				return err
			}
		}
		back, err := s.read(ctx)
		if err != nil {
			return err
		}
		if verify(back) {
			return nil
		}
	}
	return errPendingConflict
}

// newPendingEpisode keeps what a grouped message needs of a delivery.
func newPendingEpisode(eventKey string, d delivery) pendingEpisode {
	var flags deliveryFlags
	_ = json.Unmarshal(d.ReasonFlags, &flags)
	e := pendingEpisode{
		DeliveryID: d.ID, EventKey: eventKey, Title: d.EpisodeTitle,
		Season: d.SeasonNumber, Episode: d.EpisodeNumber, CreatedAt: d.CreatedAt,
	}
	if d.EpisodeID != nil {
		e.EpisodeID = *d.EpisodeID
	}
	for _, r := range []struct {
		on  bool
		key string
	}{{flags.Favorite, reasonFavorite}, {flags.Watchlist, reasonWatchlist}, {flags.ContinueWatching, reasonContinueWatching}, {flags.NextUp, reasonNextUp}} {
		if r.on {
			e.Reasons = append(e.Reasons, r.key)
		}
	}
	return e
}

// holdEpisode adds a new episode to its show's pending group, starting one when
// there is none. It returns once the write is in state, so the event can be
// acknowledged; sending is the flush's job. Adding the same delivery again
// (Silo retrying an event) changes nothing.
func (p *Plugin) holdEpisode(ctx context.Context, log *slog.Logger, eventKey string, d delivery, window time.Duration) error {
	now := p.now()
	key := seriesKey(d)
	ep := newPendingEpisode(eventKey, d)
	err := p.pending.update(ctx, func(st *pendingState) bool {
		if st.hasDelivery(d.ID) {
			return false
		}
		g := st.open(d.ProfileID, key, now)
		if g == nil {
			g = &pendingGroup{
				ID: groupID(d.ProfileID, key, d.ID), ProfileID: d.ProfileID, SeriesKey: key,
				Series: d.SeriesTitle, PosterPath: d.PosterPath, PosterURL: d.PosterURL,
				FirstAt: now, DueAt: now.Add(window),
			}
			if d.SeriesID != nil {
				g.SeriesID = *d.SeriesID
			}
			st.Groups = append(st.Groups, g)
		}
		g.Episodes = append(g.Episodes, ep)
		return true
	}, func(st *pendingState) bool { return st.hasDelivery(d.ID) })
	if err != nil {
		log.Warn("could not hold the episode for grouping; asking Silo to retry", "error", err)
		return fmt.Errorf("hold episode for grouping: %w", err)
	}
	log.Info("holding the episode for grouping", "window", window.String())
	return nil
}

// runFlusher sends due groups once at startup and then on a timer, until ctx
// ends. A pass that finds the host connection not ready yet just waits for the
// next tick.
func (p *Plugin) runFlusher(ctx context.Context) {
	p.flushTick(ctx)
	t := time.NewTicker(p.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.flushTick(ctx)
		}
	}
}

func (p *Plugin) flushTick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, flushTickBudget)
	defer cancel()
	if _, err := p.flushPending(ctx); err != nil && !errors.Is(err, errHostUnavailable) {
		p.logger.Warn("could not flush the pending episode groups; trying again next time", "error", err)
	}
}

// flushPending sends every group that is due and not claimed by another
// process, and reports how many groups it finished (sent, or dropped because
// the profile no longer wants them). A group that fails in a way that could
// pass later stays pending.
func (p *Plugin) flushPending(ctx context.Context) (int, error) {
	if !p.flushMu.TryLock() {
		return 0, nil // a pass in this process is still running
	}
	defer p.flushMu.Unlock()

	st, err := p.pending.read(ctx)
	if err != nil {
		return 0, err
	}
	now := p.now()
	var due []string
	for _, g := range st.Groups {
		if !g.DueAt.After(now) && !g.leased(now) {
			due = append(due, g.ID)
		}
	}
	done := 0
	var firstErr error
	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		finished, err := p.flushGroup(ctx, id)
		if finished {
			done++
		}
		firstErr = cmp.Or(firstErr, err)
		if errors.Is(err, errHostUnavailable) {
			break
		}
	}
	return done, firstErr
}

// flushGroup sends one group. finished says the group left pending for good.
func (p *Plugin) flushGroup(ctx context.Context, id string) (finished bool, err error) {
	log := p.logger.With("group", id)
	now := p.now()

	var claimed *pendingGroup
	err = p.pending.updateAfter(ctx, p.pending.settle, func(st *pendingState) bool {
		claimed = nil
		g := st.find(id)
		if g == nil || g.leased(now) {
			return false
		}
		g.Lease, g.LeaseUntil = p.pending.id, now.Add(leaseTTL)
		claimed = g.clone()
		return true
	}, func(st *pendingState) bool {
		g := st.find(id)
		return g != nil && g.Lease == p.pending.id
	})
	if err != nil {
		// Another server won the claim, or state failed; either way this pass
		// leaves the group alone.
		return false, err
	}
	if claimed == nil {
		return false, nil // sent by another server meanwhile, or claimed by one
	}

	release := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		_ = p.pending.update(rctx, func(st *pendingState) bool {
			g := st.find(id)
			if g == nil || g.Lease != p.pending.id {
				return false
			}
			g.Lease, g.LeaseUntil = "", time.Time{}
			return true
		}, nil)
	}

	seen, err := p.handled.Seen(ctx, id)
	if err != nil {
		release()
		return false, fmt.Errorf("read handled events: %w", err)
	}
	if seen {
		log.Info("episode group was already sent; removing it from pending")
		return p.finishGroup(ctx, log, claimed), nil
	}

	settings, found, err := p.store.Get(ctx, claimed.ProfileID)
	if err != nil {
		release()
		return false, fmt.Errorf("load Gotify settings: %w", err)
	}
	ts := TypeSettings{}
	if found {
		ts = settings.typeSettings(episodeAvailableKey)
	}
	var token string
	if found {
		token = effectiveToken(settings, p.config())
	}
	if !found || !settings.Enabled || settings.ServerURL == "" || token == "" || !ts.Enabled {
		log.Info("dropping an episode group: the profile no longer sends these to Gotify")
		return p.finishGroup(ctx, log, claimed), nil
	}

	allowed, err := p.adminDestinationAllowed(ctx, settings)
	if err != nil {
		release()
		return false, fmt.Errorf("verify administrator destination: %w", err)
	}
	if !allowed {
		return p.finishGroup(ctx, log, claimed), nil
	}
	sctx, cancel := context.WithTimeout(ctx, groupSendBudget)
	msg := p.groupMessage(sctx, settings, ts, claimed)
	msg.Token = token
	msg.Priority = settings.priorityAt(msg.Priority, p.now())
	err = p.gotify.Send(sctx, msg)
	cancel()
	if err != nil {
		var perr *gotifyError
		if !errors.As(err, &perr) || perr.Retryable {
			log.Warn("Gotify send failed; the group stays pending for the next pass", "error", err)
			release()
			return false, fmt.Errorf("send to Gotify: %w", err)
		}
		log.Warn("Gotify rejected the grouped notification; not retrying", "error", err, "http_status", perr.HTTPStatus)
	} else {
		log.Info("sent grouped Gotify notification", "episodes", len(claimed.Episodes))
	}
	return p.finishGroup(ctx, log, claimed), nil
}

// finishGroup records a sent (or abandoned) group in the handled log, then
// removes the episodes it covered from pending. The record comes first so a
// flush that repeats after a failure here skips the send. Episodes that joined
// the group after it was claimed (a lost-update race between servers) stay
// pending as a group of their own.
func (p *Plugin) finishGroup(ctx context.Context, log *slog.Logger, g *pendingGroup) bool {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*recordTimeout)
	defer cancel()
	p.recordHandled(rctx, log, g.ID)
	// The events that brought the episodes are done too, so a late retry of one
	// isn't held again. Best effort: the group's own ID is what stops a resend.
	var keys []string
	for _, e := range g.Episodes {
		if e.EventKey != "" {
			keys = append(keys, e.EventKey)
		}
	}
	if len(keys) > 0 {
		if err := p.handled.RecordMany(rctx, keys); err != nil {
			log.Warn("could not record the grouped events as handled", "error", err)
		}
	}

	sent := map[string]bool{}
	for _, e := range g.Episodes {
		sent[e.DeliveryID] = true
	}
	err := p.pending.update(rctx, func(st *pendingState) bool {
		cur := st.find(g.ID)
		if cur == nil {
			return false
		}
		cur.Episodes = slices.DeleteFunc(slices.Clone(cur.Episodes), func(e pendingEpisode) bool { return sent[e.DeliveryID] })
		if len(cur.Episodes) == 0 {
			st.Groups = slices.DeleteFunc(st.Groups, func(x *pendingGroup) bool { return x.ID == g.ID })
			return true
		}
		cur.ID = groupID(cur.ProfileID, cur.SeriesKey, cur.Episodes[0].DeliveryID)
		cur.Lease, cur.LeaseUntil = "", time.Time{}
		return true
	}, func(st *pendingState) bool { return st.find(g.ID) == nil })
	if err != nil {
		// The group is recorded, so the next pass removes it without sending.
		log.Warn("could not remove the group from pending; it will be cleared on the next pass", "error", err)
		return false
	}
	return true
}

// episodeDelivery turns a held episode back into a delivery.
func episodeDelivery(profileID string, g *pendingGroup, e pendingEpisode) delivery {
	d := delivery{
		ID: e.DeliveryID, Type: episodeAvailableKey, ProfileID: profileID,
		SeriesTitle: g.Series, EpisodeTitle: e.Title, SeasonNumber: e.Season, EpisodeNumber: e.Episode,
		PosterPath: g.PosterPath, PosterURL: g.PosterURL, CreatedAt: e.CreatedAt,
	}
	if g.SeriesID != "" {
		id := g.SeriesID
		d.SeriesID = &id
	}
	if e.EpisodeID != "" {
		id := e.EpisodeID
		d.EpisodeID = &id
	}
	flags := map[string]bool{}
	for _, r := range e.Reasons {
		flags[r] = true
	}
	d.ReasonFlags, _ = json.Marshal(flags)
	return d
}

// sortedEpisodes orders a group's episodes by season and episode number.
func sortedEpisodes(g *pendingGroup) []pendingEpisode {
	eps := slices.Clone(g.Episodes)
	num := func(v *int) int {
		if v == nil {
			return -1
		}
		return *v
	}
	slices.SortStableFunc(eps, func(a, b pendingEpisode) int {
		return cmp.Or(cmp.Compare(num(a.Season), num(b.Season)), cmp.Compare(num(a.Episode), num(b.Episode)),
			a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.DeliveryID, b.DeliveryID))
	})
	return eps
}

// groupMessage is the push for a group: the ordinary new-episode message when
// only one episode was held, otherwise the merged one.
func (p *Plugin) groupMessage(ctx context.Context, s *ProfileSettings, ts TypeSettings, g *pendingGroup) gotifyMessage {
	eps := sortedEpisodes(g)
	if len(eps) == 1 {
		return p.buildMessage(ctx, s, ts, episodeDelivery(g.ProfileID, g, eps[0]))
	}

	// The merged push is about the show: it links to the show, wears the first
	// episode's poster and carries the time the first one arrived.
	rep := episodeDelivery(g.ProfileID, g, eps[0])
	rep.EpisodeID = nil
	rep.CreatedAt = time.Time{}
	for _, e := range eps {
		if !e.CreatedAt.IsZero() && (rep.CreatedAt.IsZero() || e.CreatedAt.Before(rep.CreatedAt)) {
			rep.CreatedAt = e.CreatedAt
		}
	}
	title, message := ts.groupWording(notificationTypeByKey(episodeAvailableKey))
	return p.composeMessage(ctx, s, ts, rep, title, message, groupVars(g, eps, s.ProfileName))
}

// groupVars fills the placeholders of a merged notification.
func groupVars(g *pendingGroup, eps []pendingEpisode, profileName string) map[string]string {
	label := func(e pendingEpisode) string { return seasonEpisode(e.Season, e.Episode) }
	first, last := "", ""
	for _, e := range eps {
		if l := label(e); l != "" {
			first = l
			break
		}
	}
	for i := len(eps) - 1; i >= 0; i-- {
		if l := label(eps[i]); l != "" {
			last = l
			break
		}
	}
	rng := ""
	switch {
	case first != "" && first != last:
		rng = first + " to " + last
	case first != "":
		rng = first
	}

	var lines []string
	for i, e := range eps {
		if i == maxListedEpisodes {
			lines = append(lines, "…and "+strconv.Itoa(len(eps)-maxListedEpisodes)+" more")
			break
		}
		line := strings.TrimSpace(label(e) + " · " + e.Title)
		line = strings.Trim(line, " ·")
		if line == "" {
			line = "New episode"
		}
		lines = append(lines, line)
	}

	have := map[string]bool{}
	for _, e := range eps {
		for _, r := range e.Reasons {
			have[r] = true
		}
	}
	var reasons []string
	for _, r := range reasonLabels {
		if have[r.key] {
			reasons = append(reasons, r.label)
		}
	}

	return map[string]string{
		"profile": profileName, "type": episodeAvailableKey, "type_label": notificationTypeByKey(episodeAvailableKey).Label,
		"series": g.Series, "count": strconv.Itoa(len(eps)), "range": rng, "first": first, "last": last,
		"episodes": strings.Join(lines, "\n"), "reasons": strings.Join(reasons, ", "),
	}
}
