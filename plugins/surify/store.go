package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

var errHostUnavailable = errors.New("silo host connection is not ready yet")

// ProfileSettings is one Silo profile's Gotify configuration.
type ProfileSettings struct {
	// AdminManaged is set only by an acting administrator saving their own primary connection.
	AdminManaged bool   `json:"admin_managed,omitempty"`
	ProfileID    string `json:"profile_id"`
	ProfileName  string `json:"profile_name"`
	UserID       int    `json:"user_id"`
	Enabled      bool   `json:"enabled"`
	ServerURL    string `json:"server_url"`
	// AppToken is the profile's own Gotify application token. Empty means
	// use the admin's shared token.
	AppToken     string                  `json:"app_token,omitempty"`
	Device       string                  `json:"device,omitempty"`
	Sound        string                  `json:"sound,omitempty"`
	Priority     int                     `json:"priority"`
	HTML         bool                    `json:"html"`
	AttachPoster bool                    `json:"attach_poster"`
	IncludeLink  bool                    `json:"include_link"`
	Types        map[string]TypeSettings `json:"types"`
	UpdatedAt    time.Time               `json:"updated_at"`

	// The fields below came with 0.3.0. Settings saved by 0.2.0 don't have
	// them and load with everything off.

	// EpisodeReasons limits new-episode notifications to episodes Silo lists
	// at least one of these reasons for (reasonFavorite and so on). Empty
	// means every episode.
	EpisodeReasons []string `json:"episode_reasons,omitempty"`
	// GroupMinutes holds a show's first new episode for this many minutes and
	// merges the ones that follow into one notification. Zero sends each
	// episode on its own.
	GroupMinutes int `json:"group_minutes,omitempty"`
	// TimeZone is the IANA time zone the browser reported when the profile
	// last saved. Quiet hours are in it; empty means UTC.
	TimeZone string `json:"time_zone,omitempty"`
	// Quiet turns this profile's notifications down between two times of day.
	Quiet QuietHours `json:"quiet,omitzero"`
}

// Reasons Silo gives for a new episode (reason_flags on episode.available).
const (
	reasonFavorite         = "favorite"
	reasonWatchlist        = "watchlist"
	reasonContinueWatching = "continue_watching"
	reasonNextUp           = "next_up"

	minGroupMinutes = 1
	maxGroupMinutes = 120
	// defaultGroupMinutes is what the page offers when grouping is switched on.
	defaultGroupMinutes = 5
)

// episodeReasonOptions are the reasons the page offers, in its order.
var episodeReasonOptions = []option{
	{reasonFavorite, "Favorites"},
	{reasonWatchlist, "Watchlist"},
	{reasonContinueWatching, "Continue Watching"},
	{reasonNextUp, "Next Up"},
}

// TypeSettings controls one notification type for a profile.
type TypeSettings struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title"`
	Message string `json:"message"`
	// Priority overrides the profile default when set.
	Priority *int `json:"priority,omitempty"`
	// Sound overrides the profile default when non-empty.
	Sound string `json:"sound,omitempty"`
	// GroupTitle and GroupMessage are the wording of a merged notification, for
	// the types that can be merged. Both empty means the type's default.
	GroupTitle   string `json:"group_title,omitempty"`
	GroupMessage string `json:"group_message,omitempty"`
}

// groupWording is the title and message template of a merged notification.
func (ts TypeSettings) groupWording(t notificationType) (title, message string) {
	if ts.GroupTitle == "" && ts.GroupMessage == "" {
		return t.DefaultGroupTitle, t.DefaultGroupMessage
	}
	return ts.GroupTitle, ts.GroupMessage
}

// typeSettings returns the stored settings for a type, or its defaults.
func (s *ProfileSettings) typeSettings(key string) TypeSettings {
	if ts, ok := s.Types[key]; ok {
		return ts
	}
	return notificationTypeByKey(key).defaults()
}

// wantsEpisode reports whether the profile's reason filter lets an episode
// with these reasons through.
func (s *ProfileSettings) wantsEpisode(f deliveryFlags) bool {
	if len(s.EpisodeReasons) == 0 {
		return true
	}
	for _, r := range s.EpisodeReasons {
		if (r == reasonFavorite && f.Favorite) || (r == reasonWatchlist && f.Watchlist) ||
			(r == reasonContinueWatching && f.ContinueWatching) || (r == reasonNextUp && f.NextUp) {
			return true
		}
	}
	return false
}

