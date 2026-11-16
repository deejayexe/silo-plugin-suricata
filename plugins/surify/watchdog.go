package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

// Watchdog alerts and the state behind them.
//
// The "sessions" key tracks every session the plugin has heard of: who is
// playing what, from where, how and at what bitrate. Noise control reads it
// (binges, and a stop's location and method, which history entries lack), and
// the limit alerts count it. Silo's live session list is the truth: each
// session.started recounts from GET /admin/sessions before checking limits,
// so a missed stop can't leave a phantom stream behind.
//
// The "throttle" key holds cooldowns and the limit marks that give limit
// alerts their hysteresis: a mark is set when an admin is told a limit was
// crossed and cleared once the count is back at the limit or under, so they
// hear about it once per crossing.
//
// "seen/00".."seen/31" hold each account's known devices and remote
// addresses, for the new device and new IP alerts.
//
// Every key is shared by all API servers and written whole. Two servers
// handling events at once can overwrite each other's change, which only costs
// an extra or a missed alert. Each key is capped by its encoded size: the
// oldest entries are dropped until it fits the host's value limit.

const (
	sessionsKey = "sessions"
	throttleKey = "throttle"
	seenBuckets = 32
	// maxSeenPerUser is how many devices and how many addresses are kept
	// per account.
	maxSeenPerUser = 50
	// freshEvent is how old a start can be and still count its own session
	// when Silo's list doesn't show it yet.
	freshEvent = 2 * time.Minute
	// overMarkTTL drops a limit mark nothing has cleared, so a server that
	// stays over the limit is reported again after a week.
	overMarkTTL = 7 * 24 * time.Hour
)

// maxStateValue is the host's limit on one instance-state value.
const maxStateValue = runtimehost.MaxInstanceStateValueBytes

var errStateTooLarge = errors.New("state value doesn't fit the host's size limit")

// cappable is a state value that can shed its oldest entries.
type cappable interface {
	entries() int
	dropOldest(n int)
}

// encodeCapped encodes v, dropping its oldest entries until the encoding
// fits maxStateValue.
func encodeCapped(v cappable) ([]byte, error) {
	for range 64 {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if len(raw) <= maxStateValue {
			return raw, nil
		}
		n := v.entries()
		if n == 0 {
			break
		}
		// Drop about the share that's over, plus a little, so this takes a
		// few passes rather than one per entry.
		over := float64(len(raw)-maxStateValue) / float64(len(raw))
		v.dropOldest(max(1, int(over*float64(n))+n/50))
	}
	return nil, errStateTooLarge
}

// oldestFirst returns m's keys sorted by when(value), oldest first.
func oldestFirst[V any](m map[string]V, when func(V) time.Time) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(when(m[a]).Compare(when(m[b])), strings.Compare(a, b))
	})
	return keys
}

// --- Session tracker ---

// trackedSession is what the plugin knows about one playback session.
type trackedSession struct {
	UserID string `json:"user_id"`
	Show   string `json:"show"`
	// Started is set once the session's start event was seen.
	Started bool      `json:"started,omitempty"`
	Start   time.Time `json:"start,omitzero"`
	// End is set by the stop event, or when Silo's live list no longer has
	// the session.
	End time.Time `json:"end,omitzero"`
	// Location and Method come from Silo's live session row, so a stop
	// (whose history entry lacks them) can still be filtered.
	Location string `json:"location,omitempty"`
	Method   string `json:"method,omitempty"`
	// Device is the client label ("Living Room TV"), also missing from
	// history entries.
	Device string `json:"device,omitempty"`
	Kbps   int64  `json:"kbps,omitempty"`
	// Poster is the item's poster URL, kept so a stop (whose history entry has
	// none) can still attach it.
	Poster string `json:"poster,omitempty"`
}

// active reports whether the session was playing at, or stopped less than
// window before, the given time.
func (m *trackedSession) active(at time.Time, window time.Duration) bool {
	start := m.Start
	if start.IsZero() {
		start = m.End
	}
	if start.After(at) {
		return false // it began after this event, which arrived late
	}
	if m.End.IsZero() {
		return at.Sub(start) < liveSessionTTL
	}
	return at.Sub(m.End) < window
}

// live reports whether the session counts as playing now.
func (m *trackedSession) live(now time.Time) bool {
	return m.End.IsZero() && now.Sub(m.Start) < liveSessionTTL
}

