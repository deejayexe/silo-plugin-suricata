package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSiloIdentityRejectsWrongUserRoleOrPrimary(t *testing.T) {
	for _, tc := range []struct{ name, account, profiles, user string }{
		{"wrong-user", `{"id":"1","username":"owner","role":"admin"}`, `{"items":[]}`, "different"},
		{"not-admin", `{"id":"1","username":"owner","role":"user"}`, `{"items":[]}`, ""},
		{"no-primary", `{"id":"1","username":"owner","role":"admin"}`, `{"items":[{"id":"secondary","name":"Child","is_primary":false}]}`, ""},
		{"ambiguous-primary", `{"id":"1","username":"owner","role":"admin"}`, `{"items":[{"id":"one","is_primary":true},{"id":"two","is_primary":true}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/account/me") {
					w.Write([]byte(tc.account))
				} else {
					w.Write([]byte(tc.profiles))
				}
			}))
			defer srv.Close()
			old := http.DefaultTransport
			http.DefaultTransport = srv.Client().Transport
			defer func() { http.DefaultTransport = old }()
			if _, err := resolveSiloIdentity(context.Background(), config{SiloURL: srv.URL, SiloKey: "one-key", SiloUsername: tc.user}); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
