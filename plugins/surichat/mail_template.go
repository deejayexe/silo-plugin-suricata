package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode"

	_ "golang.org/x/image/webp"
)

var reminderTemplate = template.Must(template.ParseFS(assets, "reminder.html"))

type emailBrand struct {
	Name          string
	PNG           []byte
	Width, Height int
}

func loadEmailBrand(ctx context.Context, serverURL string) emailBrand {
	brand := emailBrand{Name: "Silo"}
	base, err := url.Parse(serverURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return brand
	}
	client := http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	brandingURL := base.JoinPath("api/v2/theme/branding")
	data := brandingBytes(ctx, &client, brandingURL, 16384)
	var config struct {
		Name     string `json:"server_name"`
		Wordmark string `json:"wordmark_url"`
	}
	if json.Unmarshal(data, &config) != nil {
		return brand
	}
	if name := strings.Join(strings.FieldsFunc(config.Name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " "); name != "" {
		brand.Name = name
	}
	ref, err := url.Parse(config.Wordmark)
	if err != nil || ref.IsAbs() || ref.Host != "" || ref.User != nil || ref.Fragment != "" || ref.Path != "/api/v2/branding/assets/wordmark" {
		return brand
	}
	assetURL := base.JoinPath(strings.TrimPrefix(ref.Path, "/"))
	assetURL.RawQuery = ref.RawQuery
	data = brandingBytes(ctx, &client, assetURL, 2<<20)
	dimensions, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > 1600 || dimensions.Height > 800 {
		return brand
	}
	logo, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return brand
	}
	var converted bytes.Buffer
	if err = png.Encode(&converted, logo); err != nil || converted.Len() > 2<<20 {
		return brand
	}
	scale := math.Min(1, math.Min(240/float64(dimensions.Width), 48/float64(dimensions.Height)))
	brand.PNG = converted.Bytes()
	brand.Width = max(1, int(float64(dimensions.Width)*scale))
	brand.Height = max(1, int(float64(dimensions.Height)*scale))
	return brand
}

func brandingBytes(ctx context.Context, client *http.Client, target *url.URL, limit int64) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil
	}
	return data
}

func reminderMessage(out io.Writer, m mailSettings, to string, unread int) error {
	brand := m.brand
	if brand.Name == "" {
		brand.Name = "Silo"
	}
	var html bytes.Buffer
	if err := reminderTemplate.ExecuteTemplate(&html, "reminder.html", struct {
		emailBrand
		URL       string
		Recipient string
		Unread    int
	}{brand, m.webURL, m.recipientName, unread}); err != nil {
		return err
	}
	var spanish, english string
	if m.recipientName != "" {
		spanish, english = "Hola, "+m.recipientName+". ", "Hi "+m.recipientName+". "
	}
	if unread == 1 {
		spanish += "Tienes un mensaje pendiente."
		english += "You have one unread message."
	} else {
		spanish += fmt.Sprintf("Tienes %d mensajes pendientes.", unread)
		english += fmt.Sprintf("You have %d unread messages.", unread)
	}
	plain := fmt.Sprintf("Mensajes pendientes en Surichat en %s\r\n\r\n%s Inicia sesión en %s y abre Surichat desde el menú de usuario.\r\n\r\nUnread Surichat messages on %s\r\n\r\n%s Sign in to %s and open Surichat from the user menu.\r\n", brand.Name, spanish, brand.Name, brand.Name, english, brand.Name)
	if m.webURL != "" {
		plain += "\r\n" + m.webURL + "\r\n"
	}
	alternative := multipart.NewWriter(out)
	messageType := mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": alternative.Boundary()})
	sender := (&mail.Address{Name: brand.Name, Address: m.from}).String()
	subject := mime.QEncoding.Encode("utf-8", brand.Name+" - Surichat Mensajes pendientes / Unread messages")
	if _, err := fmt.Fprintf(out, "From: %s\r\nTo: <%s>\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: %s\r\n\r\n", sender, to, subject, time.Now().Format(time.RFC1123Z), messageType); err != nil {
		return err
	}
	if err := writeReminderPart(alternative, "text/plain", []byte(plain)); err != nil {
		return err
	}
	if len(brand.PNG) == 0 {
		if err := writeReminderPart(alternative, "text/html", html.Bytes()); err != nil {
			return err
		}
		return alternative.Close()
	}
	var relatedBody bytes.Buffer
	related := multipart.NewWriter(&relatedBody)
	if err := writeReminderPart(related, "text/html", html.Bytes()); err != nil {
		return err
	}
	const filename = "logo.png"
	part, err := related.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {`image/png; name="logo.png"`},
		"Content-Disposition":       {mime.FormatMediaType("inline", map[string]string{"filename": filename})},
		"Content-Id":                {"<silo-logo>"},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return err
	}
	for offset := 0; offset < len(brand.PNG); offset += 57 {
		line := base64.StdEncoding.EncodeToString(brand.PNG[offset:min(offset+57, len(brand.PNG))])
		if _, err = io.WriteString(part, line+"\r\n"); err != nil {
			return err
		}
	}
	if err := related.Close(); err != nil {
		return err
	}
	relatedType := mime.FormatMediaType("multipart/related", map[string]string{"boundary": related.Boundary(), "type": "text/html"})
	htmlPart, err := alternative.CreatePart(textproto.MIMEHeader{"Content-Type": {relatedType}})
	if err != nil {
		return err
	}
	if _, err = relatedBody.WriteTo(htmlPart); err != nil {
		return err
	}
	return alternative.Close()
}

func writeReminderPart(writer *multipart.Writer, kind string, body []byte) error {
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {kind + "; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
	if err != nil {
		return err
	}
	encoded := quotedprintable.NewWriter(part)
	if _, err = encoded.Write(body); err != nil {
		return err
	}
	return encoded.Close()
}
