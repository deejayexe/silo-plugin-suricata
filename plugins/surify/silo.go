package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors for the read-only Silo key. Messages never include the key.
var (
	errSiloKeyMissing  = errors.New("no read-only Silo key is configured")
	errSiloKeyRejected = errors.New("Silo rejected the read-only key (HTTP 401)")
	errSiloKeyScope    = errors.New("the read-only Silo key isn't allowed to call this route (HTTP 403)")
)

type siloProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// siloClient is a read-only client for Silo's admin API, using the
// admin:monitor:read key. It only sends GET requests.
type siloClient struct {
	http    *http.Client
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[int]cachedProfiles
	users map[int]cachedUser
	libs  map[string]cachedLibraries
}

// cachedProfiles is one account's profile list, or the key error that
// stopped it. Network errors aren't cached.
type cachedProfiles struct {
	profiles []siloProfile
	err      error
	at       time.Time
}

// cachedUser is one account as Silo reported it, or the error that stopped
// the lookup. Network errors aren't cached.
type cachedUser struct {
	user *siloUser
	err  error
	at   time.Time
}

const (
	profileCacheTTL = 30 * time.Second
	siloCallTimeout = 5 * time.Second
	// userCacheTTL is how long an account's role is trusted before admin
	// alerts check it again.
	userCacheTTL = 10 * time.Minute
)

func newSiloClient() *siloClient {
	return &siloClient{
		http:    &http.Client{Timeout: 10 * time.Second},
		timeout: siloCallTimeout,
		now:     time.Now,
		cache:   map[int]cachedProfiles{},
		users:   map[int]cachedUser{},
		libs:    map[string]cachedLibraries{},
	}
}

// Invalidate drops cached lookups, e.g. after the key changes.
func (c *siloClient) Invalidate() {
	c.mu.Lock()
	c.cache = map[int]cachedProfiles{}
	c.users = map[int]cachedUser{}
	c.libs = map[string]cachedLibraries{}
	c.mu.Unlock()
}

// siloUser is the part of Silo's AdminUser the plugin reads.
type siloUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}

// isAdmin reports whether the account is an enabled admin.
func (u *siloUser) isAdmin() bool { return u != nil && u.Role == "admin" && u.Enabled }

var errSiloUserNotFound = errors.New("Silo has no such account")

// User looks up one account (GET /api/v2/admin/users/{id}, allowed by
// admin:monitor:read), cached for userCacheTTL. A deleted account is
// errSiloUserNotFound; key errors are cached like AccountProfiles's.
func (c *siloClient) User(ctx context.Context, baseURL, apiKey string, userID int) (*siloUser, error) {
	return c.user(ctx, baseURL, apiKey, userID, true)
}

// CurrentUser bypasses cached roles for access and recipient authorization.
func (c *siloClient) CurrentUser(ctx context.Context, baseURL, apiKey string, userID int) (*siloUser, error) {
	return c.user(ctx, baseURL, apiKey, userID, false)
}
func (c *siloClient) user(ctx context.Context, baseURL, apiKey string, userID int, useCache bool) (*siloUser, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	c.mu.Lock()
	if cached, ok := c.users[userID]; useCache && ok {
		ttl := userCacheTTL
		if cached.err != nil && !errors.Is(cached.err, errSiloUserNotFound) {
			ttl = profileCacheTTL
		}
		if c.now().Sub(cached.at) < ttl {
			c.mu.Unlock()
			return cached.user, cached.err
		}
	}
	c.mu.Unlock()

	var user siloUser
	err := c.get(ctx, baseURL, apiKey, "/api/v2/admin/users/"+strconv.Itoa(userID), &user)
	var serr *siloStatusError
	switch {
	case errors.As(err, &serr) && serr.Status == http.StatusNotFound:
		err = errSiloUserNotFound
	case err != nil && !errors.Is(err, errSiloKeyRejected) && !errors.Is(err, errSiloKeyScope):
		return nil, fmt.Errorf("look up account: %w", err)
	}
	entry := cachedUser{err: err, at: c.now()}
	if err == nil {
		entry.user = &user
	}
	if useCache {
		c.mu.Lock()
		c.users[userID] = entry
		c.mu.Unlock()
	}
	return entry.user, err
}