func (m *trackedSession) lastSeen() time.Time {
	if m.End.After(m.Start) {
		return m.End
	}
	return m.Start
}

// fromRow copies what Silo's live row says about the stream.
func (m *trackedSession) fromRow(pb playback) {
	m.Location, m.Method, m.Device, m.Kbps = pb.Location, pb.Method, pb.ClientLabel, pb.BitrateKbps
	if pb.PosterURL != "" {
		m.Poster = pb.PosterURL
	}
}

// sessionTable is the "sessions" key, by session ID.
type sessionTable map[string]*trackedSession

func (t sessionTable) entries() int { return len(t) }

// dropOldest drops stopped sessions first, oldest stop first, then live
// ones by start.
func (t sessionTable) dropOldest(n int) {
	keys := oldestFirst(t, func(m *trackedSession) time.Time {
		if m.End.IsZero() {
			// Sort every live session after every stopped one.
			return m.Start.Add(100 * 365 * 24 * time.Hour)
		}
		return m.End
	})
	for _, k := range keys[:min(n, len(keys))] {
		delete(t, k)
	}
}

// note records pb. Every change is idempotent, so a retried or repeated
// event leaves the same state, and a stop that arrived before its start
// keeps the session stopped.
func (t sessionTable) note(pb playback) {
	m := t[pb.SessionID]
	if m == nil {
		m = &trackedSession{UserID: pb.UserID, Show: pb.showKey()}
		t[pb.SessionID] = m
	}
	switch pb.Event {
	case eventSessionStarted:
		m.Started = true
		m.Start = pb.At
	case eventSessionStopped:
		m.End = pb.At
	default:
		if m.Start.IsZero() {
			m.Start = cmp.Or(pb.StartedAt, pb.At)
		}
	}
	if pb.FromRow {
		m.fromRow(pb)
	}
}

// prune drops sessions too old to matter: stopped ones once no noise window
// can reach them, live ones never seen stopping after liveSessionTTL.
func (t sessionTable) prune(now time.Time) {
	const keep = maxNoiseWindow * time.Minute
	for id, m := range t {
		stale := now.Sub(m.Start) > liveSessionTTL
		if !m.End.IsZero() {
			stale = now.Sub(m.End) > keep
		}
		if stale {
			delete(t, id)
		}
	}
}

// reconcile brings the table in line with Silo's live list: sessions it
// doesn't list have ended, and sessions it lists are recorded, with fresh
// stream details. A start that Silo doesn't list yet keeps its own session
// if the event is fresh.
func (t sessionTable) reconcile(live []playback, own *playback, now time.Time) {
	listed := map[string]bool{}
	for _, s := range live {
		listed[s.SessionID] = true
		m := t[s.SessionID]
		if m == nil {
			m = &trackedSession{UserID: s.UserID, Show: s.showKey(), Start: cmp.Or(s.StartedAt, now)}
			t[s.SessionID] = m
		}
		m.fromRow(s)
	}
	for id, m := range t {
		if !m.End.IsZero() || listed[id] {
			continue
		}
		if own != nil && id == own.SessionID && now.Sub(own.At) < freshEvent {
			continue
		}
		m.End = now
	}
}

// --- Noise and limit marks ---

// throttleState is the "throttle" key.
type throttleState struct {
	// Cooldowns is the last alert of each type about each user to each
	// admin, for the cooldown noise mode.
	Cooldowns map[string]cooldownMark `json:"cooldowns"`
	// Over marks the limits each admin was told about that haven't dropped
	// back since: "<admin>|<alert>|<user or empty>".
	Over map[string]cooldownMark `json:"over"`
}

// cooldownMark is when an alert was sent and the handled-event ID that sent
// it, so a retry of the same event isn't held back by its own mark.
type cooldownMark struct {
	At    time.Time `json:"at"`
	Event string    `json:"event,omitempty"`
}

func (st *throttleState) entries() int { return len(st.Cooldowns) + len(st.Over) }

// dropOldest drops cooldowns before limit marks, which matter more: losing
// one repeats a limit alert.
func (st *throttleState) dropOldest(n int) {
	at := func(c cooldownMark) time.Time { return c.At }
	for _, k := range oldestFirst(st.Cooldowns, at) {
		if n == 0 {
			return
		}
		delete(st.Cooldowns, k)
		n--
	}
	for _, k := range oldestFirst(st.Over, at) {
		if n == 0 {
			return
		}
		delete(st.Over, k)
		n--
	}
}

