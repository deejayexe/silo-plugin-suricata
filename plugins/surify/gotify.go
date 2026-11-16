package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxTitleRunes     = 250
	maxMessageRunes   = 16384
	maxURLBytes       = 2048
	maxURLTitleRunes  = 100
	maxAttachment     = 5 << 20
	priorityEmergency = 2 // Internal alert level; delivered once as Gotify priority 10.
	maxGotifyConns    = 2
)

var (
	gotifyKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)
	gotifyDevicePattern = regexp.MustCompile(`^$`)
	gotifySoundPattern  = regexp.MustCompile(`^$`)
	builtinSounds       = []option{}
)

type option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
type gotifyMessage struct {
	ImageURL                                                  string
	Token, User, Title, Message, URL, URLTitle, Device, Sound string
	Priority                                                  int
	HTML                                                      bool
	Timestamp                                                 time.Time
	Attachment                                                []byte
	AttachmentType                                            string
}
type gotifyError struct {
	HTTPStatus int
	Errors     []string
	Retryable  bool
}

func (e *gotifyError) Error() string {
	if len(e.Errors) > 0 {
		return "Gotify: " + strings.Join(e.Errors, "; ")
	}
	return fmt.Sprintf("Gotify returned HTTP %d", e.HTTPStatus)
}

type gotifyClient struct {
	http  *http.Client
	slots chan struct{}
}

func newGotifyClient() *gotifyClient {
	return &gotifyClient{http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, maxGotifyConns)}
}
func validServerURL(server string) bool {
	u, e := url.Parse(server)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

// Internal levels retain the upstream quiet-hour and alert inheritance behavior.
func gotifyPriority(level int) int {
	switch level {
	case -2:
		return 0
	case -1:
		return 2
	case 1:
		return 8
	case 2:
		return 10
	default:
		return 5
	}
}
func (c *gotifyClient) Send(ctx context.Context, m gotifyMessage) error {
	text := truncateRunes(m.Message, maxMessageRunes)
	if text == "" {
		text = "(no message)"
	}
	extras := map[string]any{}
	link, image := safeMessageURL(m.URL), safeMessageURL(m.ImageURL)
	if link != "" || image != "" {
		text = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "`", "\\`").Replace(text)
		if image != "" {
			text += "\n\n![Silo poster](" + markdownURL(image) + ")"
		}
		if link != "" {
			text += "\n\n[Open in Silo](" + markdownURL(link) + ")"
		}
		extras["client::display"] = map[string]string{"contentType": "text/markdown"}
		notification := map[string]any{}
		if image != "" {
			notification["bigImageUrl"] = image
		}
		if link != "" {
			notification["click"] = map[string]string{"url": link}
		}
		extras["client::notification"] = notification
	}
	body, _ := json.Marshal(map[string]any{"title": truncateRunes(m.Title, maxTitleRunes), "message": text, "priority": gotifyPriority(m.Priority), "extras": extras})
	_, err := c.do(ctx, http.MethodPost, m.User, "/message", m.Token, body)
	return err
}

// Gotify has no application-token validation endpoint without sending a message.
// This checks server identity only; the explicit test notification checks the token.
func (c *gotifyClient) Validate(ctx context.Context, token, server string) ([]string, []option, error) {
	if !validApplicationToken(token) {
		return nil, nil, &gotifyError{Errors: []string{errApplicationToken.Error()}}
	}
	raw, err := c.do(ctx, http.MethodGet, server, "/version", "", nil)
	if err != nil {
		return nil, nil, err
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version == "" {
		return nil, nil, &gotifyError{Errors: []string{"The server did not return a Gotify version."}}
	}
	return []string{}, []option{}, nil
}
func (c *gotifyClient) do(ctx context.Context, method, server, path, token string, body []byte) ([]byte, error) {
	if !validServerURL(server) {
		return nil, &gotifyError{Errors: []string{"Enter a valid HTTP(S) Gotify server URL."}}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(server, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, &gotifyError{Errors: []string{"Could not build the Gotify request."}}
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		header, err := applicationTokenHeader(token, time.Now())
		if err != nil {
			return nil, &gotifyError{Errors: []string{errApplicationToken.Error()}}
		}
		req.Header.Set("X-Gotify-Key", header)
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, &gotifyError{Errors: []string{"Request cancelled."}, Retryable: true}
	}
	defer func() { <-c.slots }()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &gotifyError{Errors: []string{"Could not reach the Gotify server."}, Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &gotifyError{Errors: []string{"Could not read the Gotify response."}, Retryable: true}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if method == http.MethodPost {
			var ack struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(raw, &ack) != nil || ack.ID <= 0 {
				return nil, &gotifyError{Errors: []string{"Gotify did not confirm message delivery."}, Retryable: true}
			}
		}
		return raw, nil
	}
	// Do not reflect upstream bodies, URLs or credentials into logs or the UI.
	return nil, &gotifyError{HTTPStatus: resp.StatusCode, Retryable: resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 408}
}
func sortedOptions(opts []option) []option {
	slices.SortFunc(opts, func(a, b option) int { return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)) })
	return opts
}
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
}

func safeMessageURL(raw string) string {
	if raw == "" || len(raw) > 8192 {
		return ""
	}
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "\r\n") {
		return ""
	}
	return raw
}
func markdownURL(raw string) string {
	return strings.NewReplacer("(", "%28", ")", "%29", "<", "%3C", ">", "%3E", " ", "%20", "\\", "%5C").Replace(raw)
}
