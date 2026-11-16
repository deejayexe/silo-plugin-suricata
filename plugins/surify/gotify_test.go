package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/types/known/structpb"
)

var testNow = time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)

type memoryState struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (m *memoryState) Read(_ context.Context, k string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[k]
	return append([]byte(nil), v...), ok, nil
}
func (m *memoryState) Write(_ context.Context, k string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[k] = append([]byte(nil), v...)
	return nil
}
func testPlugin(t *testing.T) *Plugin {
	t.Helper()
	return newPlugin(slog.New(slog.NewTextHandler(io.Discard, nil)), &memoryState{values: map[string][]byte{}})
}

type receivedMessage struct {
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Priority int            `json:"priority"`
	Extras   map[string]any `json:"extras"`
}

func TestGotifyDelivery(t *testing.T) {
	var got receivedMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/prefix/message" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Gotify-Key") != "APP_secret" || r.URL.RawQuery != "" {
			t.Error("token must only be sent in header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected JSON")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer srv.Close()
	err := newGotifyClient().Send(context.Background(), gotifyMessage{User: srv.URL + "/prefix/", Token: "APP_secret", Title: "New movie 🎬", Message: "Hello *cinema*", Priority: 2, URL: "https://silo.example/item/1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "New movie 🎬" || got.Priority != 10 || !strings.Contains(got.Message, `Hello \*cinema\*`) || !strings.Contains(got.Message, "[Open in Silo](https://silo.example/item/1)") {
		t.Fatalf("unexpected message: %+v", got)
	}
	display, ok := got.Extras["client::display"].(map[string]any)
	if !ok || display["contentType"] != "text/markdown" {
		t.Fatalf("extras: %v", got.Extras)
	}
}
func TestGotifyPriorityMapping(t *testing.T) {
	for level, want := range map[int]int{-2: 0, -1: 2, 0: 5, 1: 8, 2: 10} {
		if gotifyPriority(level) != want {
			t.Errorf("level %d", level)
		}
	}
}
func TestGotifyErrorClassificationAndRedaction(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, "APP_secret") }))
			defer srv.Close()
			err := newGotifyClient().Send(context.Background(), gotifyMessage{User: srv.URL, Token: "APP_secret", Message: "x"})
			var ge *gotifyError
			if !errors.As(err, &ge) || ge.Retryable != (status >= 500 || status == 429 || status == 408) {
				t.Fatalf("classification: %v", err)
			}
			if strings.Contains(err.Error(), "APP_secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}
func TestGotifyRejectsRedirects(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; fmt.Fprint(w, `{"id":1}`) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer srv.Close()
	if newGotifyClient().Send(context.Background(), gotifyMessage{User: srv.URL, Token: "secret", Message: "x"}) == nil || called {
		t.Fatal("must not forward credentials on redirects")
	}
}
func TestGotifyValidateOnlyChecksServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" || r.Method != "GET" || r.Header.Get("X-Gotify-Key") != "" {
			t.Error("server check must not send a message or token")
		}
		fmt.Fprint(w, `{"version":"2.7.3"}`)
	}))
	defer srv.Close()
	if _, _, err := newGotifyClient().Validate(context.Background(), "app_token", srv.URL); err != nil {
		t.Fatal(err)
	}
}
func TestGotifyRequiresDeliveryAcknowledgement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) }))
	defer srv.Close()
	var ge *gotifyError
	if err := newGotifyClient().Send(context.Background(), gotifyMessage{User: srv.URL, Token: "app_token", Message: "x"}); !errors.As(err, &ge) || !ge.Retryable {
		t.Fatalf("unconfirmed delivery: %v", err)
	}
}
func TestServerURLValidation(t *testing.T) {
	for _, s := range []string{"", "ftp://server", "https://user:password@server", "https://server?token=secret", "https://server#fragment", "https://server?"} {
		if validServerURL(s) {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{"http://localhost:8080", "https://gotify.example/prefix/"} {
		if !validServerURL(s) {
			t.Errorf("rejected %q", s)
		}
	}
}
func TestSharedTokenBoundToServer(t *testing.T) {
	cfg := adminConfig{DefaultServerURL: "https://trusted.example/gotify", DefaultAppToken: "shared_secret"}
	if effectiveToken(&ProfileSettings{ServerURL: "https://attacker.example"}, cfg) != "" {
		t.Fatal("shared token must not go to another server")
	}
	if effectiveToken(&ProfileSettings{ServerURL: "https://trusted.example/gotify/"}, cfg) != "shared_secret" {
		t.Fatal("expected trusted default")
	}
	if effectiveToken(&ProfileSettings{ServerURL: "https://other.example", AppToken: "own"}, cfg) != "own" {
		t.Fatal("expected profile token")
	}
}
func notificationEvent(id, profile string) *pluginv1.HandleEventRequest {
	p, _ := structpb.NewStruct(map[string]any{"id": id, "type": "episode.available", "profile_id": profile, "series_id": "s1", "episode_id": "e1", "series_title": "Severance", "episode_title": "Hello", "season_number": 2, "episode_number": 1, "created_at": "2026-09-23T12:00:00Z"})
	return &pluginv1.HandleEventRequest{EventName: eventNotificationCreated, EventId: id, Payload: p}
}
func TestSiloEventProfileRoutingDedupeAndRetry(t *testing.T) {
	p := testPlugin(t)
	mockAdminHost(t, p)
	ctx := context.Background()
	var messages []receivedMessage
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		var m receivedMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		messages = append(messages, m)
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer srv.Close()
	if err := p.store.Put(ctx, &ProfileSettings{ProfileID: "p1", UserID: 7, AdminManaged: true, ProfileName: "Test", Enabled: true, ServerURL: srv.URL, AppToken: "own_token", Priority: -1}); err != nil {
		t.Fatal(err)
	}
	events := &eventServer{plugin: p}
	for _, e := range []*pluginv1.HandleEventRequest{notificationEvent("one", "p1"), notificationEvent("one", "p1"), notificationEvent("other", "p2")} {
		if _, err := events.HandleEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 1 || messages[0].Title != "New episode: Severance" || messages[0].Message != "S02E01 · Hello" || messages[0].Priority != 2 {
		t.Fatalf("delivery: %+v", messages)
	}
	// A second process reads the same persisted event log.
	other := testPlugin(t)
	mockAdminHost(t, other)
	other.store = p.store
	other.handled = newEventLog(p.store.state)
	if _, err := (&eventServer{plugin: other}).HandleEvent(ctx, notificationEvent("one", "p1")); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatal("dedupe must survive a process restart")
	}
	status = 503
	if _, err := events.HandleEvent(ctx, notificationEvent("retry", "p1")); err == nil {
		t.Fatal("503 must ask Silo to retry")
	}
	status = 200
	if _, err := events.HandleEvent(ctx, notificationEvent("retry", "p1")); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatal("failed event must be delivered on retry")
	}
	status = 401
	if _, err := events.HandleEvent(ctx, notificationEvent("permanent", "p1")); err != nil {
		t.Fatal("invalid token should be permanently acknowledged")
	}
	status = 200
	_, _ = events.HandleEvent(ctx, notificationEvent("permanent", "p1"))
	if len(messages) != 2 {
		t.Fatal("permanently rejected event should stay acknowledged")
	}
}
func TestGotifyProfileSaveAndSecretMasking(t *testing.T) {
	p := testPlugin(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":"2.7.3"}`) }))
	defer srv.Close()
	s, err := p.applyInput(adminViewerForTest(), siloProfile{ID: "p1", Name: "Test"}, nil, profileInput{Enabled: true, ServerURL: srv.URL, AppToken: "secret_token", IncludeLink: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(newProfileView(siloProfile{ID: "p1"}, s))
	if strings.Contains(string(raw), "secret_token") {
		t.Fatal("token leaked in profile view")
	}
	if !strings.Contains(string(raw), srv.URL) {
		t.Fatal("server URL should be available to edit")
	}
}
func TestManifestIdentityAndEvents(t *testing.T) {
	m, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetPluginId() != "silo.gotify" || m.GetVersion() != "0.0.1" {
		t.Fatalf("identity %s %s", m.GetPluginId(), m.GetVersion())
	}
	if err = publicmanifest.ValidateCatalogPresentation(m, "https://github.com/deejayexe/silo-plugin-suricata"); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Capabilities []struct {
			Subscriptions []string `json:"subscriptions"`
		} `json:"capabilities"`
	}
	_ = json.Unmarshal(manifestJSON, &raw)
	if len(raw.Capabilities[0].Subscriptions) != 7 {
		t.Fatal("upstream event subscriptions changed")
	}
}
func TestGotifyTimeoutIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var ge *gotifyError
	if err := newGotifyClient().Send(ctx, gotifyMessage{User: srv.URL, Token: "app", Message: "x"}); !errors.As(err, &ge) || !ge.Retryable {
		t.Fatalf("timeout: %v", err)
	}
}

func TestRenderTemplate(t *testing.T) {
	vars := map[string]string{"series": "Severance", "sxe": "S02E01", "episode_title": "", "year": ""}
	cases := []struct{ tmpl, want string }{
		{"{sxe} · {episode_title}", "S02E01"},
		{"New episode: {series}", "New episode: Severance"},
		{"{series} ({year})", "Severance"},
		{"{unknown} stays", "{unknown} stays"},
		{"Line one\n{episode_title}\nLine three", "Line one\nLine three"},
		{"Keep\n\nblank", "Keep\n\nblank"},
	}
	for _, c := range cases {
		if got := renderTemplate(c.tmpl, vars, false); got != c.want {
			t.Errorf("renderTemplate(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
	if got := renderTemplate("{series}", map[string]string{"series": "<b>"}, true); got != "&lt;b&gt;" {
		t.Errorf("HTML mode should escape values, got %q", got)
	}
}

func TestDeliveryVars(t *testing.T) {
	season, episode := 2, 1
	d := delivery{Type: "episode.available", SeriesTitle: "Severance", EpisodeTitle: "Hello", SeasonNumber: &season, EpisodeNumber: &episode,
		ReasonFlags: json.RawMessage(`{"favorite":true,"next_up":true}`)}
	vars := deliveryVars(d, "Josh")
	if vars["sxe"] != "S02E01" || vars["reasons"] != "Favorite, Next Up" || vars["profile"] != "Josh" {
		t.Fatalf("unexpected episode vars: %v", vars)
	}

	d = delivery{Type: "request.declined", ReasonFlags: json.RawMessage(`{"request_id":"r1","title":"Dune","year":2021,"media_type":"movie","reason":"Dupe"}`)}
	vars = deliveryVars(d, "")
	if vars["title_year"] != "Dune (2021)" || vars["media_type"] != "Movie" || vars["decline_reason"] != "Dupe" {
		t.Fatalf("unexpected request vars: %v", vars)
	}

	d = delivery{Type: "request.fulfilled", SeriesTitle: "Dune", ReasonFlags: json.RawMessage(`{"request_id":"r1","tmdb_id":1}`)}
	if got := deliveryVars(d, "")["title_year"]; got != "Dune" {
		t.Fatalf("fulfilled title_year = %q", got)
	}
}

func TestStorePersistsAcrossReload(t *testing.T) {
	state := &memoryState{values: map[string][]byte{}}
	s := newProfileStore(state)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, &ProfileSettings{ProfileID: id, ServerURL: "k" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	reloaded := newProfileStore(state)
	got, found, err := reloaded.Get(ctx, "c")
	if err != nil || !found || got.ServerURL != "kc" {
		t.Fatalf("Get(c) = %v, %v, %v", got, found, err)
	}
	if _, found, _ := reloaded.Get(ctx, "b"); found {
		t.Fatal("deleted profile came back")
	}
}

func TestQuietHoursActive(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, time.UTC) }
	night := QuietHours{Enabled: true, Start: "22:00", End: "07:00"}
	for _, c := range []struct {
		q    QuietHours
		t    time.Time
		want bool
	}{
		{night, at(23, 0), true},
		{night, at(22, 0), true},
		{night, at(3, 30), true},
		{night, at(6, 59), true},
		{night, at(7, 0), false},
		{night, at(12, 0), false},
		{night, at(21, 59), false},
		{QuietHours{Enabled: true, Start: "13:00", End: "14:30"}, at(14, 29), true},
		{QuietHours{Enabled: true, Start: "13:00", End: "14:30"}, at(14, 30), false},
		{QuietHours{Enabled: false, Start: "00:00", End: "23:59"}, at(12, 0), false},
		{QuietHours{Enabled: true, Start: "08:00", End: "08:00"}, at(8, 0), false},
		{QuietHours{Enabled: true, Start: "bad", End: "08:00"}, at(8, 0), false},
	} {
		if got := c.q.active(c.t); got != c.want {
			t.Errorf("%+v at %s: active = %v, want %v", c.q, c.t.Format("15:04"), got, c.want)
		}
	}
}

func TestPriorityAtQuietHours(t *testing.T) {
	a := defaultAdminSettings()
	a.TimeZone = "America/New_York"
	a.Quiet = QuietHours{Enabled: true, Start: "14:00", End: "16:00"}
	// 20:00 UTC is 15:00 in New York: quiet.
	for _, c := range []struct{ in, want int }{{1, -1}, {0, -1}, {-1, -1}, {-2, -2}, {2, 2}} {
		if got := a.priorityAt(nil, c.in, testNow); got != c.want {
			t.Errorf("quiet: priority %d became %d, want %d", c.in, got, c.want)
		}
	}
	a.Quiet.SilenceEmergency = true
	if got := a.priorityAt(nil, 2, testNow); got != -1 {
		t.Errorf("emergency with SilenceEmergency became %d, want -1", got)
	}
	// 21:00 UTC is 16:00 in New York: over. The same instant in UTC is not
	// in a quiet window set for New York's afternoon.
	if got := a.priorityAt(nil, 1, testNow.Add(time.Hour)); got != 1 {
		t.Errorf("after quiet hours priority became %d", got)
	}
	a.TimeZone = "UTC"
	if got := a.priorityAt(nil, 0, testNow); got != 0 {
		t.Errorf("14:00-16:00 must not apply at 20:00 UTC, got %d", got)
	}
	// An unknown zone falls back to UTC rather than failing.
	a.TimeZone = "Mars/Olympus"
	if a.location() != time.UTC {
		t.Errorf("unknown zone location = %v", a.location())
	}
}

// setProfileQuiet gives a saved profile quiet hours.

func TestDigestSlotWindow(t *testing.T) {
	a := defaultAdminSettings()
	a.TimeZone = "America/New_York"
	a.Digest = &DigestSchedule{Day: int(time.Sunday), Hour: 22}
	// Sunday 22:00 New York is Monday 03:00 UTC (EST, UTC-5, after DST ends).
	slot := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	week := "2026-W45" // Sunday 2026-11-08
	for _, c := range []struct {
		at  time.Duration
		due bool
	}{
		{-time.Second, false}, {0, true}, {59 * time.Minute, true}, {3 * time.Hour, true},
		{6 * time.Hour, true}, {6*time.Hour + 59*time.Minute, true}, {7 * time.Hour, false}, {24 * time.Hour, false},
	} {
		got, due := a.digestSlot(slot.Add(c.at))
		if due != c.due || (due && got != week) {
			t.Errorf("%v after the chosen hour: due=%v week=%q, want due=%v week=%q", c.at, due, got, c.due, week)
		}
	}
	// 01:00 Monday in New York is after midnight, in the next ISO week, but the
	// digest is still the Sunday's.
	if got, due := a.digestSlot(time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)); !due || got != week {
		t.Errorf("after midnight: due=%v week=%q", due, got)
	}
}

func TestDigestWeekIsTheAdminsLocalWeek(t *testing.T) {
	a := defaultAdminSettings()
	a.TimeZone = "Pacific/Auckland"
	// Sunday 23:00 UTC is already Monday in Auckland: the next ISO week.
	now := time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)
	if isoWeek(now.In(a.location())) == isoWeek(now) {
		t.Fatal("the local ISO week should differ from UTC's here")
	}
}