func (st *throttleState) prune(now time.Time) {
	const keep = maxNoiseWindow * time.Minute
	for k, c := range st.Cooldowns {
		if now.Sub(c.At) > keep {
			delete(st.Cooldowns, k)
		}
	}
	for k, c := range st.Over {
		if now.Sub(c.At) > overMarkTTL {
			delete(st.Over, k)
		}
	}
}

func cooldownKey(adminID int, userID, kind string) string {
	return strconv.Itoa(adminID) + "|" + userID + "|" + kind
}

func overPrefix(adminID int, kind string) string {
	return strconv.Itoa(adminID) + "|" + kind + "|"
}

// limitAlerts are the alerts with hysteresis.
var limitAlerts = []string{alertStreamsUser, alertStreamsServer, alertBandwidth}

func isLimitAlert(kind string) bool { return slices.Contains(limitAlerts, kind) }

// watchStore owns the "sessions" and "throttle" keys.
type watchStore struct {
	state stateBackend
	now   func() time.Time
	// mu makes this process's read-decide-write of the keys atomic.
	mu sync.Mutex
}

func newWatchStore(state stateBackend) *watchStore {
	return &watchStore{state: state, now: time.Now}
}

func (w *watchStore) readSessions(ctx context.Context) (sessionTable, []byte, error) {
	raw, found, err := w.state.Read(ctx, sessionsKey)
	if err != nil {
		return nil, nil, err
	}
	t := sessionTable{}
	if found && json.Unmarshal(raw, &t) != nil {
		t = sessionTable{} // damaged state only costs noise control
	}
	for id, m := range t {
		if m == nil {
			delete(t, id)
		}
	}
	return t, raw, nil
}

func (w *watchStore) readThrottle(ctx context.Context) (*throttleState, []byte, error) {
	raw, found, err := w.state.Read(ctx, throttleKey)
	if err != nil {
		return nil, nil, err
	}
	st := &throttleState{}
	if found && json.Unmarshal(raw, st) != nil {
		st = &throttleState{}
	}
	if st.Cooldowns == nil {
		st.Cooldowns = map[string]cooldownMark{}
	}
	if st.Over == nil {
		st.Over = map[string]cooldownMark{}
	}
	return st, raw, nil
}

// write stores a capped value under key when it changed.
func (w *watchStore) write(ctx context.Context, key string, v cappable, old []byte) error {
	raw, err := encodeCapped(v)
	if err != nil || string(raw) == string(old) {
		return err
	}
	return w.state.Write(ctx, key, raw)
}

// watchEvent is one session event with what the lookups found.
type watchEvent struct {
	pb     *playback
	admins []*AdminSettings
	// live is Silo's live session list; nil when it wasn't recounted.
	live []playback
	// libs are the libraries the item is in; libsKnown is false when they
	// weren't looked up.
	libs      []string
	libsKnown bool
	// newDevice and newIP say what this event's account hadn't used before.
	newDevice, newIP bool
}

