package main

import (
	"cmp"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// The stop-stream page lets an acting admin see live streams and stop one,
// with or without a message to the viewer. The page is an admin-only plugin
// route that isn't in any sidebar; the Gotify page's admin section links to
// it. Opening it only reads: the read-only key lists live sessions. Stopping
// is a JSON POST, which the plugin sends to Silo with the stream-control key
// (admin:sessions:control). That key is never logged or returned.
//
// Silo's sequenced playback commands (silo-server
// internal/api/handlers/admin_playback_commands.go, checked at d10d9c93):
//
//   - POST /api/v2/admin/sessions/{id}/stop and /message take command_id (a
//     canonical lowercase UUID naming one command; a retry with the same body
//     replays the receipt) and sequence (a positive number, at most 2^53-1,
//     that must be above the last command Silo applied to that session; a
//     lower one gets 409 with X-Silo-Latest-Sequence). Optional reason (up to
//     1024) and deadline_ms (how long to wait for the player to acknowledge a
//     stop before Silo ends the session itself; default 3000, max 10000,
//     ignored by message). message also takes title (up to 256) and message
//     (required, up to 2048).
//   - 202 (applied) or 200 (replayed) with a receipt: delivery "dispatched"
//     or, for a stop without a live connection to the player,
//     "fallback_scheduled" (Silo ends the session after the deadline).
//   - 404 when the session is gone; 409 when a message has no live
//     connection to the player; 503 when commands are unavailable.
//
// Silo's web player shows a message but not a stop's reason, so "Stop with
// message" sends the message, waits a moment so it can be read, then stops
// with the same text as the reason (for players that show one).

//go:embed web/stop.html
var stopHTML []byte

const (
	// stopPagePath is the page's route (manifest "stop-stream-page").
	stopPagePath = "/admin/sessions"

	// stopBudget bounds a stop, message included. It runs without the page
	// request's deadline (Silo gives plugin routes 10 s), so a stop that has
	// started isn't cut off between the message and the stop.
	stopBudget = 8 * time.Second
	// commandCallTimeout bounds each command call to Silo.
	commandCallTimeout = 3 * time.Second
	// messageGap is how long the viewer gets to read the message before the
	// stream stops.
	messageGap = 2 * time.Second
	// maxStopMessage keeps the text within both the message (2048) and the
	// stop reason (1024) limits.
	maxStopMessage   = 1000
	stopMessageTitle = "Message from the server admin"

	latestSequenceHeader = "X-Silo-Latest-Sequence"
	maxCommandSequence   = 1<<53 - 1
)

// sessionIDPattern accepts Silo's session IDs (path parameter, at most 128
// characters) and nothing that could change the request path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)

var (
	errSessionEnded        = errors.New("the playback session has ended")
	errControlKeyRejected  = errors.New("Silo rejected the stream-control key (HTTP 401)")
	errControlKeyScope     = errors.New("Silo didn't let the stream-control key send this command (HTTP 403)")
	errNoRealtime          = errors.New("the player has no live connection for messages")
	errCommandsUnavailable = errors.New("Silo can't send playback commands right now")
)

// commandReceipt is Silo's AdminPlaybackCommandReceipt.
type commandReceipt struct {
	CommandID string `json:"command_id"`
	Sequence  int64  `json:"sequence"`
	Outcome   string `json:"outcome"`  // applied or replayed
	Delivery  string `json:"delivery"` // dispatched or fallback_scheduled
}

// sessionCommander sends Silo's sequenced playback commands with the
// stream-control key, which is passed in on every call and never kept.
type sessionCommander struct {
	http    *http.Client
	timeout time.Duration
	now     func() time.Time
	newID   func() string
	// gap is how long a message stays up before the stop (messageGap).
	gap time.Duration

	mu   sync.Mutex
	last map[string]int64 // last sequence used per session
}

func newSessionCommander() *sessionCommander {
	return &sessionCommander{
		http:    &http.Client{Timeout: 10 * time.Second},
		timeout: commandCallTimeout,
		now:     time.Now,
		newID:   newCommandID,
		gap:     messageGap,
		last:    map[string]int64{},
	}
}

// newCommandID returns a random (version 4) UUID in the canonical lowercase
// form Silo requires.
func newCommandID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// allocate picks a sequence for one command: the clock in milliseconds (as
// Silo's web app does, so commands from either rise together), above this
// process's last one for the session and above floor.
func (c *sessionCommander) allocate(sessionID string, floor int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := max(c.last[sessionID]+1, floor+1, c.now().UnixMilli())
	if len(c.last) >= 1024 {
		c.last = map[string]int64{}
	}
	c.last[sessionID] = seq
	return seq
}

// send posts one command (action "stop" or "message") with a new identity.
// When Silo says another command got further ahead (a stale sequence), it
// tries once more above the sequence Silo reports.
func (c *sessionCommander) send(ctx context.Context, baseURL, apiKey, sessionID, action string, fields map[string]any) (*commandReceipt, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, errSessionEnded
	}
	var floor int64
	for attempt := 0; ; attempt++ {
		body := maps.Clone(fields)
		if body == nil {
			body = map[string]any{}
		}
		body["command_id"] = c.newID()
		body["sequence"] = c.allocate(sessionID, floor)
		var header http.Header
		var receipt commandReceipt
		err := siloCall(ctx, c.http, c.timeout, siloRequest{
			method: http.MethodPost, baseURL: baseURL, apiKey: apiKey,
			path: "/api/v2/admin/sessions/" + url.PathEscape(sessionID) + "/" + action,
			body: body, respHeader: &header,
		}, &receipt)
		var serr *siloStatusError
		switch {
		case err == nil:
			return &receipt, nil
		case errors.Is(err, errSiloKeyRejected):
			return nil, errControlKeyRejected
		case errors.Is(err, errSiloKeyScope):
			return nil, errControlKeyScope
		case !errors.As(err, &serr):
			return nil, err
		case serr.Status == http.StatusNotFound:
			return nil, errSessionEnded
		case serr.Status == http.StatusServiceUnavailable:
			return nil, errCommandsUnavailable
		case serr.Status == http.StatusConflict:
			if latest, ok := latestSequence(header); ok && attempt == 0 && latest < maxCommandSequence {
				floor = latest
				continue
			}
			if strings.Contains(strings.ToLower(serr.Detail), "realtime") {
				return nil, errNoRealtime
			}
		}
		return nil, err
	}
}

