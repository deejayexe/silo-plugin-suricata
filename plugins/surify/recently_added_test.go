package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

type recentFixture struct {
	p        *Plugin
	start    time.Time
	mu       sync.Mutex
	pages    map[string][]catalogAddition
	next     map[string]string
	sent     []receivedMessage
	failPush bool
	role     string
	queries  []string
}

func recentTest(t *testing.T) *recentFixture {
	t.Helper()
	f := &recentFixture{p: testPlugin(t), start: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), pages: map[string][]catalogAddition{}, next: map[string]string{}, role: "admin"}
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failPush {
			w.WriteHeader(503)
			return
		}
		var m receivedMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.sent = append(f.sent, m)
		fmt.Fprint(w, `{"id":1}`)
	}))
	t.Cleanup(push.Close)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer monitor" {
			t.Error("wrong scoped key")
		}
		switch r.URL.Path {
		case "/api/v2/admin/users/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "7", "role": f.role, "enabled": true})
		case "/api/v2/admin/catalog/items":
			if r.URL.Query().Get("added_after") == "" || r.URL.Query().Get("limit") != "20" {
				t.Error("catalog must be bounded and time-filtered")
			}
			cursor := r.URL.Query().Get("cursor")
			f.queries = append(f.queries, cursor)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": f.pages[cursor], "page": map[string]any{"has_more": f.next[cursor] != "", "next_cursor": f.next[cursor]}})
		default:
			t.Errorf("unexpected API route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(host.Close)
	f.p.setConfig(adminConfig{MonitorAPIKey: "monitor"})
	f.p.catalogMediaFn = nil
	f.p.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
		return &runtimehost.HostInfo{InternalBaseURL: host.URL, PublicBaseURL: "https://silo.example.com"}, nil
	}
	if err := f.p.store.Put(context.Background(), &ProfileSettings{ProfileID: "p1", UserID: 7, AdminManaged: true, Enabled: true, ServerURL: push.URL, AppToken: "app"}); err != nil {
		t.Fatal(err)
	}
	a := defaultAdminSettings()
	a.UserID = 7
	a.ProfileID = "p1"
	a.RecentSince = f.start
	setting := a.alert(alertRecentlyAdded)
	setting.Enabled = true
	a.Alerts[alertRecentlyAdded] = setting
	if err := f.p.admins.Put(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *recentFixture) item(id string, minutes int) catalogAddition {
	return catalogAddition{ContentID: id, Type: "movie", Title: "Movie " + id, Year: 2026, AddedAt: f.start.Add(time.Duration(minutes) * time.Minute), LibraryIDs: []string{"1"}}
}
func TestRecentlyAddedNoBacklogDedupePagingAndRestart(t *testing.T) {
	f := recentTest(t)
	ctx := context.Background()
	f.pages[""] = []catalogAddition{f.item("old", -60), f.item("one", 1)}
	f.pages["next"] = []catalogAddition{f.item("two", 2)}
	f.next[""] = "next"
	now := f.start.Add(10 * time.Minute)
	if _, err := f.p.runRecentlyAdded(ctx, now); err != nil {
		t.Fatal(err)
	}
	st, _ := f.p.readRecent(ctx)
	if st.Admins["7"].Cursor != "next" || !st.Admins["7"].Until.Equal(now) {
		t.Fatal("must persist the current cursor window")
	}
	// A restarted plugin shares instance state, not in-memory progress.
	other := testPlugin(t)
	other.store = f.p.store
	other.admins = f.p.admins
	other.handled = newEventLog(f.p.store.state)
	other.hostInfoFn = f.p.hostInfoFn
	other.setConfig(f.p.config())
	other.catalogMediaFn = nil
	if _, err := other.runRecentlyAdded(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, _ = other.readRecent(ctx)
	if st.Admins["7"].Cursor != "" || !st.Admins["7"].Since.Equal(now) {
		t.Fatal("watermark must advance only after the last page")
	}
	f.mu.Lock()
	f.next[""] = ""
	f.pages[""] = []catalogAddition{f.item("one", 1), f.item("two", 2)}
	f.mu.Unlock()
	if _, err := other.runRecentlyAdded(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 2 || !strings.Contains(f.sent[0].Title, "Movie one") || !strings.Contains(f.sent[1].Title, "Movie two") {
		t.Fatalf("sent: %+v", f.sent)
	}
	if strings.Join(f.queries, ",") != ",next," {
		t.Fatal("cursor must resume across restart")
	}
}
func TestRecentlyAddedRetryDoesNotAdvanceWatermark(t *testing.T) {
	f := recentTest(t)
	f.pages[""] = []catalogAddition{f.item("one", 1)}
	f.failPush = true
	ctx := context.Background()
	now := f.start.Add(10 * time.Minute)
	if _, err := f.p.runRecentlyAdded(ctx, now); err == nil {
		t.Fatal("temporary send failure must retry")
	}
	st, _ := f.p.readRecent(ctx)
	if !st.Admins["7"].Since.IsZero() {
		t.Fatal("failed page must not commit progress")
	}
	f.mu.Lock()
	f.failPush = false
	f.mu.Unlock()
	if _, err := f.p.runRecentlyAdded(ctx, now); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 {
		t.Fatal("retry should deliver once")
	}
}
func TestRecentlyAddedRespectsAdminRoleAndFilters(t *testing.T) {
	f := recentTest(t)
	ctx := context.Background()
	f.pages[""] = []catalogAddition{f.item("one", 1)}
	f.role = "user"
	if _, err := f.p.runRecentlyAdded(ctx, f.start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.role = "admin"
	f.mu.Unlock()
	a, _, _ := f.p.admins.Get(ctx, 7)
	a.Filters.Libraries = []string{"2"}
	if err := f.p.admins.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.runRecentlyAdded(ctx, f.start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 0 {
		t.Fatal("must not send to non-admins or for excluded libraries")
	}
}
func TestRecentlyAddedArtworkAndMetadataFromSiloHost(t *testing.T) {
	f := recentTest(t)
	item := f.item("one", 1)
	score := 8.2
	item.Genres = []string{"Science fiction"}
	item.RatingTMDB = &score
	f.pages[""] = []catalogAddition{item}
	f.p.catalogMediaFn = func(ctx context.Context, req runtimehost.ListLibraryMediaRequest) (*runtimehost.ListLibraryMediaResponse, error) {
		return &runtimehost.ListLibraryMediaResponse{Items: []runtimehost.CatalogMediaItem{{MediaID: "one", Overview: "Synopsis from Silo", PosterURL: "https://images.example/poster.jpg"}}}, nil
	}
	if _, err := f.p.runRecentlyAdded(context.Background(), f.start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || !strings.Contains(f.sent[0].Message, "Synopsis from Silo") || !strings.Contains(f.sent[0].Message, "TMDB 8.2 / 10") || !strings.Contains(f.sent[0].Message, "Science fiction") || !strings.Contains(f.sent[0].Message, "![Silo poster](https://images.example/poster.jpg)") {
		t.Fatalf("message: %+v", f.sent)
	}
	notification := f.sent[0].Extras["client::notification"].(map[string]any)
	if notification["bigImageUrl"] != "https://images.example/poster.jpg" {
		t.Fatal("Android image must use the Silo URL")
	}
	if !strings.Contains(f.sent[0].Message, "https://silo.example.com/item/one") {
		t.Fatal("content link missing")
	}
}
func TestRecentlyAddedMissingOptionalHostAPIStillDelivers(t *testing.T) {
	f := recentTest(t)
	f.pages[""] = []catalogAddition{f.item("one", 1)}
	f.p.catalogMediaFn = func(context.Context, runtimehost.ListLibraryMediaRequest) (*runtimehost.ListLibraryMediaResponse, error) {
		return nil, errors.New("unimplemented")
	}
	if _, err := f.p.runRecentlyAdded(context.Background(), f.start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 {
		t.Fatal("missing artwork must not block the notification")
	}
}
func TestNavigationUsesNativeSiloAdministration(t *testing.T) {
	p := testPlugin(t)
	r := &routesServer{plugin: p}
	res, _ := r.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Path: "/admin/sessions", Method: "GET", Headers: adminHeaders()})
	if res.GetStatusCode() != 303 || res.GetHeaders()["Location"] != "/admin/activity" {
		t.Fatal("old stream links must redirect to native activity")
	}
	if !strings.Contains(string(indexHTML), `class="back" href="/admin"`) || strings.Contains(string(indexHTML), `href="admin/sessions"`) {
		t.Fatal("navigation must use native admin routes")
	}
	if len(surifyLogo) > 40000 {
		t.Fatal("optimized logo unexpectedly large")
	}
}
func TestActivationTimestampSurvivesUnrelatedAdminSaves(t *testing.T) {
	f := recentTest(t)
	ctx := context.Background()
	f.p.taskNow = func() time.Time { return f.start.Add(time.Hour) }
	a, _, _ := f.p.admins.Get(ctx, 7)
	body, _ := json.Marshal(adminInput{Alerts: a.Alerts, Filters: a.Filters, Noise: a.Noise, Limits: a.Limits, TimeZone: "UTC"})
	res := f.p.handlePutAdminAlerts(ctx, &adminViewer{userID: 7, profileID: "p1"}, body)
	if res.GetStatusCode() != 200 {
		t.Fatalf("save: %s", res.GetBody())
	}
	saved, _, _ := f.p.admins.Get(ctx, 7)
	if !saved.RecentSince.Equal(f.start) {
		t.Fatal("unrelated changes must not reset enrollment")
	}
}

func TestRecentlyAddedReactivationStartsFresh(t *testing.T) {
	f := recentTest(t)
	ctx := context.Background()
	f.p.taskNow = func() time.Time { return f.start.Add(time.Hour) }
	a, _, _ := f.p.admins.Get(ctx, 7)
	off := a.alert(alertRecentlyAdded)
	off.Enabled = false
	a.Alerts[alertRecentlyAdded] = off
	body, _ := json.Marshal(adminInput{Alerts: a.Alerts, Filters: a.Filters, Noise: a.Noise, Limits: a.Limits})
	if res := f.p.handlePutAdminAlerts(ctx, &adminViewer{userID: 7, profileID: "p1"}, body); res.GetStatusCode() != 200 {
		t.Fatal(string(res.GetBody()))
	}
	off.Enabled = true
	a.Alerts[alertRecentlyAdded] = off
	body, _ = json.Marshal(adminInput{Alerts: a.Alerts, Filters: a.Filters, Noise: a.Noise, Limits: a.Limits})
	if res := f.p.handlePutAdminAlerts(ctx, &adminViewer{userID: 7, profileID: "p1"}, body); res.GetStatusCode() != 200 {
		t.Fatal(string(res.GetBody()))
	}
	a, _, _ = f.p.admins.Get(ctx, 7)
	if !a.RecentSince.Equal(f.start.Add(time.Hour)) {
		t.Fatal("reactivation must reset its start time")
	}
	f.pages[""] = []catalogAddition{f.item("while-disabled", 30), f.item("new", 65)}
	if _, err := f.p.runRecentlyAdded(ctx, f.start.Add(70*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || !strings.Contains(f.sent[0].Title, "Movie new") {
		t.Fatal("disabled interval must not replay old content")
	}
}
func TestImageURLsAreSafeAndSignedURLsRemainIntact(t *testing.T) {
	f := recentTest(t)
	item := f.item("one", 1)
	item.PosterURL = "javascript:alert(1)"
	f.pages[""] = []catalogAddition{item}
	if _, err := f.p.runRecentlyAdded(context.Background(), f.start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if strings.Contains(f.sent[0].Message, "javascript:") {
		t.Fatal("unsafe image URL leaked")
	}
	f.mu.Unlock()
	signed := "https://cdn.example/image.jpg?signature=example&expires=1800000000"
	f.p.catalogMediaFn = func(context.Context, runtimehost.ListLibraryMediaRequest) (*runtimehost.ListLibraryMediaResponse, error) {
		return &runtimehost.ListLibraryMediaResponse{Items: []runtimehost.CatalogMediaItem{{MediaID: "two", PosterURL: signed}}}, nil
	}
	enriched := f.p.enrichAddition(context.Background(), f.item("two", 2))
	if enriched.PosterURL != signed {
		t.Fatal("valid signed artwork URLs must be preserved")
	}
}
