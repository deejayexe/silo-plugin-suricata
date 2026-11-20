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
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var reminderTemplate = template.Must(template.ParseFS(assets, "reminder.html"))

type emailBrand struct {
	Name          string
	Accent        string
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
		Accent   string `json:"accent_color"`
	}
	if json.Unmarshal(data, &config) != nil {
		return brand
	}
	if name := strings.Join(strings.FieldsFunc(config.Name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " "); name != "" {
		brand.Name = name
	}
	brand.Accent = config.Accent
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
	scale := math.Min(1, math.Min(240/float64(dimensions.Width), 48/float64(dimensions.Height)))
	displayWidth, displayHeight := max(1, int(float64(dimensions.Width)*scale)), max(1, int(float64(dimensions.Height)*scale))
	if dimensions.Width > displayWidth*2 || dimensions.Height > displayHeight*2 {
		scaled := image.NewNRGBA(image.Rect(0, 0, displayWidth*2, displayHeight*2))
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), logo, logo.Bounds(), draw.Src, nil)
		logo = scaled
	}
	var converted bytes.Buffer
	if err = png.Encode(&converted, logo); err != nil {
		return brand
	}
	brand.PNG = converted.Bytes()
	brand.Width, brand.Height = displayWidth, displayHeight
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

// Only validated hex colors are passed into the email's inline CSS.
func reminderButtonColors(accent string) (background, foreground string) {
	background = "#f59e0b"
	accent = strings.TrimSpace(accent)
	if len(accent) == 7 && accent[0] == '#' {
		if _, err := strconv.ParseUint(accent[1:], 16, 24); err == nil {
			background = strings.ToLower(accent)
		}
	}
	rgb, _ := strconv.ParseUint(background[1:], 16, 24)
	linear := func(shift uint) float64 {
		channel := float64((rgb>>shift)&255) / 255
		if channel <= 0.04045 {
			return channel / 12.92
		}
		return math.Pow((channel+0.055)/1.055, 2.4)
	}
	luminance := 0.2126*linear(16) + 0.7152*linear(8) + 0.0722*linear(0)
	if luminance < math.Sqrt(0.0525)-0.05 {
		return background, "#ffffff"
	}
	return background, "#000000"
}

func reminderMessage(out io.Writer, m mailSettings, to string, unread int) error {
	brand := m.brand
	if brand.Name == "" {
		brand.Name = "Silo"
	}
	background, foreground := reminderButtonColors(brand.Accent)
	var html bytes.Buffer
	if err := reminderTemplate.ExecuteTemplate(&html, "reminder.html", struct {
		emailBrand
		URL                          string
		Recipient                    string
		Unread                       int
		ButtonBackground, ButtonText string
	}{brand, m.webURL, m.recipientName, unread, background, foreground}); err != nil {
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
	plain := fmt.Sprintf("Mensajes pendientes en Surichat - %s\r\n\r\n%s Inicia sesión en %s y abre Surichat desde el menú de usuario.\r\n\r\nUnread Surichat messages - %s\r\n\r\n%s Sign in to %s and open Surichat from the user menu.\r\n", brand.Name, spanish, brand.Name, brand.Name, english, brand.Name)
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
