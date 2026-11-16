package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotificationTitleTracksSiloServerSettings(t *testing.T) {
	name := "Home cinema 🍿"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v2/theme/branding" || r.Method != http.MethodGet {
			t.Errorf("unexpected branding request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("public branding request must not receive an administrator credential")
		}
		json.NewEncoder(w).Encode(map[string]string{"server_name": name})
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = old }()
	c := config{SiloURL: server.URL, SiloKey: "private-administrator-key"}
	if got := notificationTitle(context.Background(), c); got != name {
		t.Fatalf("server name was not used: %q", got)
	}
	name = "Renamed cinema"
	if got := notificationTitle(context.Background(), c); got != name {
		t.Fatalf("server rename did not take effect: %q", got)
	}
	c.MessageTitle = "Custom notification title"
	if got := notificationTitle(context.Background(), c); got != c.MessageTitle || requests != 2 {
		t.Fatalf("custom title should avoid a branding request: %q, requests %d", got, requests)
	}
}

func TestNotificationTitleSurvivesUnavailableBranding(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"server_name":"  "}`, "not JSON"} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if body == "" {
					w.WriteHeader(http.StatusNotFound)
				}
				w.Write([]byte(body))
			}))
			defer server.Close()
			old := http.DefaultTransport
			http.DefaultTransport = server.Client().Transport
			defer func() { http.DefaultTransport = old }()
			if got := notificationTitle(context.Background(), config{SiloURL: server.URL}); got != "Silo" {
				t.Fatalf("unexpected fallback: %q", got)
			}
		})
	}
}
