package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

func adminHeaders() map[string]string {
	return map[string]string{"X-Silo-User-Id": "7", "X-Silo-User-Role": "admin", "X-Silo-Profile-Id": "p1", "X-Silo-Profile-Primary": "true", "X-Silo-Profile-Name": "Administrator", "Content-Type": "application/json"}
}
func adminViewerForTest() *viewer {
	return &viewer{userID: 7, activeID: "p1", admin: true, primary: true}
}

// mode: 0 admin, 1 ordinary user, 2 disabled admin, 3 deleted, 4 offline, 5 revoked key.
func mockAdminHost(t *testing.T, p *Plugin) *atomic.Int32 {
	t.Helper()
	mode := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/admin/users/7" || r.Header.Get("Authorization") != "Bearer monitor" {
			t.Errorf("unexpected role lookup: %s", r.URL.Path)
			w.WriteHeader(403)
			return
		}
		switch mode.Load() {
		case 3:
			w.WriteHeader(404)
			return
		case 4:
			w.WriteHeader(503)
			return
		case 5:
			w.WriteHeader(401)
			return
		}
		role := "admin"
		if mode.Load() == 1 {
			role = "user"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "7", "username": "admin", "role": role, "enabled": mode.Load() != 2})
	}))
	t.Cleanup(srv.Close)
	p.setConfig(adminConfig{MonitorAPIKey: "monitor"})
	p.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
		return &runtimehost.HostInfo{InternalBaseURL: srv.URL}, nil
	}
	return mode
}
func TestAdminOnlyEveryRouteAndMethod(t *testing.T) {
	p := testPlugin(t)
	routes := &routesServer{plugin: p}
	paths := []string{"/", "/assets/surify.png", "/assets/surify.webp", "/index.html", "/admin/sessions", "/admin/sessions/any", "/api/state", "/api/openapi.json", "/api/profiles/p1", "/api/profiles/p1/validate", "/api/profiles/p1/test", "/api/admin/alerts", "/api/admin/alerts/test", "/api/admin/users", "/api/admin/key-setup", "/api/admin/sessions", "/api/admin/sessions/session/stop"}
	for _, identity := range []string{"anonymous", "ordinary", "secondary", "bad-id"} {
		for _, path := range paths {
			for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
				t.Run(identity+method+path, func(t *testing.T) {
					headers := adminHeaders()
					want := 403
					switch identity {
					case "anonymous":
						delete(headers, "X-Silo-User-Id")
						want = 401
					case "ordinary":
						headers["X-Silo-User-Role"] = "user"
					case "secondary":
						headers["X-Silo-Profile-Primary"] = "false"
					case "bad-id":
						headers["X-Silo-User-Id"] = "not-an-id"
						want = 401
					}
					req := &pluginv1.HandleHTTPRequest{Path: path, Method: method, Headers: headers, Body: []byte(`{"admin":true,"role":"admin"}`)}
					res, err := routes.Handle(context.Background(), req)
					if err != nil || int(res.GetStatusCode()) != want {
						t.Fatalf("status=%d error=%v, want %d", res.GetStatusCode(), err, want)
					}
					if strings.Contains(string(res.GetBody()), "<html") {
						t.Fatal("page leaked")
					}
					if strings.HasPrefix(path, "/api/") && int(p.serveAPI(context.Background(), req).GetStatusCode()) != want {
						t.Fatal("direct API dispatch bypassed authorization")
					}
				})
			}
		}
	}
}
func TestAdministratorCanOnlyManageOwnConnection(t *testing.T) {
	p := testPlugin(t)
	routes := &routesServer{plugin: p}
	ctx := context.Background()
	if err := p.store.Put(ctx, &ProfileSettings{ProfileID: "child", UserID: 7, ServerURL: "https://private.example", AppToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/assets/surify.png", "/assets/surify.webp", "/api/openapi.json", "/api/state", "/api/profiles/p1"} {
		res, err := routes.Handle(ctx, &pluginv1.HandleHTTPRequest{Path: path, Method: "GET", Headers: adminHeaders()})
		if err != nil || res.GetStatusCode() != 200 {
			t.Fatalf("%s: %d %v", path, res.GetStatusCode(), err)
		}
		if path == "/api/state" {
			var s struct {
				Profiles []profileSummary `json:"profiles"`
			}
			_ = json.Unmarshal(res.GetBody(), &s)
			if len(s.Profiles) != 1 || s.Profiles[0].ID != "p1" {
				t.Fatal("household profiles must not be exposed")
			}
		}
	}
	for _, id := range []string{"child", "other-admin"} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			res, _ := routes.Handle(ctx, &pluginv1.HandleHTTPRequest{Path: "/api/profiles/" + id, Method: method, Headers: adminHeaders(), Body: []byte(`{}`)})
			if res.GetStatusCode() != 404 {
				t.Fatal("cannot manage another connection")
			}
		}
	}
	if _, err := p.applyInput(&viewer{userID: 7}, siloProfile{ID: "p1"}, nil, profileInput{}); err == nil {
		t.Fatal("only admin input can activate a destination")
	}
	s, err := p.applyInput(adminViewerForTest(), siloProfile{ID: "p1"}, nil, profileInput{ServerURL: "https://gotify.example", AppToken: "app", Enabled: true})
	if err != nil || !s.AdminManaged {
		t.Fatal("administrator save must activate own destination")
	}
}
func TestAdminDestinationRoleChangesAndLegacyState(t *testing.T) {
	p := testPlugin(t)
	mode := mockAdminHost(t, p)
	ctx := context.Background()
	s := &ProfileSettings{ProfileID: "p1", UserID: 7, AdminManaged: true}
	// Prime the old cache, then ensure authorization ignores it on demotion.
	base, _ := p.siloBase(ctx)
	if _, err := p.silo.User(ctx, base, "monitor", 7); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode         int32
		allow, retry bool
	}{{0, true, false}, {1, false, false}, {2, false, false}, {3, false, false}, {4, false, true}, {5, false, false}} {
		mode.Store(c.mode)
		allowed, err := p.adminDestinationAllowed(ctx, s)
		if allowed != c.allow || (err != nil) != c.retry {
			t.Fatalf("mode %d: allowed=%v error=%v", c.mode, allowed, err)
		}
	}
	mode.Store(0)
	s.AdminManaged = false
	if allowed, err := p.adminDestinationAllowed(ctx, s); allowed || err != nil {
		t.Fatal("legacy connection must stay inactive")
	}
	s.AdminManaged = true
	p.setConfig(adminConfig{})
	if allowed, err := p.adminDestinationAllowed(ctx, s); allowed || err != nil {
		t.Fatal("no delivery without role verification key")
	}
}
func TestDemotedAdminStopsInboxDeliveryImmediately(t *testing.T) {
	p := testPlugin(t)
	mode := mockAdminHost(t, p)
	var sent atomic.Int32
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); fmt.Fprint(w, `{"id":1}`) }))
	defer push.Close()
	ctx := context.Background()
	s := &ProfileSettings{ProfileID: "p1", UserID: 7, AdminManaged: true, Enabled: true, ServerURL: push.URL, AppToken: "app"}
	if err := p.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	event := &eventServer{plugin: p}
	if _, err := event.HandleEvent(ctx, notificationEvent("admin-one", "p1")); err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	if _, err := event.HandleEvent(ctx, notificationEvent("admin-demoted", "p1")); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 1 {
		t.Fatal("demoted administrator received a notification")
	}
}
func TestLegacyAndDemotedPendingGroupsNeverSend(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			p := testPlugin(t)
			mode := mockAdminHost(t, p)
			if !legacy {
				mode.Store(1)
			}
			var sent atomic.Int32
			push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); fmt.Fprint(w, `{"id":1}`) }))
			defer push.Close()
			ctx := context.Background()
			s := &ProfileSettings{ProfileID: "p1", UserID: 7, AdminManaged: !legacy, Enabled: true, ServerURL: push.URL, AppToken: "app"}
			if err := p.store.Put(ctx, s); err != nil {
				t.Fatal(err)
			}
			group := &pendingGroup{ID: "old-group", ProfileID: "p1", Series: "Series", DueAt: time.Now().Add(-time.Minute), Episodes: []pendingEpisode{{DeliveryID: "old"}}}
			raw, _ := json.Marshal(pendingState{Groups: []*pendingGroup{group}})
			if err := p.pending.state.Write(ctx, pendingKey, raw); err != nil {
				t.Fatal(err)
			}
			finished, err := p.flushGroup(ctx, group.ID)
			if err != nil || !finished || sent.Load() != 0 {
				t.Fatalf("finished=%v sent=%d error=%v", finished, sent.Load(), err)
			}
		})
	}
}
func TestAdminAlertsRejectLegacyDestination(t *testing.T) {
	p := testPlugin(t)
	ctx := context.Background()
	if err := p.store.Put(ctx, &ProfileSettings{ProfileID: "p1", UserID: 7, ServerURL: "https://gotify.example", AppToken: "app"}); err != nil {
		t.Fatal(err)
	}
	kept, again, err := p.dropHandled(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []*recipient{{admin: &AdminSettings{UserID: 7, ProfileID: "p1"}}})
	if err != nil || again || len(kept) != 0 {
		t.Fatal("legacy connection must not receive scheduled or server alerts")
	}
}
func TestManifestAllRoutesAdminOnly(t *testing.T) {
	var m struct {
		HTTPRoutes []struct {
			Access         string `json:"access"`
			Navigable      bool   `json:"navigable"`
			NavigationKind string `json:"navigation_kind"`
		} `json:"http_routes"`
	}
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	for _, r := range m.HTTPRoutes {
		if r.Access != "admin" || r.Navigable && r.NavigationKind != "admin" {
			t.Fatal("route is visible or accessible to ordinary users")
		}
	}
}