// decide records the event, applies each recipient's filters, noise mode
// and limits, and returns the recipients to send to. A stop is first filled
// in with the location, method and device its start recorded.
func (w *watchStore) decide(ctx context.Context, log *slog.Logger, ev *watchEvent, candidates []*recipient) ([]*recipient, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	sessions, rawSessions, err := w.readSessions(ctx)
	if err != nil {
		return nil, err
	}
	th, rawThrottle, err := w.readThrottle(ctx)
	if err != nil {
		return nil, err
	}
	now := w.now()
	pb := ev.pb

	// Silo doesn't deliver events in order, so a pause or resume can arrive
	// after its session's stop. One from no later than the stop is stale.
	stale := false
	if m := sessions[pb.SessionID]; m != nil && !m.End.IsZero() &&
		(pb.Event == eventSessionPaused || pb.Event == eventSessionResumed) && !pb.At.After(m.End) {
		stale = true
	}

	if m := sessions[pb.SessionID]; m != nil && pb.Event == eventSessionStopped {
		if m.Location != "" {
			pb.Location = m.Location
		}
		if pb.Method == "" {
			pb.Method = m.Method
		}
		if pb.ClientLabel == "" {
			pb.ClientLabel = m.Device
		}
		if pb.PosterURL == "" {
			pb.PosterURL = m.Poster
		}
	}
	sessions.note(*pb)
	counts := countStreams(sessions, ev, now)
	th.clearLimits(ev.admins, counts)

	st := &noiseState{sessions: sessions, throttle: th}
	var pbVars map[string]string
	var out []*recipient
	for _, r := range candidates {
		switch {
		case isLimitAlert(r.kind):
			if counts == nil || !counts.recounted {
				continue
			}
			subject := ""
			if r.kind == alertStreamsUser {
				subject = pb.UserID
			}
			if !counts.over(r.admin, r.kind, subject) || !th.markOver(overPrefix(r.admin.UserID, r.kind)+subject, pb.At, r.key) {
				continue
			}
			if pbVars == nil {
				pbVars = pb.vars()
			}
			r.vars = counts.vars(r.admin, r.kind, subject, pbVars)
		case isPlaybackAlert(r.kind):
			if stale {
				continue
			}
			if pb.Event == eventSessionStopped && !r.admin.Filters.match(*pb) {
				continue
			}
			if libs := r.admin.Filters.Libraries; len(libs) > 0 && !slices.ContainsFunc(ev.libs, func(id string) bool { return slices.Contains(libs, id) }) {
				continue
			}
			if st.suppressed(*pb, r) {
				continue
			}
			if at, _ := adminAlertTypeByKey(r.kind); at.NoiseControlled && r.admin.Noise.Mode == noiseCooldown {
				ck := cooldownKey(r.admin.UserID, pb.UserID, r.kind)
				if prev, ok := th.Cooldowns[ck]; !ok || !pb.At.Before(prev.At) {
					th.Cooldowns[ck] = cooldownMark{At: pb.At, Event: r.key}
				}
			}
		}
		out = append(out, r)
	}

	sessions.prune(now)
	th.prune(now)
	// Sending still beats dropping the alerts; the state just won't
	// remember this event.
	if err := w.write(ctx, sessionsKey, sessions, rawSessions); err != nil {
		log.Warn("could not save the live session tracker", "error", err)
	}
	if err := w.write(ctx, throttleKey, th, rawThrottle); err != nil {
		log.Warn("could not save alert noise-control state", "error", err)
	}
	return out, nil
}

// markOver sets a limit mark and reports whether the admin should be told:
// yes for a new mark, or for a retry of the event that set it.
func (st *throttleState) markOver(key string, at time.Time, event string) bool {
	if prev, ok := st.Over[key]; ok {
		return event != "" && prev.Event == event
	}
	st.Over[key] = cooldownMark{At: at, Event: event}
	return true
}

// clearLimits drops the limit marks of admins who no longer have that alert
// on, and, when the streams were counted, of limits back at or under.
func (st *throttleState) clearLimits(admins []*AdminSettings, counts *streamCounts) {
	byID := map[string]*AdminSettings{}
	for _, a := range admins {
		byID[strconv.Itoa(a.UserID)] = a
	}
	for k := range st.Over {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			delete(st.Over, k)
			continue
		}
		a, kind, subject := byID[parts[0]], parts[1], parts[2]
		switch {
		case a == nil || !a.alert(kind).Enabled || a.Limits.limit(kind) <= 0:
			delete(st.Over, k)
		case counts != nil && !counts.over(a, kind, subject):
			delete(st.Over, k)
		}
	}
}

// noiseState is what noise control reads.
type noiseState struct {
	sessions sessionTable
	throttle *throttleState
}

// suppressed reports whether r's noise mode holds back pb.
func (st *noiseState) suppressed(pb playback, r *recipient) bool {
	t, _ := adminAlertTypeByKey(r.kind)
	if !t.NoiseControlled {
		return false
	}
	window := r.admin.window()
	switch r.admin.Noise.Mode {
	case noiseCooldown:
		prev, ok := st.throttle.Cooldowns[cooldownKey(r.admin.UserID, pb.UserID, r.kind)]
		if !ok || (r.key != "" && prev.Event == r.key) {
			return false
		}
		gap := pb.At.Sub(prev.At)
		return gap < window && gap > -window
	case noiseBinge:
		return st.inBinge(pb, window)
	}
	return false
}