// Users lists every account (GET /api/v2/admin/users), for the page's
// "ignore users" picker. Not cached.
func (c *siloClient) Users(ctx context.Context, baseURL, apiKey string) ([]siloUser, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var all []siloUser
	cursor := ""
	for range 25 {
		path := "/api/v2/admin/users?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []siloUser `json:"items"`
			Page  *struct {
				NextCursor string `json:"next_cursor"`
				HasMore    bool   `json:"has_more"`
			} `json:"page"`
		}
		if err := c.get(ctx, baseURL, apiKey, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Page == nil || !page.Page.HasMore || page.Page.NextCursor == "" {
			return all, nil
		}
		cursor = page.Page.NextCursor
	}
	return all, nil
}

// pageInfo is Silo's cursor state on a collection.
type pageInfo struct {
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// next returns the cursor for the following page, or "" on the last one.
func (p *pageInfo) next() string {
	if p == nil || !p.HasMore {
		return ""
	}
	return p.NextCursor
}

// Sessions lists every live playback session (GET /api/v2/admin/sessions),
// following the pages. Not cached: the watchdog recounts before each check.
func (c *siloClient) Sessions(ctx context.Context, baseURL, apiKey string) ([]adminSession, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var all []adminSession
	cursor := ""
	for range 25 {
		path := "/api/v2/admin/sessions?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []adminSession `json:"items"`
			Page  *pageInfo      `json:"page"`
		}
		if err := c.get(ctx, baseURL, apiKey, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if cursor = page.Page.next(); cursor == "" {
			return all, nil
		}
	}
	return nil, errors.New("Silo listed too many live sessions")
}

// siloNode is the part of Silo's AdminNodeSummary the health check reads.
type siloNode struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Type            string     `json:"type"`
	Enabled         bool       `json:"enabled"`
	Healthy         bool       `json:"healthy"`
	LastHealthCheck *time.Time `json:"last_health_check"`
}

// NodeSummaries lists Silo's stream nodes (GET /api/v2/admin/nodes/summary,
// allowed by admin:monitor:read), following the pages.
func (c *siloClient) NodeSummaries(ctx context.Context, baseURL, apiKey string) ([]siloNode, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var all []siloNode
	cursor := ""
	for range 10 {
		path := "/api/v2/admin/nodes/summary?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []siloNode `json:"items"`
			Page  *pageInfo  `json:"page"`
		}
		if err := c.get(ctx, baseURL, apiKey, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if cursor = page.Page.next(); cursor == "" {
			return all, nil
		}
	}
	return nil, errors.New("Silo listed too many nodes")
}

// siloComponent is one dependency's health in AdminServerStatus: OK is
// absent when Silo hasn't checked it.
type siloComponent struct {
	Configured bool  `json:"configured"`
	OK         *bool `json:"ok"`
}

// siloServerStatus is the part of AdminServerStatus the health check reads.
type siloServerStatus struct {
	RestartRequired        bool     `json:"restart_required"`
	RestartRequiredReason  string   `json:"restart_required_reason"`
	RestartRequiredReasons []string `json:"restart_required_reasons"`
	Health                 struct {
		Postgres siloComponent `json:"postgres"`
		Redis    siloComponent `json:"redis"`
	} `json:"health"`
}