// groupWindow is how long a show's first new episode is held; zero when the
// profile doesn't group.
func (s *ProfileSettings) groupWindow() time.Duration {
	if s.GroupMinutes < minGroupMinutes {
		return 0
	}
	return time.Duration(min(s.GroupMinutes, maxGroupMinutes)) * time.Minute
}

// priorityAt is the priority a notification goes out with at the given time,
// after the profile's quiet hours.
func (s *ProfileSettings) priorityAt(priority int, at time.Time) int {
	return s.Quiet.priority(priority, at.In(locationOf(s.TimeZone)))
}

func (s *ProfileSettings) clone() *ProfileSettings {
	c := *s
	c.Types = maps.Clone(s.Types)
	c.EpisodeReasons = slices.Clone(s.EpisodeReasons)
	return &c
}

// stateBackend is the host's encrypted per-installation key/value store.
type stateBackend interface {
	Read(ctx context.Context, key string) ([]byte, bool, error)
	Write(ctx context.Context, key string, value []byte) error
}

type hostStateBackend struct{}

func (hostStateBackend) Read(ctx context.Context, key string) ([]byte, bool, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, false, errHostUnavailable
	}
	return h.ReadInstanceState(ctx, key)
}

func (hostStateBackend) Write(ctx context.Context, key string, value []byte) error {
	h := sdkruntime.Host()
	if h == nil {
		return errHostUnavailable
	}
	return h.WriteInstanceState(ctx, key, value)
}

// profileStore caches every profile's settings in memory and persists them to
// host instance state. The host allows at most 256 keys of 256 KiB each, so
// profiles are spread across a fixed set of bucket keys instead of one key
// per profile.
//
// Every Silo API server runs its own plugin process against the same state,
// so the cache is reloaded once it is older than ttl, and each write re-reads
// its bucket first so it doesn't undo another server's change.
type profileStore struct {
	state stateBackend
	ttl   time.Duration
	now   func() time.Time

	// loadMu serializes reloads and writes, so a reload that started before
	// a write can't put stale data back in the cache.
	loadMu sync.Mutex

	mu       sync.RWMutex
	loadedAt time.Time // zero until the first successful full load
	// bucketAt is when each bucket was last read or written on its own, so
	// Get can refresh one bucket instead of reloading all of them.
	bucketAt map[string]time.Time
	profiles map[string]*ProfileSettings
}

const (
	storeBuckets     = 64
	settingsCacheTTL = 30 * time.Second
)

func newProfileStore(state stateBackend) *profileStore {
	return &profileStore{state: state, ttl: settingsCacheTTL, now: time.Now, bucketAt: map[string]time.Time{}, profiles: map[string]*ProfileSettings{}}
}

func bucketKey(profileID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(profileID))
	return fmt.Sprintf("profiles/%02d", h.Sum32()%storeBuckets)
}

// fresh reports whether the cache was loaded and is younger than the TTL.
func (s *profileStore) fresh() (fresh, loaded bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	loaded = !s.loadedAt.IsZero()
	return loaded && s.now().Sub(s.loadedAt) < s.ttl, loaded
}

// bucketFresh is fresh for one bucket: either the full cache or that bucket
// on its own is younger than the TTL.
func (s *profileStore) bucketFresh(key string) (fresh, loaded bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	at := s.bucketAt[key]
	if s.loadedAt.After(at) {
		at = s.loadedAt
	}
	loaded = !at.IsZero()
	return loaded && s.now().Sub(at) < s.ttl, loaded
}

// ensureLoaded reloads the cache when it is missing or past its TTL. If a
// reload fails but an older copy exists, that copy keeps serving and the next
// call tries again.
func (s *profileStore) ensureLoaded(ctx context.Context) error {
	if fresh, _ := s.fresh(); fresh {
		return nil
	}
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	return s.ensureLoadedLocked(ctx)
}

// ensureLoadedLocked is ensureLoaded for callers that hold loadMu.
func (s *profileStore) ensureLoadedLocked(ctx context.Context) error {
	fresh, loaded := s.fresh()
	if fresh {
		return nil
	}
	err := s.reloadLocked(ctx)
	if err != nil && loaded {
		return nil
	}
	return err
}