// inBinge reports whether pb continues a binge: the same user played the
// same show in another session that is still playing or stopped less than
// window ago, or this is a later event of a session whose start was seen.
// The session's own start is left out, so a retry of a start decides the
// same way as the first try.
func (st *noiseState) inBinge(pb playback, window time.Duration) bool {
	show := pb.showKey()
	for id, m := range st.sessions {
		if m.UserID != pb.UserID || m.Show != show {
			continue
		}
		if id == pb.SessionID {
			if pb.Event != eventSessionStarted && m.Started {
				return true
			}
			continue
		}
		if m.active(pb.At, window) {
			return true
		}
	}
	return false
}

// --- Stream counts ---

// streamCounts is the set of streams playing now, for the limit alerts.
type streamCounts struct {
	// recounted is set when the streams came from Silo's live list (on a
	// start). Otherwise they come from the plugin's own tracker (on a stop)
	// and are only used to clear limit marks.
	recounted bool
	streams   []playback
}

// countStreams counts the streams playing after ev: Silo's list when it was
// recounted, the tracker on a stop, nothing otherwise.
func countStreams(sessions sessionTable, ev *watchEvent, now time.Time) *streamCounts {
	pb := ev.pb
	switch {
	case ev.live != nil:
		sessions.reconcile(ev.live, pb, now)
		c := &streamCounts{recounted: true, streams: slices.Clone(ev.live)}
		if !slices.ContainsFunc(c.streams, func(s playback) bool { return s.SessionID == pb.SessionID }) {
			if m := sessions[pb.SessionID]; pb.Event == eventSessionStarted && m != nil && m.End.IsZero() && now.Sub(pb.At) < freshEvent {
				c.streams = append(c.streams, *pb)
			}
		}
		return c
	case pb.Event == eventSessionStopped:
		c := &streamCounts{}
		for id, m := range sessions {
			if m.live(now) {
				c.streams = append(c.streams, playback{SessionID: id, UserID: m.UserID, Location: m.Location, BitrateKbps: m.Kbps})
			}
		}
		return c
	}
	return nil
}

// counted returns the streams a limit alert counts.
func (c *streamCounts) counted(a *AdminSettings, kind, subject string) []playback {
	var out []playback
	for _, s := range c.streams {
		switch {
		case kind == alertStreamsUser && s.UserID != subject:
		case kind == alertBandwidth && a.Limits.BandwidthRemoteOnly && s.Location != "remote":
		default:
			out = append(out, s)
		}
	}
	return out
}

func totalKbps(streams []playback) int64 {
	var sum int64
	for _, s := range streams {
		sum += max(s.BitrateKbps, 0)
	}
	return sum
}

// over reports whether the admin's limit for kind is crossed.
func (c *streamCounts) over(a *AdminSettings, kind, subject string) bool {
	limit := a.Limits.limit(kind)
	if limit <= 0 {
		return false
	}
	streams := c.counted(a, kind, subject)
	if kind == alertBandwidth {
		return totalKbps(streams) > int64(limit)*1000
	}
	return len(streams) > limit
}

// maxStreamLines is how many streams a limit alert lists.
const maxStreamLines = 10

// vars fills a limit alert's placeholders, on top of the starting stream's.
func (c *streamCounts) vars(a *AdminSettings, kind, subject string, pbVars map[string]string) map[string]string {
	streams := c.counted(a, kind, subject)
	v := maps.Clone(pbVars)
	v["count"] = strconv.Itoa(len(streams))
	v["limit"] = strconv.Itoa(a.Limits.limit(kind))
	if kind == alertBandwidth {
		v["limit"] += " Mbps"
		v["total"] = fmt.Sprintf("%.1f Mbps", float64(totalKbps(streams))/1000)
		slices.SortStableFunc(streams, func(x, y playback) int { return cmp.Compare(y.BitrateKbps, x.BitrateKbps) })
	} else {
		slices.SortStableFunc(streams, func(x, y playback) int { return x.StartedAt.Compare(y.StartedAt) })
	}
	var lines []string
	for i, s := range streams {
		if i == maxStreamLines {
			lines = append(lines, fmt.Sprintf("and %d more", len(streams)-maxStreamLines))
			break
		}
		sv := s.vars()
		var parts []string
		for _, k := range []string{"user_profile", "title", "device", "bitrate"} {
			if sv[k] != "" {
				parts = append(parts, sv[k])
			}
		}
		lines = append(lines, strings.Join(parts, " · "))
	}
	v["streams"] = strings.Join(lines, "\n")
	return v
}

// --- Known devices and addresses ---

// seenStore keeps each account's known devices and remote addresses in
// seen/00..31, by account ID.
type seenStore struct {
	state stateBackend
	now   func() time.Time
	mu    sync.Mutex
}

