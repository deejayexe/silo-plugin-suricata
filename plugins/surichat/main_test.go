package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestChatIsolationAndPersistence(t *testing.T) {
	raw, err := assets.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := manifest.Load(raw)
	if err != nil {
		t.Fatal(err)
	}

	if spec.GetPluginId() != "local.surichat" {
		t.Fatalf("unexpected Surichat identity: %q", spec.GetPluginId())
	}
	var reminder *pb.CapabilityDescriptor
	for _, capability := range spec.GetCapabilities() {
		if capability.GetId() == "mail_reminders" {
			reminder = capability
			break
		}
	}
	if trigger := reminder.GetScheduledTask().GetDefaultTrigger(); trigger.GetType() != "interval" || trigger.GetIntervalMs() != 300000 {
		t.Fatalf("unexpected reminder schedule: %v", trigger)
	}

	directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/admin/users" && r.URL.Query().Get("identity") == "lucia":
			w.Write([]byte(`{"items":[{"id":"9","username":"lucia","role":"user","enabled":true},{"id":"12","username":"root","role":"admin","enabled":true}]}`))
		case r.URL.Path == "/api/v2/admin/users" && r.URL.Query().Get("limit") == "100":
			switch r.URL.Query().Get("cursor") {
			case "":
				w.Write([]byte(`{"items":[{"id":"12","username":"root","role":"admin","enabled":true},{"id":"13","username":"disabled","role":"user","enabled":false}],"page":{"next_cursor":"next+page"}}`))
			case "next+page":
				w.Write([]byte(`{"items":[{"id":"9","username":"lucia","email":"private@example.test","role":"user","enabled":true}]}`))
			default:
				w.WriteHeader(400)
			}
		case r.URL.Path == "/api/v2/admin/users/9":
			w.Write([]byte(`{"id":"9","username":"lucia","role":"user","enabled":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer directory.Close()
	cfg := settings{database: filepath.Join(t.TempDir(), "chat.sqlite"), serverURL: directory.URL, apiKey: "test-key"}
	app := &support{config: cfg}
	call := func(method, path, id, role string, query map[string]any, body any, code int) string {
		t.Helper()
		q, err := structpb.NewStruct(query)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		res, err := app.Handle(t.Context(), &pb.HandleHTTPRequest{Method: method, Path: path, Query: q, Body: b, Headers: map[string]string{
			"X-Silo-User-Id": id, "X-Silo-User-Role": role, "X-Silo-User-Name": "Cuenta " + id, "Content-Type": "application/json",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != int32(code) {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, res.StatusCode, code, res.Body)
		}
		return string(res.Body)
	}
	token := func(id, role string) string {
		t.Helper()
		var v struct{ Token string }
		if err := json.Unmarshal([]byte(call("GET", "/state", id, role, nil, nil, 200)), &v); err != nil || v.Token == "" {
			t.Fatal("token ausente", err)
		}
		return v.Token
	}
	call("GET", "/state", "", "user", nil, nil, 401)
	userToken := token("3", "user")
	adminToken := token("2", "admin")
	call("GET", "/find", "3", "user", map[string]any{"identity": "lucia"}, nil, 403)
	call("GET", "/directory", "3", "user", nil, nil, 403)
	first := call("GET", "/directory", "2", "admin", nil, nil, 200)
	if !strings.Contains(first, `"items":[]`) || !strings.Contains(first, `"next_cursor":"next+page"`) {
		t.Fatal(first)
	}
	second := call("GET", "/directory", "2", "admin", map[string]any{"cursor": "next+page"}, nil, 200)
	if !strings.Contains(second, "lucia") || strings.Contains(second, "private@example.test") {
		t.Fatal(second)
	}
	call("GET", "/state", "3", "user", map[string]any{"user": "9"}, nil, 403)
	call("POST", "/send", "3", "user", nil, map[string]string{"body": "falso", "user": "9", "token": userToken}, 403)
	call("POST", "/send", "3", "user", nil, map[string]string{"body": "falso", "token": adminToken}, 403)
	call("POST", "/send", "3", "user", nil, map[string]string{"body": "Mi consulta", "token": userToken}, 201)
	filtered := call("GET", "/find", "2", "admin", map[string]any{"identity": "lucia"}, nil, 200)
	if !strings.Contains(filtered, "lucia") || strings.Contains(filtered, "root") {
		t.Fatalf("cuentas incorrectas: %s", filtered)
	}
	call("POST", "/send", "2", "admin", nil, map[string]string{"body": "Respuesta", "user": "3", "token": adminToken}, 201)
	call("POST", "/send", "2", "admin", nil, map[string]string{"body": "Primer contacto", "user": "9", "token": adminToken}, 201)
	call("POST", "/send", "2", "admin", nil, map[string]string{"body": "Cuenta inexistente", "user": "15", "token": adminToken}, 400)
	unread := func(id, role string, want int) {
		t.Helper()
		var data struct{ Unread int }
		err := json.Unmarshal([]byte(call("GET", "/state", id, role, nil, nil, 200)), &data)
		if err != nil || data.Unread != want {
			t.Fatalf("unread for %s: got %d, want %d (%v)", id, data.Unread, want, err)
		}
	}
	unread("2", "admin", 1)
	unread("3", "user", 1)
	call("POST", "/read", "3", "user", nil, map[string]any{"user": "9", "token": userToken, "through": 3}, 403)
	call("POST", "/read", "3", "user", nil, map[string]any{"token": adminToken, "through": 2}, 403)
	call("POST", "/read", "3", "user", nil, map[string]any{"token": userToken, "through": 3}, 400)
	call("POST", "/read", "2", "admin", nil, map[string]any{"user": "3", "token": adminToken, "through": 1}, 200)
	unread("2", "admin", 0)
	unread("7", "admin", 1)
	call("POST", "/read", "3", "user", nil, map[string]any{"token": userToken, "through": 2}, 200)
	call("POST", "/read", "3", "user", nil, map[string]any{"token": userToken, "through": 1}, 200)
	unread("3", "user", 0)
	if err := app.db.Close(); err != nil {
		t.Fatal(err)
	}
	app = &support{config: cfg}
	defer func() {
		if app.db != nil {
			app.db.Close()
		}
	}()
	unread("3", "user", 0)
	unread("2", "admin", 0)
	history := call("GET", "/state", "3", "user", nil, nil, 200)
	if !strings.Contains(history, "Mi consulta") || !strings.Contains(history, "Respuesta") || strings.Contains(history, "Primer contacto") || strings.Contains(history, "lucia") {
		t.Fatalf("historial incorrecto: %s", history)
	}
	adminHistory := call("GET", "/state", "2", "admin", map[string]any{"user": "9"}, nil, 200)
	if !strings.Contains(adminHistory, "Primer contacto") || strings.Contains(adminHistory, "Respuesta") {
		t.Fatalf("historial de administración incorrecto: %s", adminHistory)
	}
}