// ServerStatus reads restart state and dependency health
// (GET /api/v2/admin/server/status, allowed by admin:monitor:read).
func (c *siloClient) ServerStatus(ctx context.Context, baseURL, apiKey string) (*siloServerStatus, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var status siloServerStatus
	if err := c.get(ctx, baseURL, apiKey, "/api/v2/admin/server/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// siloDisk is one mount in AdminSystemStats. Scoped keys get no path, only a
// role ("scratch", "library-1", ...).
type siloDisk struct {
	Path        string  `json:"path"`
	Role        string  `json:"role"`
	UsedGB      float64 `json:"used_gb"`
	TotalGB     float64 `json:"total_gb"`
	Stale       bool    `json:"stale"`
	Unavailable bool    `json:"unavailable"`
	Scratch     bool    `json:"scratch"`
}

// siloResources is the part of AdminSystemResources the health check reads:
// the last sample of the API server that answered.
type siloResources struct {
	Available bool `json:"available"`
	Stale     bool `json:"stale"`
	System    *struct {
		MemUsedMB  int64      `json:"mem_used_mb"`
		MemTotalMB int64      `json:"mem_total_mb"`
		Disks      []siloDisk `json:"disks"`
	} `json:"system"`
}

// SystemResources reads the server's last resource sample
// (GET /api/v2/admin/system/resources, allowed by admin:monitor:read).
func (c *siloClient) SystemResources(ctx context.Context, baseURL, apiKey string) (*siloResources, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var res siloResources
	if err := c.get(ctx, baseURL, apiKey, "/api/v2/admin/system/resources", &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// siloTopActivity is AdminDashboardTopActivity.
type siloTopActivity struct {
	Limit    int `json:"limit"`
	Profiles []struct {
		UserID       string `json:"user_id"`
		Username     string `json:"username"`
		ProfileName  string `json:"profile_name"`
		Plays        int64  `json:"plays"`
		TotalSeconds int64  `json:"total_seconds"`
	} `json:"profiles"`
	Titles []struct {
		Title        string `json:"title"`
		MediaType    string `json:"media_type"`
		Plays        int64  `json:"plays"`
		TotalSeconds int64  `json:"total_seconds"`
	} `json:"titles"`
}

// TopActivity reads the most active profiles and most played titles of the
// last days days (GET /api/v2/admin/stats/top-activity, allowed by
// admin:monitor:read). Silo caps days at 30 and limit at 25.
func (c *siloClient) TopActivity(ctx context.Context, baseURL, apiKey string, days, limit int) (*siloTopActivity, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var top siloTopActivity
	path := fmt.Sprintf("/api/v2/admin/stats/top-activity?days=%d&limit=%d", days, limit)
	if err := c.get(ctx, baseURL, apiKey, path, &top); err != nil {
		return nil, err
	}
	return &top, nil
}

// siloPlaybackActivity is the part of AdminDashboardPlaybackActivity the
// digest reads.
type siloPlaybackActivity struct {
	Buckets []struct {
		Direct    int64 `json:"direct"`
		Remux     int64 `json:"remux"`
		Transcode int64 `json:"transcode"`
	} `json:"buckets"`
	Reliability struct {
		SessionsStarted   int64   `json:"sessions_started"`
		TranscodeStarts   int64   `json:"transcode_starts"`
		FinalizedSessions int64   `json:"finalized_sessions"`
		CompletedSessions int64   `json:"completed_sessions"`
		CompletionRate    float64 `json:"completion_rate"`
		UniqueProfiles    int64   `json:"unique_profiles"`
	} `json:"reliability"`
}

// PlaybackActivity reads how much was played over the last hours hours
// (GET /api/v2/admin/stats/playback-activity, allowed by admin:monitor:read).
// Silo caps hours at 744.
func (c *siloClient) PlaybackActivity(ctx context.Context, baseURL, apiKey string, hours int) (*siloPlaybackActivity, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	var act siloPlaybackActivity
	path := fmt.Sprintf("/api/v2/admin/stats/playback-activity?hours=%d", hours)
	if err := c.get(ctx, baseURL, apiKey, path, &act); err != nil {
		return nil, err
	}
	return &act, nil
}

// UserDevices lists the device IDs Silo knows for an account
// (GET /api/v2/admin/users/{id}/devices). A deleted account has none.
func (c *siloClient) UserDevices(ctx context.Context, baseURL, apiKey, userID string) ([]string, error) {
	if !accountIDPattern.MatchString(userID) {
		return nil, nil
	}
	var body struct {
		Items []struct {
			DeviceID string `json:"device_id"`
		} `json:"items"`
	}
	err := c.get(ctx, baseURL, apiKey, "/api/v2/admin/users/"+url.PathEscape(userID)+"/devices", &body)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range body.Items {
		if d.DeviceID != "" {
			ids = append(ids, d.DeviceID)
		}
	}
	return ids, nil
}

// UserRemoteIPs lists the remote addresses an account used in the last 90
// days, most recent first, up to maxSeenPerUser
// (GET /api/v2/admin/users/{id}/ips). A deleted account has none.
func (c *siloClient) UserRemoteIPs(ctx context.Context, baseURL, apiKey, userID string) ([]string, error) {
	if !accountIDPattern.MatchString(userID) {
		return nil, nil
	}
	var body struct {
		Items []struct {
			ClientIP string `json:"client_ip"`
			Location string `json:"location"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/api/v2/admin/users/%s/ips?days=90&limit=%d", url.PathEscape(userID), maxSeenPerUser)
	err := c.get(ctx, baseURL, apiKey, path, &body)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ips []string
	for _, ip := range body.Items {
		if ip.ClientIP != "" && ip.Location == "remote" {
			ips = append(ips, ip.ClientIP)
		}
	}
	return ips, nil
}

// cachedLibraries is the libraries one catalog item is in.
type cachedLibraries struct {
	ids []string
	at  time.Time
}

const (
	libraryCacheTTL  = 30 * time.Minute
	libraryCacheSize = 4096
)

// ContentLibraries returns the IDs of the libraries a catalog item is in
// (GET /api/v2/admin/catalog/items?content_ids=...), cached for
// libraryCacheTTL. An item the catalog doesn't list (an audiobook, or one no
// longer present) is in none.
func (c *siloClient) ContentLibraries(ctx context.Context, baseURL, apiKey, contentID string) ([]string, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	if contentID == "" {
		return nil, nil
	}
	c.mu.Lock()
	if cached, ok := c.libs[contentID]; ok && c.now().Sub(cached.at) < libraryCacheTTL {
		c.mu.Unlock()
		return cached.ids, nil
	}
	c.mu.Unlock()

	var body struct {
		Items []struct {
			ContentID  string   `json:"content_id"`
			LibraryIDs []string `json:"library_ids"`
		} `json:"items"`
	}
	path := "/api/v2/admin/catalog/items?limit=1&content_ids=" + url.QueryEscape(contentID)
	if err := c.get(ctx, baseURL, apiKey, path, &body); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, item := range body.Items {
		if item.ContentID == contentID {
			ids = item.LibraryIDs
		}
	}
	c.mu.Lock()
	if len(c.libs) >= libraryCacheSize {
		c.libs = map[string]cachedLibraries{}
	}
	c.libs[contentID] = cachedLibraries{ids: ids, at: c.now()}
	c.mu.Unlock()
	return ids, nil
}

func isNotFound(err error) bool {
	var serr *siloStatusError
	return errors.As(err, &serr) && serr.Status == http.StatusNotFound
}

// AccountProfiles lists the profiles of one Silo account
// (GET /api/v2/admin/users/{id}/profiles, allowed by admin:monitor:read).
func (c *siloClient) AccountProfiles(ctx context.Context, baseURL, apiKey string, userID int) ([]siloProfile, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	c.mu.Lock()
	if cached, ok := c.cache[userID]; ok && c.now().Sub(cached.at) < profileCacheTTL {
		c.mu.Unlock()
		return cached.profiles, cached.err
	}
	c.mu.Unlock()

	var body struct {
		Items []siloProfile `json:"items"`
	}
	err := c.get(ctx, baseURL, apiKey, "/api/v2/admin/users/"+strconv.Itoa(userID)+"/profiles", &body)
	if err != nil && !errors.Is(err, errSiloKeyRejected) && !errors.Is(err, errSiloKeyScope) {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	c.mu.Lock()
	c.cache[userID] = cachedProfiles{profiles: body.Items, err: err, at: c.now()}
	c.mu.Unlock()
	return body.Items, err
}

// get calls one GET route with the key and decodes the JSON answer.
func (c *siloClient) get(ctx context.Context, baseURL, apiKey, path string, out any) error {
	return siloCall(ctx, c.http, c.timeout, siloRequest{method: http.MethodGet, baseURL: baseURL, apiKey: apiKey, path: path}, out)
}

// siloStatusError is an answer other than success, 401 or 403. Detail is
// Silo's own explanation (its problem document), never a key.
type siloStatusError struct {
	Status int
	Detail string
}

func (e *siloStatusError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("Silo returned HTTP %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("Silo returned HTTP %d", e.Status)
}

// siloRequest is one call to Silo's API with a bearer key.
type siloRequest struct {
	method, baseURL, apiKey, path string
	body                          any
	ifMatch                       string
	// respHeader, when set, receives the answer's headers (any status).
	respHeader *http.Header
}

// siloCall sends one request and decodes a JSON answer into out (when out is
// not nil). 401 and 403 become errSiloKeyRejected and errSiloKeyScope.
// Errors never include the key or the request line.
func siloCall(ctx context.Context, client *http.Client, timeout time.Duration, r siloRequest, out any) error {
	if r.baseURL == "" {
		return errors.New("Silo's internal URL is not known yet")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.baseURL+r.path, body)
	if err != nil {
		return errors.New("couldn't build the request to Silo")
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.ifMatch != "" {
		req.Header.Set("If-Match", r.ifMatch)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Keep the cause, not the full request line.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	if r.respHeader != nil {
		*r.respHeader = resp.Header.Clone()
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errSiloKeyRejected
	case resp.StatusCode == http.StatusForbidden:
		return errSiloKeyScope
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &siloStatusError{Status: resp.StatusCode, Detail: problemDetail(resp.Body)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode Silo response: %w", err)
	}
	return nil
}

// problemDetail pulls a short explanation out of Silo's problem document.
func problemDetail(r io.Reader) string {
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Errors []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&problem) != nil {
		return ""
	}
	parts := []string{}
	for _, s := range []string{problem.Detail, problem.Title} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
			break
		}
	}
	for _, e := range problem.Errors {
		if s := strings.TrimSpace(e.Detail); s != "" {
			parts = append(parts, s)
		}
	}
	msg := strings.Join(parts, " ")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// Scopes the plugin's keys carry (silo-server internal/auth, mapped to routes
// in internal/api/middleware/api_key_scopes.go).
const (
	scopeMonitorRead     = "admin:monitor:read"
	scopeSessionsControl = "admin:sessions:control"
)

// siloAPIKey is one row of GET /api/v2/admin/api-keys. It never holds the
// key itself, only its first characters.
type siloAPIKey struct {
	ID        string   `json:"id"`
	UserID    string   `json:"user_id"`
	Label     string   `json:"label"`
	KeyPrefix string   `json:"key_prefix"`
	Scopes    []string `json:"scopes"`
}

// createdAPIKey is POST /api/v2/admin/api-keys's answer. Key is shown once.
type createdAPIKey struct {
	ID     string   `json:"id"`
	UserID string   `json:"user_id"`
	Label  string   `json:"label"`
	Key    string   `json:"key"`
	Scopes []string `json:"scopes"`
}

// keyAdmin manages Silo API keys with a full-access key an admin hands over
// for one setup. It is separate from siloClient, which only reads with the
// stored read-only key. The full-access key is passed in on every call and
// never kept.
type keyAdmin struct {
	http    *http.Client
	timeout time.Duration
}

func newKeyAdmin() *keyAdmin {
	return &keyAdmin{http: &http.Client{Timeout: 10 * time.Second}, timeout: keySetupCallTimeout}
}

// keySetupCallTimeout bounds each call. Silo gives the whole page request
// 10 s, and a setup makes several calls.
const keySetupCallTimeout = 4 * time.Second

func (k *keyAdmin) call(ctx context.Context, r siloRequest, out any) error {
	return siloCall(ctx, k.http, k.timeout, r, out)
}

// List returns every API key on the server (GET /api/v2/admin/api-keys,
// unscoped keys only), following the pages.
func (k *keyAdmin) List(ctx context.Context, baseURL, apiKey string) ([]siloAPIKey, error) {
	var all []siloAPIKey
	cursor := ""
	for range 50 {
		path := "/api/v2/admin/api-keys?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []siloAPIKey `json:"items"`
			Page  *struct {
				NextCursor string `json:"next_cursor"`
				HasMore    bool   `json:"has_more"`
			} `json:"page"`
		}
		if err := k.call(ctx, siloRequest{method: http.MethodGet, baseURL: baseURL, apiKey: apiKey, path: path}, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Page == nil || !page.Page.HasMore || page.Page.NextCursor == "" {
			return all, nil
		}
		cursor = page.Page.NextCursor
	}
	return nil, errors.New("Silo listed too many API keys")
}

// Create makes a key on account userID (POST /api/v2/admin/api-keys).
func (k *keyAdmin) Create(ctx context.Context, baseURL, apiKey string, userID int, label string, scopes []string) (*createdAPIKey, error) {
	body := map[string]any{"label": label, "user_id": strconv.Itoa(userID), "scopes": scopes}
	var created createdAPIKey
	if err := k.call(ctx, siloRequest{method: http.MethodPost, baseURL: baseURL, apiKey: apiKey, path: "/api/v2/admin/api-keys", body: body}, &created); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(created.Key, "sa_") || !validKeyID(created.ID) {
		return &created, errors.New("Silo's answer didn't include a usable key")
	}
	return &created, nil
}

// Delete revokes a key by ID (DELETE /api/v2/admin/api-keys/{id}). Silo
// requires If-Match on this route; "*" means whatever its current revision.
func (k *keyAdmin) Delete(ctx context.Context, baseURL, apiKey, id string) error {
	if !validKeyID(id) {
		return errors.New("invalid API key ID")
	}
	return k.call(ctx, siloRequest{method: http.MethodDelete, baseURL: baseURL, apiKey: apiKey, path: "/api/v2/admin/api-keys/" + id, ifMatch: "*"}, nil)
}

// Probe calls a GET route and reports only whether the key may use it.
func (k *keyAdmin) Probe(ctx context.Context, baseURL, apiKey, path string) error {
	return k.call(ctx, siloRequest{method: http.MethodGet, baseURL: baseURL, apiKey: apiKey, path: path}, nil)
}

func validKeyID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}
