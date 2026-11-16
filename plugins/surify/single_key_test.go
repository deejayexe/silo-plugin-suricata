package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestSingleAdministratorKeyForMonitoringAndControl(t *testing.T) {
	const key = "sa_new_administrator_secret"
	var stopCommands atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("Silo call did not use the single administrator key")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/admin/api-keys":
			w.Write([]byte(`{"items":[]}`))
		case "GET /api/v2/admin/users/7":
			w.Write([]byte(`{"id":"7","username":"admin","role":"admin","enabled":true}`))
		case "POST /api/v2/admin/sessions/session-1/stop":
			stopCommands.Add(1)
			w.Write([]byte(`{"outcome":"applied","delivery":"dispatched"}`))
		default:
			t.Errorf("setup must not create or revoke keys: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	p := testPlugin(t)
	p.configured.Store(true)
	p.setConfig(adminConfig{MonitorAPIKey: "old-monitor", ControlAPIKey: "old-control", DefaultAppToken: "gotify-secret", DefaultServerURL: "https://gotify.example"})
	p.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
		return &runtimehost.HostInfo{InternalBaseURL: server.URL}, nil
	}
	var saved map[string]any
	p.setGlobalConfigFn = func(_ context.Context, name string, value map[string]any) error {
		if name != adminConfigKey {
			t.Fatal("wrong config entry")
		}
		saved = value
		return nil
	}
	body, _ := json.Marshal(map[string]any{"key": key})
	response := p.handleKeySetup(context.Background(), &pb.HandleHTTPRequest{Headers: adminHeaders(), Body: body})
	if response.StatusCode != 200 || strings.Contains(string(response.Body), key) {
		t.Fatalf("unexpected setup result: %s", response.Body)
	}
	if saved[fieldAdminAPIKey] != key || saved[fieldDefaultAppToken] != "gotify-secret" {
		t.Fatal("single key save lost administrator or Gotify configuration")
	}
	for _, obsolete := range []string{fieldMonitorAPIKey, fieldControlAPIKey, legacyAPIKeyField} {
		if _, exists := saved[obsolete]; exists {
			t.Errorf("obsolete credential still stored: %s", obsolete)
		}
	}
	// Read back the persisted configuration as Silo does on a restart.
	value, _ := structpb.NewStruct(saved)
	reloaded := parseAdminConfig([]*pb.ConfigEntry{{Key: adminConfigKey, Value: value}})
	p.setConfig(reloaded)
	if reloaded.monitorKey() != key || reloaded.controlKey() != key {
		t.Fatal("monitoring and control must share the saved key after restart")
	}
	if _, err := p.silo.CurrentUser(context.Background(), server.URL, reloaded.monitorKey(), 7); err != nil {
		t.Fatal(err)
	}
	response = p.handleStopSession(context.Background(), &pb.HandleHTTPRequest{Headers: adminHeaders(), Body: []byte(`{}`)}, "session-1")
	if response.StatusCode != 200 || stopCommands.Load() != 1 {
		t.Fatalf("stream control did not use the single key: %s", response.Body)
	}
	// A blank edit retains the saved key without sending it back to the page.
	response = p.handleKeySetup(context.Background(), &pb.HandleHTTPRequest{Headers: adminHeaders(), Body: []byte(`{}`)})
	if response.StatusCode != 200 || strings.Contains(string(response.Body), key) {
		t.Fatal("blank edit did not preserve the hidden saved key")
	}
}

func TestKeyReplacementFailuresPreservePreviousConfiguration(t *testing.T) {
	for _, mode := range []string{"rejected", "scoped", "disabled", "save-failed"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("key replacement must never mutate Silo's API keys")
				}
				if mode == "rejected" {
					w.WriteHeader(401)
					return
				}
				if mode == "scoped" {
					w.WriteHeader(403)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/users/7") {
					json.NewEncoder(w).Encode(map[string]any{"id": "7", "role": "admin", "enabled": mode != "disabled"})
					return
				}
				w.Write([]byte(`{"items":[]}`))
			}))
			defer server.Close()
			p := testPlugin(t)
			p.configured.Store(true)
			p.setConfig(adminConfig{MonitorAPIKey: "working-monitor", ControlAPIKey: "working-control"})
			p.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
				return &runtimehost.HostInfo{InternalBaseURL: server.URL}, nil
			}
			p.setGlobalConfigFn = func(context.Context, string, map[string]any) error {
				if mode != "save-failed" {
					t.Error("invalid key reached persistence")
				}
				return errors.New("storage unavailable")
			}
			response := p.handleKeySetup(context.Background(), &pb.HandleHTTPRequest{Headers: adminHeaders(), Body: []byte(`{"key":"sa_new_administrator_secret"}`)})
			if response.StatusCode == 200 || p.config().monitorKey() != "working-monitor" || p.config().controlKey() != "working-control" {
				t.Fatal("failed replacement changed the active keys")
			}
			if strings.Contains(string(response.Body), "sa_new_administrator_secret") {
				t.Fatal("error exposed the supplied key")
			}
		})
	}
}

func TestStaleKeyCreationRequestDoesNotRevokeAnything(t *testing.T) {
	p := testPlugin(t)
	p.configured.Store(true)
	p.setConfig(adminConfig{AdminAPIKey: "sa_current_administrator_secret"})
	response := p.handleKeySetup(context.Background(), &pb.HandleHTTPRequest{Headers: adminHeaders(), Body: []byte(`{"key":"sa_new_administrator_secret","revoke":true,"control":true}`)})
	if response.StatusCode != 409 || p.config().AdminAPIKey != "sa_current_administrator_secret" {
		t.Fatal("stale setup must be refused before it can affect saved credentials")
	}
}

func TestAdministratorConfigurationRedactsKeysAndKeepsLegacyFallback(t *testing.T) {
	c := adminConfig{AdminAPIKey: "private-admin", MonitorAPIKey: "private-monitor", ControlAPIKey: "private-control", DefaultAppToken: "private-gotify", legacyAPIKey: "private-legacy"}
	var log strings.Builder
	slog.New(slog.NewTextHandler(&log, nil)).Info("config", "config", c)
	for _, output := range []string{fmt.Sprintf("%v %+v %#v", c, c, c), log.String()} {
		if strings.Contains(output, "private-") {
			t.Fatal("config formatting exposed credentials")
		}
	}
	c.AdminAPIKey = ""
	if c.monitorKey() != "private-monitor" || c.controlKey() != "private-control" {
		t.Fatal("existing scoped installations lost their working credentials")
	}
	if c.storedValue()[fieldControlAPIKey] != "private-control" {
		t.Fatal("compatibility save discarded a working scoped key")
	}
}
