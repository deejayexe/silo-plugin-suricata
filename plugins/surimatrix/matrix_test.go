package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// A local homeserver contract test exercises real Olm/Megolm and SQLite. It
// does not model federation, recipient devices or Element's rendering.
func TestEncryptedSendAndCryptoStoreReopen(t *testing.T) {
	var mu sync.Mutex
	var ownKeys any
	var uploaded []byte
	var wire json.RawMessage
	var txns []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/account/whoami"):
			io.WriteString(w, `{"user_id":"@bot:test","device_id":"SURIMATRIX"}`)
		case strings.HasSuffix(r.URL.Path, "/keys/query"):
			devices := map[string]any{}
			if ownKeys != nil {
				devices["SURIMATRIX"] = ownKeys
			}
			json.NewEncoder(w).Encode(map[string]any{"device_keys": map[string]any{"@bot:test": devices}})
		case strings.HasSuffix(r.URL.Path, "/keys/claim"):
			io.WriteString(w, `{"one_time_keys":{}}`)
		case strings.Contains(r.URL.Path, "/sendToDevice/"):
			io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/keys/upload"):
			var data map[string]any
			json.NewDecoder(r.Body).Decode(&data)
			if k := data["device_keys"]; k != nil {
				ownKeys = k
			}
			io.WriteString(w, `{"one_time_key_counts":{"signed_curve25519":50}}`)
		case strings.HasSuffix(r.URL.Path, "/sync"):
			io.WriteString(w, `{"next_batch":"next","device_one_time_keys_count":{"signed_curve25519":50},"rooms":{"join":{}}}`)
		case strings.HasSuffix(r.URL.Path, "/state"):
			io.WriteString(w, `[{"type":"m.room.encryption","state_key":"","content":{"algorithm":"m.megolm.v1.aes-sha2"}},{"type":"m.room.member","state_key":"@bot:test","sender":"@bot:test","content":{"membership":"join"}}]`)
		case strings.HasSuffix(r.URL.Path, "/state/m.room.encryption/"):
			io.WriteString(w, `{"algorithm":"m.megolm.v1.aes-sha2"}`)
		case strings.HasSuffix(r.URL.Path, "/upload"):
			uploaded, _ = io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Error("upload MIME leaks image type")
			}
			io.WriteString(w, `{"content_uri":"mxc://test/poster"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.encrypted/"):
			wire, _ = io.ReadAll(r.Body)
			txns = append(txns, r.URL.Path)
			io.WriteString(w, `{"event_id":"$sent"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND"}`)
		}
	}))
	defer server.Close()
	// Trust only this test server's certificate without disabling TLS checks.
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	c := config{Homeserver: server.URL, UserID: "@bot:test", DeviceID: "SURIMATRIX", AccessToken: "test-token", RoomID: "!room:test", StateDir: t.TempDir()}
	var fingerprint string
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := withMatrix(ctx, c, func(m *matrixClient) error {
			fp := m.crypto.Machine().Fingerprint()
			if attempt == 0 {
				fingerprint = fp
			} else if fp != fingerprint {
				return fmt.Errorf("identity changed after reopening SQLite")
			}
			if e := m.send(ctx, "stable-batch", "Título secreto", logo, "image/png"); e != nil {
				return e
			}
			mu.Lock()
			body := append([]byte(nil), wire...)
			ciphertext := append([]byte(nil), uploaded...)
			mu.Unlock()
			if bytes.Contains(body, []byte("Título")) || bytes.Equal(ciphertext, logo) {
				return fmt.Errorf("plaintext was sent")
			}
			var encrypted event.EncryptedEventContent
			if e := json.Unmarshal(body, &encrypted); e != nil {
				return e
			}
			evt := &event.Event{ID: id.EventID(fmt.Sprintf("$test%d", attempt)), RoomID: m.room, Sender: m.cli.UserID, Type: event.EventEncrypted, Timestamp: time.Now().UnixMilli(), Content: event.Content{Parsed: &encrypted}}
			decrypted, e := m.crypto.Decrypt(ctx, evt)
			if e != nil {
				return fmt.Errorf("decrypt room event: %w", e)
			}
			msg := decrypted.Content.AsMessage()
			if msg.MsgType != event.MsgImage || msg.Body != "Título secreto" || msg.File == nil {
				return fmt.Errorf("invalid decrypted image event")
			}
			plain, e := msg.File.Decrypt(ciphertext)
			if e != nil || !bytes.Equal(plain, logo) {
				return fmt.Errorf("poster roundtrip failed: %v", e)
			}
			return nil
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(txns) != 2 || txns[0] != txns[1] {
		t.Fatal("transaction ID changed across restart")
	}
}