// latestSequence reads a stale refusal's latest applied sequence.
func latestSequence(h http.Header) (int64, bool) {
	raw := strings.TrimSpace(h.Get(latestSequenceHeader))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n > 0 && n <= maxCommandSequence
}

// isStopPagePath reports whether path is the stop-stream page: the live
// stream list, or one stream.
func isStopPagePath(path string) bool {
	return path == stopPagePath || path == stopPagePath+"/" || strings.HasPrefix(path, stopPagePath+"/")
}

// serveStopPage answers GET /admin/sessions[/{id}]. The page loads the
// stream itself; opening it never changes anything.
func serveStopPage(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	if req.GetMethod() != http.MethodGet {
		return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
	}
	// Silo only lets acting admins through (route access "admin"); check
	// again so the page never shows to anyone else.
	if uid, _ := strconv.Atoi(header(req, "X-Silo-User-Id")); uid <= 0 || header(req, "X-Silo-User-Role") != "admin" {
		return &pluginv1.HandleHTTPResponse{
			StatusCode: http.StatusForbidden,
			Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store"},
			Body:       []byte("Only a server admin, signed in to their primary profile, can open this page."),
		}
	}
	return servePage(req, stopHTML)
}

// liveSessionView is one live stream as the stop-stream page shows it.
type liveSessionView struct {
	SessionID  string `json:"session_id"`
	User       string `json:"user"`
	Title      string `json:"title"`
	Episode    string `json:"episode"`
	MediaType  string `json:"media_type"`
	Device     string `json:"device"`
	Client     string `json:"client"`
	Location   string `json:"location"`
	IP         string `json:"ip"`
	Method     string `json:"method"`
	Video      string `json:"video"`
	Bitrate    string `json:"bitrate"`
	Position   string `json:"position"`
	Duration   string `json:"duration"`
	Progress   string `json:"progress"`
	Paused     bool   `json:"paused"`
	StartedAt  string `json:"started_at"`
	CanMessage bool   `json:"can_message"`
}