func (s *profileStore) reloadLocked(ctx context.Context) error {
	profiles := map[string]*ProfileSettings{}
	for i := range storeBuckets {
		bucket, err := s.readBucket(ctx, fmt.Sprintf("profiles/%02d", i))
		if err != nil {
			return err
		}
		maps.Copy(profiles, bucket)
	}
	s.mu.Lock()
	s.profiles = profiles
	s.loadedAt = s.now()
	s.bucketAt = map[string]time.Time{}
	s.mu.Unlock()
	return nil
}

func (s *profileStore) readBucket(ctx context.Context, key string) (map[string]*ProfileSettings, error) {
	raw, found, err := s.state.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	bucket := map[string]*ProfileSettings{}
	if !found {
		return bucket, nil
	}
	if err := json.Unmarshal(raw, &bucket); err != nil {
		return nil, fmt.Errorf("decode settings bucket %s: %w", key, err)
	}
	return bucket, nil
}

// Get returns a copy of a profile's settings. When the cache is missing or
// stale it re-reads only the profile's own bucket, so a delivery costs one
// state read instead of a full reload.
func (s *profileStore) Get(ctx context.Context, profileID string) (*ProfileSettings, bool, error) {
	key := bucketKey(profileID)
	if fresh, _ := s.bucketFresh(key); !fresh {
		if err := s.refreshBucket(ctx, key); err != nil {
			return nil, false, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	settings, ok := s.profiles[profileID]
	if !ok {
		return nil, false, nil
	}
	return settings.clone(), true, nil
}

// refreshBucket re-reads one bucket into the cache. If the read fails but the
// cache was loaded before, the older copy keeps serving.
func (s *profileStore) refreshBucket(ctx context.Context, key string) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	fresh, loaded := s.bucketFresh(key)
	if fresh {
		return nil
	}
	bucket, err := s.readBucket(ctx, key)
	if err != nil {
		if loaded {
			return nil
		}
		return err
	}
	s.replaceBucket(key, bucket)
	return nil
}

// replaceBucket swaps one bucket's profiles in memory for the given ones,
// which were just read from or written to state.
func (s *profileStore) replaceBucket(key string, bucket map[string]*ProfileSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketAt[key] = s.now()
	for id := range s.profiles {
		if bucketKey(id) == key {
			delete(s.profiles, id)
		}
	}
	maps.Copy(s.profiles, bucket)
}

// ListByUser returns copies of the settings saved for one Silo account's
// profiles, sorted by profile name.
func (s *profileStore) ListByUser(ctx context.Context, userID int) ([]*ProfileSettings, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	var out []*ProfileSettings
	for _, settings := range s.profiles {
		if settings.UserID == userID {
			out = append(out, settings.clone())
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b *ProfileSettings) int {
		if c := strings.Compare(strings.ToLower(a.ProfileName), strings.ToLower(b.ProfileName)); c != 0 {
			return c
		}
		return strings.Compare(a.ProfileID, b.ProfileID)
	})
	return out, nil
}

// Put stores a profile's settings.
func (s *profileStore) Put(ctx context.Context, settings *ProfileSettings) error {
	return s.mutate(ctx, settings.ProfileID, settings.clone())
}

// Delete removes a profile's settings.
func (s *profileStore) Delete(ctx context.Context, profileID string) error {
	return s.mutate(ctx, profileID, nil)
}

// mutate re-reads the profile's bucket from state, writes it back with the
// change applied, and updates memory only once the write succeeds.
func (s *profileStore) mutate(ctx context.Context, profileID string, next *ProfileSettings) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()

	key := bucketKey(profileID)
	bucket, err := s.readBucket(ctx, key)
	if err != nil {
		return err
	}
	if next == nil {
		delete(bucket, profileID)
	} else {
		bucket[profileID] = next
	}

	raw, err := json.Marshal(bucket)
	if err != nil {
		return err
	}
	if len(raw) > runtimehost.MaxInstanceStateValueBytes {
		return errors.New("too many profiles are configured to store another one")
	}
	if err := s.state.Write(ctx, key, raw); err != nil {
		return err
	}

	// Replace this bucket's profiles in memory with what was just written,
	// which also picks up other servers' changes to the same bucket.
	s.replaceBucket(key, bucket)
	return nil
}