func TestRoleLookupOutageKeepsDeliveryRetryable(t *testing.T) {
	p := testPlugin(t)
	mode := mockAdminHost(t, p)
	var sent atomic.Int32
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); fmt.Fprint(w, `{"id":1}`) }))
	defer push.Close()
	ctx := context.Background()
	if err := p.store.Put(ctx, &ProfileSettings{ProfileID: "p1", UserID: 7, AdminManaged: true, Enabled: true, ServerURL: push.URL, AppToken: "app"}); err != nil {
		t.Fatal(err)
	}
	events := &eventServer{plugin: p}
	request := notificationEvent("role-outage", "p1")
	mode.Store(4)
	if _, err := events.HandleEvent(ctx, request); err == nil || sent.Load() != 0 {
		t.Fatal("lookup outage must defer delivery")
	}
	mode.Store(0)
	if _, err := events.HandleEvent(ctx, request); err != nil || sent.Load() != 1 {
		t.Fatal("deferred event must deliver after recovery")
	}
}

func TestSurifyLogoEmbeddedAndAdminProtected(t *testing.T) {
	p := testPlugin(t)
	res, err := (&routesServer{plugin: p}).Handle(context.Background(), &pluginv1.HandleHTTPRequest{Path: "/assets/surify.webp", Method: "GET", Headers: adminHeaders()})
	if err != nil || res.GetStatusCode() != 200 || res.GetHeaders()["Content-Type"] != "image/webp" || len(res.GetBody()) != len(surifyLogo) || !strings.HasPrefix(string(res.GetBody()), "RIFF") {
		t.Fatal("logo must be embedded in the plugin and served as WebP")
	}
}
