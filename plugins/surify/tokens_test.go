package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func enhancedTestToken() (string, ed25519.PublicKey) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return "gtfya." + base64.RawURLEncoding.EncodeToString(seed), ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}
func verifyEnhancedHeader(t *testing.T, header string, pub ed25519.PublicKey, now time.Time) {
	t.Helper()
	parts := strings.Split(header, ".")
	if len(parts) != 4 || parts[0] != "gtfya" || parts[1] != base64.RawURLEncoding.EncodeToString(pub) {
		t.Fatal("expected signed application token with public key")
	}
	ts, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || ts < now.Unix()-5 || ts > now.Unix()+5 {
		t.Fatal("expected fresh timestamp")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512([]byte("iat=" + parts[2]))
	if err := ed25519.VerifyWithOptions(pub, digest[:], signature, &ed25519.Options{Hash: crypto.SHA512}); err != nil {
		t.Fatalf("Gotify Ed25519ph signature rejected: %v", err)
	}
}
func TestEnhancedApplicationTokenSigning(t *testing.T) {
	token, pub := enhancedTestToken()
	now := time.Unix(1800000000, 0)
	header, err := applicationTokenHeader(token, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(header, strings.TrimPrefix(token, "gtfya.")) {
		t.Fatal("private seed leaked")
	}
	verifyEnhancedHeader(t, header, pub, now)
	next, err := applicationTokenHeader(token, now.Add(time.Second))
	if err != nil || next == header {
		t.Fatal("signature must refresh with the timestamp")
	}
}
func TestEnhancedApplicationTokenValidation(t *testing.T) {
	token, _ := enhancedTestToken()
	for _, good := range []string{token, "Alegacy123", "app_token-123"} {
		if !validApplicationToken(good) {
			t.Fatal("rejected supported token")
		}
	}
	for _, bad := range []string{"", "gtfya.bad", "gtfyc." + strings.TrimPrefix(token, "gtfya."), token + ".1800000000.signature", "token\r\nheader: value", "token with spaces"} {
		if validApplicationToken(bad) {
			t.Fatal("accepted invalid credential")
		}
		if _, err := applicationTokenHeader(bad, time.Now()); err == nil {
			t.Fatal("invalid credential must not be sent")
		}
	}
}
func TestEnhancedTokenThroughProfileAndDelivery(t *testing.T) {
	token, pub := enhancedTestToken()
	received := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			if r.Header.Get("X-Gotify-Key") != "" {
				t.Error("server check leaked credentials")
			}
			fmt.Fprint(w, `{"version":"2.8.0"}`)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/message" {
			t.Error("unexpected delivery endpoint")
		}
		verifyEnhancedHeader(t, r.Header.Get("X-Gotify-Key"), pub, time.Now())
		received = true
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer srv.Close()
	p := testPlugin(t)
	mockAdminHost(t, p)
	s, err := p.applyInput(adminViewerForTest(), siloProfile{ID: "p1"}, nil, profileInput{Enabled: true, ServerURL: srv.URL, AppToken: token})
	if err != nil {
		t.Fatalf("profile rejected enhanced token: %v", err)
	}
	if _, _, err := p.gotify.Validate(context.Background(), s.AppToken, s.ServerURL); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Put(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := (&eventServer{plugin: p}).HandleEvent(context.Background(), notificationEvent("enhanced", "p1")); err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Fatal("notification was not sent")
	}
}