// eventLog records the IDs of events that are done (sent, or rejected for
// good) in instance state, so a retry or repeat that reaches any API server
// is skipped. IDs are hashed into eventLogBuckets keys; each key keeps its
// newest eventLogPerBucket IDs, oldest first. Silo stops retrying an event
// after about 20 minutes, so this only needs to cover a short window.
type eventLog struct {
	state stateBackend
	// recent short-cuts state reads for IDs this process already recorded.
	recent *recentIDs
	// mu serializes this process's read-modify-write of a bucket.
	mu sync.Mutex
}

const (
	eventLogBuckets   = 4
	eventLogPerBucket = 1000
	// maxEventIDBytes keeps one bucket under the host's value limit however
	// long the IDs are; longer IDs are stored as a hash.
	maxEventIDBytes = 200
)

func newEventLog(state stateBackend) *eventLog {
	return &eventLog{state: state, recent: newRecentIDs(1024)}
}

// eventLogEntry is the stored form of an ID: the ID itself unless it is too
// long to keep 1000 of them in one value.
func eventLogEntry(id string) string {
	if len(id) <= maxEventIDBytes {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func eventLogKey(entry string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(entry))
	return fmt.Sprintf("events/%d", h.Sum32()%eventLogBuckets)
}

func (l *eventLog) read(ctx context.Context, key string) ([]string, error) {
	raw, found, err := l.state.Read(ctx, key)
	if err != nil || !found {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		// A damaged bucket only costs dedupe for its IDs; start it over.
		return nil, nil
	}
	return ids, nil
}

// eventIDList is one events/ bucket, oldest first.
type eventIDList []string

func (l *eventIDList) entries() int     { return len(*l) }
func (l *eventIDList) dropOldest(n int) { *l = (*l)[min(n, len(*l)):] }

// Seen reports whether id was recorded.
func (l *eventLog) Seen(ctx context.Context, id string) (bool, error) {
	entry := eventLogEntry(id)
	if l.recent.Has(entry) {
		return true, nil
	}
	ids, err := l.read(ctx, eventLogKey(entry))
	if err != nil {
		return false, err
	}
	if slices.Contains(ids, entry) {
		l.recent.Add(entry)
		return true, nil
	}
	return false, nil
}

// SeenMany reports which of ids were recorded, reading each bucket at most
// once.
func (l *eventLog) SeenMany(ctx context.Context, ids []string) (map[string]bool, error) {
	seen := map[string]bool{}
	buckets := map[string][]string{}
	for _, id := range ids {
		entry := eventLogEntry(id)
		if l.recent.Has(entry) {
			seen[id] = true
			continue
		}
		key := eventLogKey(entry)
		stored, ok := buckets[key]
		if !ok {
			var err error
			if stored, err = l.read(ctx, key); err != nil {
				return nil, err
			}
			buckets[key] = stored
		}
		if slices.Contains(stored, entry) {
			l.recent.Add(entry)
			seen[id] = true
		}
	}
	return seen, nil
}

// Record adds id, dropping the bucket's oldest IDs past the cap.
func (l *eventLog) Record(ctx context.Context, id string) error {
	return l.RecordMany(ctx, []string{id})
}

// RecordMany adds ids, writing each bucket once.
func (l *eventLog) RecordMany(ctx context.Context, ids []string) error {
	byKey := map[string][]string{}
	for _, id := range ids {
		entry := eventLogEntry(id)
		l.recent.Add(entry)
		byKey[eventLogKey(entry)] = append(byKey[eventLogKey(entry)], entry)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for key, entries := range byKey {
		stored, err := l.read(ctx, key)
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		changed := false
		for _, entry := range entries {
			if !slices.Contains(stored, entry) {
				stored, changed = append(stored, entry), true
			}
		}
		if !changed {
			continue
		}
		if len(stored) > eventLogPerBucket {
			stored = stored[len(stored)-eventLogPerBucket:]
		}
		ids := eventIDList(stored)
		raw, err := encodeCapped(&ids)
		if err == nil {
			err = l.state.Write(ctx, key, raw)
		}
		firstErr = cmp.Or(firstErr, err)
	}
	return firstErr
}