func newLiveSessionView(row *adminSession) liveSessionView {
	pb := playback{SessionID: row.SessionID}
	pb.fromSession(row)
	if pb.Location == "" {
		pb.Location = locationFromIP(pb.ClientIP)
	}
	v := pb.vars()
	return liveSessionView{
		SessionID: row.SessionID, User: v["user_profile"], Title: cmp.Or(v["title"], "Something"),
		Episode: v["episode_title"], MediaType: v["media_type"], Device: v["device"], Client: v["client"],
		Location: v["location"], IP: v["ip"], Method: v["method"], Video: v["video"], Bitrate: v["bitrate"],
		Position: v["position"], Duration: v["duration"], Progress: v["progress"], Paused: row.IsPaused,
		StartedAt: row.StartedAt, CanMessage: row.HasPlaybackControl == nil || *row.HasPlaybackControl,
	}
}

// serveSessionsAPI answers the stop-stream page's routes under
// /api/admin/sessions:
//
//	GET  /api/admin/sessions            live streams
//	GET  /api/admin/sessions/{id}       one stream, or ended
//	POST /api/admin/sessions/{id}/stop  stop it, optionally with a message
func (p *Plugin) serveSessionsAPI(ctx context.Context, req *pluginv1.HandleHTTPRequest, rest []string) *pluginv1.HandleHTTPResponse {
	method := req.GetMethod()
	switch {
	case len(rest) == 0, len(rest) == 1:
		if method != http.MethodGet {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
		}
	case len(rest) == 2 && rest[1] == "stop":
		if method != http.MethodPost {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
		}
		return p.handleStopSession(ctx, req, rest[0])
	default:
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
	}
	// Reading needs an acting admin and a working read-only key.
	a, aerr := p.resolveAdmin(ctx, req)
	if aerr != nil {
		return errorResponse(aerr)
	}
	rows, err := p.silo.Sessions(ctx, a.base, a.key)
	switch {
	case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
		return errorResponse(monitorKeyProblem(err))
	case err != nil:
		p.logger.Warn("could not list live streams with the read-only Silo key", "error", err)
		return errorResponse(apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't reach Silo to list live streams. Reload the page to try again."))
	}
	controlSet := p.config().ControlAPIKey != ""
	if len(rest) == 1 {
		for i := range rows {
			if rows[i].SessionID == rest[0] {
				view := newLiveSessionView(&rows[i])
				return jsonResponse(http.StatusOK, map[string]any{"session": view, "ended": false, "control_key_set": controlSet})
			}
		}
		return jsonResponse(http.StatusOK, map[string]any{"session": nil, "ended": true, "control_key_set": controlSet})
	}
	views := make([]liveSessionView, 0, len(rows))
	for i := range rows {
		if rows[i].SessionID != "" {
			views = append(views, newLiveSessionView(&rows[i]))
		}
	}
	slices.SortStableFunc(views, func(x, y liveSessionView) int { return strings.Compare(y.StartedAt, x.StartedAt) })
	return jsonResponse(http.StatusOK, map[string]any{"sessions": views, "control_key_set": controlSet})
}

type stopInput struct {
	// Message, when set, is shown on the player before the stream stops.
	Message string `json:"message"`
}

// stopResult reports a stop. Details are plain sentences for the page.
type stopResult struct {
	OK      bool     `json:"ok"`
	Level   string   `json:"level"` // "ok" or "warn"
	Title   string   `json:"title"`
	Details []string `json:"details"`
}

const controlKeyFix = " Set it up again from the Silo keys section of the Gotify page (tick “Also allow stopping streams”), or replace it under " + adminFormPath + "."

// handleStopSession is POST /api/admin/sessions/{id}/stop. serveAPI has
// already checked that the body is JSON.
func (p *Plugin) handleStopSession(ctx context.Context, req *pluginv1.HandleHTTPRequest, sessionID string) *pluginv1.HandleHTTPResponse {
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if userID <= 0 {
		return errorResponse(apiErr(http.StatusUnauthorized, "unauthenticated", "Your session for this page has expired. Open Surify from the administrator navigation again."))
	}
	if header(req, "X-Silo-User-Role") != "admin" {
		return errorResponse(apiErr(http.StatusForbidden, "forbidden", "Only a server admin, signed in to their primary profile, can stop streams."))
	}
	var in stopInput
	if err := json.Unmarshal(req.GetBody(), &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the request."))
	}
	msg := strings.TrimSpace(in.Message)
	if utf8.RuneCountInString(msg) > maxStopMessage {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Keep the message under "+strconv.Itoa(maxStopMessage)+" characters."))
	}
	if !sessionIDPattern.MatchString(sessionID) {
		return errorResponse(apiErr(http.StatusNotFound, "session_ended", "This stream has already ended."))
	}
	key := p.config().ControlAPIKey
	if key == "" {
		return errorResponse(apiErr(http.StatusConflict, "control_key_missing", "Stopping streams needs the plugin's stream-control key (scope admin:sessions:control), which isn't set up."+controlKeyFix))
	}
	base, err := p.siloBase(ctx)
	if err != nil {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopBudget)
	defer cancel()
	log := p.logger.With("user_id", userID, "session_id", sessionID)
	res := &stopResult{OK: true, Level: "ok", Title: "Stream stopped."}

	stopBody := map[string]any{}
	if msg != "" {
		_, err := p.commands.send(ctx, base, key, sessionID, "message", map[string]any{"title": stopMessageTitle, "message": msg})
		switch {
		case err == nil:
			res.Details = append(res.Details, "Your message was shown on the player.")
			_ = sleepCtx(ctx, p.commands.gap)
		case errors.Is(err, errNoRealtime):
			res.Level = "warn"
			res.Details = append(res.Details, "The player has no live connection to Silo for messages (some apps can't show them), so your message wasn't shown.")
		default:
			log.Warn("could not send a message to a stream before stopping it", "error", err)
			return errorResponse(commandProblem(err, "The stream wasn't stopped."))
		}
		stopBody["reason"] = msg
	}

	receipt, err := p.commands.send(ctx, base, key, sessionID, "stop", stopBody)
	if err != nil {
		log.Warn("could not stop a stream", "error", err)
		note := "The stream wasn't stopped."
		if len(res.Details) > 0 && res.Level == "ok" {
			note = "Your message was shown, but the stream wasn't stopped."
		}
		return errorResponse(commandProblem(err, note))
	}
	if receipt.Delivery == "fallback_scheduled" {
		res.Title = "Stopping the stream."
		res.Details = append(res.Details, "The player didn't take the command right away, so Silo will end the session within a few seconds.")
	}
	log.Info("stopped a stream from the stop-stream page", "with_message", msg != "", "delivery", receipt.Delivery, "outcome", receipt.Outcome)
	return jsonResponse(http.StatusOK, res)
}

// commandProblem explains a failed command. note says what didn't happen.
func commandProblem(err error, note string) *apiError {
	var serr *siloStatusError
	switch {
	case errors.Is(err, errSessionEnded):
		return apiErr(http.StatusNotFound, "session_ended", "This stream has already ended.")
	case errors.Is(err, errControlKeyRejected):
		return apiErr(http.StatusConflict, "control_key_rejected", note+" Silo rejected the plugin's stream-control key; it may have been revoked."+controlKeyFix)
	case errors.Is(err, errControlKeyScope):
		return apiErr(http.StatusConflict, "control_key_scope", note+" Silo didn't let the plugin's stream-control key do this. It needs the admin:sessions:control scope and must belong to an enabled admin account."+controlKeyFix)
	case errors.Is(err, errCommandsUnavailable):
		return apiErr(http.StatusServiceUnavailable, "commands_unavailable", note+" Silo can't send playback commands right now. Try again in a moment, or stop it from Silo's admin dashboard.")
	case errors.Is(err, errNoRealtime):
		return apiErr(http.StatusConflict, "no_realtime", note+" The player has no live connection to Silo.")
	case isTimeout(err):
		return apiErr(http.StatusGatewayTimeout, "silo_timeout", "Silo didn't answer in time, so the plugin can't tell whether the stream stopped. Reload the page to check.")
	case errors.As(err, &serr):
		return apiErr(http.StatusBadGateway, "silo_failed", note+" "+serr.Error()+".")
	}
	return apiErr(http.StatusBadGateway, "silo_unavailable", note+" Couldn't reach Silo. Try again.")
}
