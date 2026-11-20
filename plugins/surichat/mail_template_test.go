package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
)

func TestBrandedReminderUsesSignInAndInlineWordmark(t *testing.T) {
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 188, 48))); err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public branding request sent credentials")
		}
		switch r.URL.Path {
		case "/api/v2/theme/branding":
			io.WriteString(w, `{"server_name":"Example & Silo","wordmark_url":"/api/v2/branding/assets/wordmark?v=test"}`)
		case "/api/v2/branding/assets/wordmark":
			if r.URL.Query().Get("v") != "test" {
				t.Error("asset version missing")
			}
			w.Write(original.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	brand := loadEmailBrand(t.Context(), host.URL)
	if len(brand.PNG) == 0 || brand.Name != "Example & Silo" || brand.Width != 188 || brand.Height != 48 {
		t.Fatalf("invalid brand: %q %dx%d, %d bytes", brand.Name, brand.Width, brand.Height, len(brand.PNG))
	}

	var raw bytes.Buffer
	cfg := mailSettings{brand: brand, recipientName: "Lucía & Co", from: "support@example.test", webURL: "https://silo.example.test/"}
	if err := reminderMessage(&raw, cfg, "user@example.test", 2); err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(&raw)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || sender.Name != "Example & Silo" || sender.Address != cfg.from {
		t.Fatalf("unexpected From: %q (%v)", msg.Header.Get("From"), err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != "Example & Silo - Surichat Mensajes pendientes / Unread messages" {
		t.Fatalf("unexpected Subject: %q (%v)", subject, err)
	}
	reader := func(contentType, kind string, input io.Reader) *multipart.Reader {
		t.Helper()
		got, params, err := mime.ParseMediaType(contentType)
		if err != nil || got != kind {
			t.Fatalf("expected %s, got %q (%v)", kind, contentType, err)
		}
		return multipart.NewReader(input, params["boundary"])
	}
	part := func(r *multipart.Reader) *multipart.Part {
		t.Helper()
		p, err := r.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	all := func(r io.Reader) string {
		t.Helper()
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	alt := reader(msg.Header.Get("Content-Type"), "multipart/alternative", msg.Body)
	if plain := all(part(alt)); !strings.Contains(plain, cfg.webURL) || !strings.Contains(plain, "Sign in") || !strings.Contains(plain, "Mensajes pendientes en Surichat en Example & Silo") || !strings.Contains(plain, "Unread Surichat messages on Example & Silo") {
		t.Fatalf("missing sign-in instructions: %s", plain)
	}
	related := part(alt)
	inside := reader(related.Header.Get("Content-Type"), "multipart/related", related)
	html := all(part(inside))
	for _, wanted := range []string{`href="https://silo.example.test/"`, `src="cid:silo-logo"`, "Mensajes pendientes en Surichat en Example &amp; Silo</h1>", "Unread Surichat messages on Example &amp; Silo</h2>", "Tienes 2 mensajes pendientes", "Hola, Lucía &amp; Co.", "Hi Lucía &amp; Co.", "<strong>Surichat</strong>"} {
		if !strings.Contains(html, wanted) {
			t.Errorf("HTML missing %q", wanted)
		}
	}
	logo := part(inside)
	if logo.Header.Get("Content-ID") != "<silo-logo>" || logo.FileName() != "logo.png" ||
		all(base64.NewDecoder(base64.StdEncoding, logo)) != string(brand.PNG) {
		t.Fatal("invalid inline logo")
	}
}
