package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	manifestutil "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestManifest(t *testing.T) {
	m, e := manifestutil.Load(manifestJSONForTest())
	if e != nil {
		t.Fatal(e)
	}
	if e = manifestutil.Validate(m); e != nil {
		t.Fatal(e)
	}
}
func manifestJSONForTest() []byte { return manifest }
func TestAdminRoutes(t *testing.T) {
	p := &plugin{}
	for _, h := range []map[string]string{nil, {"X-Silo-User-Id": "1", "X-Silo-User-Role": "user", "X-Silo-Profile-Primary": "true"}, {"X-Silo-User-Id": "1", "X-Silo-User-Role": "admin", "X-Silo-Profile-Primary": "false"}} {
		r, e := p.Handle(context.Background(), &pb.HandleHTTPRequest{Method: "POST", Path: "/api/test", Headers: h})
		if e != nil || r.StatusCode != 403 {
			t.Fatalf("unexpected access: %v %v", r, e)
		}
	}
	r, e := p.Handle(context.Background(), &pb.HandleHTTPRequest{Method: "GET", Path: "/", Headers: map[string]string{"X-Silo-User-Id": "1", "X-Silo-User-Role": "admin", "X-Silo-Profile-Primary": "true"}})
	if e != nil || r.StatusCode != 200 {
		t.Fatal(r, e)
	}
}
func TestGroupingSurvivesRestartAndCapsDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := progress{Seen: map[string]time.Time{}, Pending: map[string]*batch{}}
	i := item{ID: "episode-1", Type: "episode", SeriesID: "series-1", Season: 1, Added: now}
	enqueue(&st, i, now)
	enqueue(&st, i, now.Add(time.Minute))
	if len(st.Pending[groupKey(i)].Items) != 1 {
		t.Fatal("duplicate queued")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if e := saveJSON(path, &st); e != nil {
		t.Fatal(e)
	}
	var restored progress
	if e := loadJSON(path, &restored); e != nil {
		t.Fatal(e)
	}
	for n := 2; n <= 29; n++ {
		next := i
		next.ID = string(rune('A' + n))
		enqueue(&restored, next, now.Add(time.Duration(n)*time.Minute))
	}
	b := restored.Pending[groupKey(i)]
	if !b.Due.Equal(now.Add(30 * time.Minute)) {
		t.Fatal(b.Due)
	}
	b.Frozen = true
	before := len(b.Items)
	next := i
	next.ID = "later"
	enqueue(&restored, next, now.Add(time.Hour))
	if len(b.Items) != before || len(restored.Pending) != 2 {
		t.Fatal("retry batch mutated")
	}
	restored.Seen[i.key()] = now
	delete(restored.Pending, groupKey(i))
	enqueue(&restored, i, now.Add(time.Hour))
	if len(restored.Pending) != 1 {
		t.Fatal("already sent queued again")
	}
}
func TestMatrixImageWireFormatAndEncryption(t *testing.T) {
	plain := []byte("poster-bytes-secret")
	file := attachment.NewEncryptedFile()
	ciphertext := file.Encrypt(plain)
	if bytes.Equal(ciphertext, plain) {
		t.Fatal("attachment is plaintext")
	}
	content := event.MessageEventContent{MsgType: event.MsgImage, Body: "Title: Movie 🎬", File: &event.EncryptedFileInfo{EncryptedFile: *file, URL: id.ContentURIString("mxc://test/image")}, Info: &event.FileInfo{MimeType: "image/jpeg", Size: len(plain)}}
	raw, e := json.Marshal(content)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), `"url":"mxc://test/image"`) == false || strings.Contains(string(raw), `"v":"v2"`) == false {
		t.Fatal(string(raw))
	}
	var decoded event.MessageEventContent
	if e = json.Unmarshal(raw, &decoded); e != nil {
		t.Fatal(e)
	}
	got, e := decoded.File.Decrypt(ciphertext)
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatalf("attachment roundtrip: %s %v", got, e)
	}
	ciphertext[0] ^= 1
	if _, e = decoded.File.Decrypt(ciphertext); e == nil {
		t.Fatal("tampering accepted")
	}
}
func TestArtworkDestinations(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1", "224.0.0.1"} {
		if publicAddress(net.ParseIP(s)) {
			t.Fatalf("accepted %s", s)
		}
	}
	if !publicAddress(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
}
func TestNoUnverifiedCompleteSeasonClaim(t *testing.T) {
	b := &batch{Items: []item{{Season: 1, Episode: 1, Title: "Uno"}, {Season: 1, Episode: 2, Title: "Dos"}}}
	s := formatBatch(b, item{Type: "episode", Title: "Series"}, nil, "https://silo.example")
	if strings.Contains(s, "Complete season") {
		t.Fatal(s)
	}
}