func newSeenStore(state stateBackend) *seenStore {
	return &seenStore{state: state, now: time.Now}
}

// seenUser is one account's known devices and addresses.
type seenUser struct {
	Devices map[string]seenItem `json:"devices"`
	IPs     map[string]seenItem `json:"ips"`
	// Updated is when the entry last changed (Unix seconds).
	Updated int64 `json:"updated"`
}

// seenItem is when a device or address was last seen (Unix seconds,
// refreshed about hourly) and, for one an event added, that event's ID, so a
// retry of the event still finds it new.
type seenItem struct {
	At    int64  `json:"t"`
	Event string `json:"e,omitempty"`
}

// seenBucket is one seen/NN key, by account ID. keep is the account being
// updated, which a size cap never drops.
type seenBucket struct {
	users map[string]*seenUser
	keep  string
}

func (b *seenBucket) MarshalJSON() ([]byte, error) { return json.Marshal(b.users) }
func (b *seenBucket) entries() int {
	if _, ok := b.users[b.keep]; ok {
		return len(b.users) - 1
	}
	return len(b.users)
}

func (b *seenBucket) dropOldest(n int) {
	for _, id := range oldestFirst(b.users, func(u *seenUser) time.Time { return time.Unix(u.Updated, 0) }) {
		if n == 0 {
			return
		}
		if id != b.keep {
			delete(b.users, id)
			n--
		}
	}
}

func seenBucketKey(userID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(userID))
	return fmt.Sprintf("seen/%02d", h.Sum32()%seenBuckets)
}

func (s *seenStore) read(ctx context.Context, key string) (map[string]*seenUser, []byte, error) {
	raw, found, err := s.state.Read(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	users := map[string]*seenUser{}
	if found && json.Unmarshal(raw, &users) != nil {
		users = map[string]*seenUser{}
	}
	for id, u := range users {
		if u == nil {
			delete(users, id)
			continue
		}
		if u.Devices == nil {
			u.Devices = map[string]seenItem{}
		}
		if u.IPs == nil {
			u.IPs = map[string]seenItem{}
		}
	}
	return users, raw, nil
}

// seedFunc lists the devices and remote addresses Silo already knows for an
// account.
type seedFunc func(ctx context.Context) (devices, ips []string, err error)

// check records the device and remote address (an ipKey) an account played
// from and reports which it hadn't used before. The first time an account
// is seen, seed supplies what Silo already knows and nothing counts as new.
// eventKey marks what this event added.
func (s *seenStore) check(ctx context.Context, userID, deviceID, ip, eventKey string, seed seedFunc) (newDevice, newIP bool, err error) {
	key := seenBucketKey(userID)
	users, _, err := s.read(ctx, key)
	if err != nil {
		return false, false, err
	}
	var seedDevices, seedIPs []string
	if users[userID] == nil {
		// Silo's calls happen outside the lock; the bucket is read again
		// under it.
		if seedDevices, seedIPs, err = seed(ctx); err != nil {
			return false, false, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	users, raw, err := s.read(ctx, key)
	if err != nil {
		return false, false, err
	}
	now := s.now().Unix()
	u := users[userID]
	changed := false
	if u == nil {
		u = &seenUser{Devices: map[string]seenItem{}, IPs: map[string]seenItem{}}
		for _, d := range append(seedDevices, deviceID) {
			if d != "" {
				u.Devices[d] = seenItem{At: now}
			}
		}
		for _, a := range seedIPs {
			if k := ipKey(a); k != "" {
				u.IPs[k] = seenItem{At: now}
			}
		}
		if ip != "" {
			u.IPs[ip] = seenItem{At: now}
		}
		users[userID], changed = u, true
	} else {
		var c1, c2 bool
		newDevice, c1 = touchSeen(u.Devices, deviceID, eventKey, now)
		newIP, c2 = touchSeen(u.IPs, ip, eventKey, now)
		changed = c1 || c2
	}
	if !changed {
		return newDevice, newIP, nil
	}
	capSeen(u.Devices)
	capSeen(u.IPs)
	u.Updated = now
	// A failed write doesn't hold back the alerts: it only means the next
	// play finds the same device or address new again, or seeds the account
	// again.
	if next, err := encodeCapped(&seenBucket{users: users, keep: userID}); err == nil && string(next) != string(raw) {
		_ = s.state.Write(ctx, key, next)
	}
	return newDevice, newIP, nil
}

// touchSeen records id in m and reports whether it is new, and whether m
// changed. A retry of the event that added it still finds it new. Known ones
// are refreshed at most hourly, to keep writes down.
func touchSeen(m map[string]seenItem, id, eventKey string, now int64) (isNew, changed bool) {
	if id == "" {
		return false, false
	}
	it, ok := m[id]
	switch {
	case !ok:
		m[id] = seenItem{At: now, Event: eventKey}
		return true, true
	case eventKey != "" && it.Event == eventKey:
		return true, false
	case now-it.At >= 3600:
		m[id] = seenItem{At: now}
		return false, true
	}
	return false, false
}

// capSeen keeps the newest maxSeenPerUser entries.
func capSeen(m map[string]seenItem) {
	if n := len(m) - maxSeenPerUser; n > 0 {
		for _, k := range oldestFirst(m, func(it seenItem) time.Time { return time.Unix(it.At, 0) })[:n] {
			delete(m, k)
		}
	}
}

// ipKey is how an address is remembered: an IPv4 address as is, an IPv6
// address by its /64 network, since devices change the rest often.
func ipKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(ip))
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// seenIP is the address the new IP alert tracks for pb: remote ones only.
func seenIP(pb playback) string {
	if pb.Location != "remote" || pb.ClientIP == "" {
		return ""
	}
	return ipKey(pb.ClientIP)
}

// --- Scan and job failures ---

const (
	eventScanFailed = "scan.failed"
	eventJobFailed  = "job.failed"
)

func isServerEvent(name string) bool { return name == eventScanFailed || name == eventJobFailed }

// scanRun is a scan.* payload (silo-server internal/events ScanRun).
type scanRun struct {
	ID           string     `json:"id"`
	LibraryID    int        `json:"library_id"`
	Mode         string     `json:"mode"`
	Path         string     `json:"path"`
	Trigger      string     `json:"trigger"`
	Status       string     `json:"status"`
	CompletedAt  *time.Time `json:"completed_at"`
	ErrorMessage string     `json:"error_message"`
}

// adminJob is a job.* payload (silo-server models.AdminJob).
type adminJob struct {
	ID           string     `json:"id"`
	JobType      string     `json:"job_type"`
	Status       string     `json:"status"`
	Message      string     `json:"message"`
	ErrorMessage string     `json:"error_message"`
	CompletedAt  *time.Time `json:"completed_at"`
}

// serverAlert is a scan or job failure, ready for its recipients.
type serverAlert struct {
	kind string
	// key identifies the failure across retries. Silo publishes a failed
	// scan from more than one place, so it's the scan or job ID rather than
	// the event ID.
	key  string
	at   time.Time
	scan *scanRun
	job  *adminJob
}

func parseServerEvent(eventID, name string, raw []byte, now time.Time) (serverAlert, bool) {
	a := serverAlert{at: now}
	idKey := func(id string) string {
		if id != "" {
			return name + ":" + id
		}
		return eventID
	}
	switch name {
	case eventScanFailed:
		var run scanRun
		if json.Unmarshal(raw, &run) != nil || (run.ID == "" && run.LibraryID == 0) {
			return a, false
		}
		a.kind, a.key, a.scan = alertScanFailed, idKey(run.ID), &run
		if run.CompletedAt != nil {
			a.at = *run.CompletedAt
		}
	case eventJobFailed:
		var job adminJob
		if json.Unmarshal(raw, &job) != nil || (job.ID == "" && job.JobType == "") {
			return a, false
		}
		a.kind, a.key, a.job = alertJobFailed, idKey(job.ID), &job
		if job.CompletedAt != nil {
			a.at = *job.CompletedAt
		}
	default:
		return a, false
	}
	return a, true
}

// vars fills the alert's placeholders.
func (p *Plugin) serverAlertVars(ctx context.Context, a serverAlert) map[string]string {
	if run := a.scan; run != nil {
		library := ""
		if run.LibraryID > 0 {
			library = p.libraryName(ctx, strconv.Itoa(run.LibraryID))
		}
		scan := "Scan"
		if run.Mode != "" {
			scan = titleCase(strings.ReplaceAll(run.Mode, "_", " ")) + " scan"
		}
		return map[string]string{
			"library": cmp.Or(library, "a library"),
			"error":   errorSummary(cmp.Or(run.ErrorMessage, "Silo didn't say why.")),
			"scan":    scan,
			"path":    run.Path,
			"trigger": titleCase(strings.ReplaceAll(run.Trigger, "_", " ")),
		}
	}
	job := a.job
	errText := cmp.Or(job.ErrorMessage, job.Message, "Silo didn't say why.")
	details := ""
	if job.Message != "" && job.Message != errText {
		details = errorSummary(job.Message)
	}
	return map[string]string{
		"job":     jobLabel(job.JobType),
		"error":   errorSummary(errText),
		"details": details,
	}
}

// jobLabel names Silo's admin job types (silo-server internal/adminjob).
func jobLabel(jobType string) string {
	switch jobType {
	case "library_refresh":
		return "Library refresh"
	case "item_refresh":
		return "Item refresh"
	case "delete_library":
		return "Library removal"
	case "catalog_export":
		return "Catalog export"
	case "catalog_import":
		return "Catalog import"
	case "storage_transition":
		return "Storage move"
	case "image_cache_cleanup":
		return "Image cache cleanup"
	case "template_bundle_apply":
		return "Template bundle"
	case "":
		return "A job"
	}
	return humanizeType(jobType)
}

// errorSummary is an error message on one line, cut to 300 characters.
func errorSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 300 {
		s = string([]rune(s)[:299]) + "…"
	}
	return s
}

// handleServerEvent sends the scan.failed and job.failed alerts. Like the
// playback alerts, each recipient is handled on its own and their role is
// checked first.
func (p *Plugin) handleServerEvent(ctx context.Context, eventID, eventName string, raw []byte) error {
	cfg := p.config()
	if cfg.monitorKey() == "" {
		return nil
	}
	alert, ok := parseServerEvent(eventID, eventName, raw, time.Now())
	if !ok {
		return nil
	}
	log := p.logger.With("event", alert.key, "event_name", eventName)
	admins, err := waitForHost(ctx, func() ([]*AdminSettings, error) { return p.admins.List(ctx) })
	if err != nil {
		log.Warn("could not load admin alert settings; asking Silo to retry", "error", err)
		return fmt.Errorf("load admin alert settings: %w", err)
	}
	var candidates []*recipient
	for _, a := range admins {
		if a.alert(alert.kind).Enabled {
			candidates = append(candidates, newRecipient(a, alert.kind, alert.key))
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	base, err := p.siloBase(ctx)
	if err != nil {
		log.Warn("Silo's internal URL isn't known yet; asking Silo to retry", "error", err)
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, lookupBudget)
	var vars map[string]string
	var wg sync.WaitGroup
	wg.Go(func() { vars = p.serverAlertVars(lctx, alert) })
	candidates, stop, retry := p.confirmAdmins(lctx, log, base, cfg.monitorKey(), candidates)
	wg.Wait()
	cancel()
	if stop {
		return nil
	}
	candidates, again, err := p.dropHandled(ctx, log, candidates)
	if err != nil {
		log.Warn("could not read handled events; asking Silo to retry", "error", err)
		return fmt.Errorf("read handled events: %w", err)
	}
	for _, r := range candidates {
		r.vars = vars
	}
	if p.fanOut(ctx, log, candidates, alert.at) || retry || again {
		return errors.New("some admin alerts couldn't be sent yet")
	}
	return nil
}

// --- Libraries ---

// siloLibrary is one of Silo's libraries, from the host.
type siloLibrary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const libraryListTTL = 5 * time.Minute

// libraries lists Silo's libraries through the host, cached for a few
// minutes. A failure returns the last list, if any.
func (p *Plugin) libraries(ctx context.Context) ([]siloLibrary, error) {
	p.libsMu.Lock()
	defer p.libsMu.Unlock()
	if p.libsList != nil && time.Since(p.libsAt) < libraryListTTL {
		return p.libsList, nil
	}
	libs, err := p.listLibrariesFn(ctx)
	if err != nil {
		if p.libsList != nil {
			return p.libsList, nil
		}
		return nil, err
	}
	slices.SortFunc(libs, func(a, b siloLibrary) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	p.libsList, p.libsAt = libs, time.Now()
	return libs, nil
}

// libraryName names a library by ID, or "" if it can't be found.
func (p *Plugin) libraryName(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	libs, _ := p.libraries(ctx)
	for _, l := range libs {
		if l.ID == id {
			return l.Name
		}
	}
	return ""
}
