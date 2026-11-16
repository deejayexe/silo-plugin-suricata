package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type routesServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	plugin *Plugin
}

func (s *routesServer) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if aerr := requireAdminRequest(req); aerr != nil {
		return errorResponse(aerr), nil
	}
	path := req.GetPath()
	switch {
	case path == "/assets/surify.png" || path == "/assets/surify.webp":
		if req.GetMethod() != http.MethodGet {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")), nil
		}
		return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusOK, Headers: map[string]string{"Content-Type": "image/webp", "Cache-Control": "private, max-age=3600", "X-Content-Type-Options": "nosniff"}, Body: surifyLogo}, nil
	case path == "" || path == "/" || path == "/index.html":
		if req.GetMethod() != http.MethodGet {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")), nil
		}
		return serveIndex(req), nil
	case isStopPagePath(path):
		return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusSeeOther, Headers: map[string]string{"Location": "/admin/activity", "Cache-Control": "no-store"}}, nil
	case strings.HasPrefix(path, "/api/"):
		return s.plugin.serveAPI(ctx, req), nil
	}
	return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found.")), nil
}

// serveIndex returns the settings page, pre-set to the viewer's Silo theme so
// it doesn't flash the wrong colours.
func serveIndex(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	return servePage(req, indexHTML)
}

// servePage returns an HTML page, pre-set to the viewer's Silo theme.
func servePage(req *pluginv1.HandleHTTPRequest, html []byte) *pluginv1.HandleHTTPResponse {
	theme := "auto"
	if t := strings.ToLower(header(req, "X-Silo-Theme")); t != "" {
		theme = "dark"
		if strings.Contains(t, "light") {
			theme = "light"
		}
	}
	page := bytes.Replace(html, []byte(`data-theme="auto"`), []byte(`data-theme="`+theme+`"`), 1)
	return &pluginv1.HandleHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"},
		Body:       page,
	}
}

// viewer is the authenticated administrator and their own primary connection.
type viewer struct {
	userID   int
	activeID string
	primary  bool
	// admin is Silo's "acting admin": an admin account on its primary profile.
	admin    bool
	profiles []siloProfile
	// household stays "self" for compatibility with earlier API consumers.
	household string
	// notice is reserved for administrator setup messages.
	notice *pageNotice
}

// pageNotice is a setup message shown at the top of the page.
type pageNotice struct {
	Code    string `json:"code"`
	Level   string `json:"level"` // "info" or "warn"
	Title   string `json:"title"`
	Message string `json:"message"`
}

func (v *viewer) profile(id string) (siloProfile, bool) {
	for _, p := range v.profiles {
		if p.ID == id {
			return p, true
		}
	}
	return siloProfile{}, false
}

type apiError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func apiErr(status int, code, message string) *apiError {
	return &apiError{status: status, Code: code, Message: message}
}

const adminFormPath = "Admin > Plugins > Surify"

// resolveViewer identifies the viewer from the headers Silo's plugin proxy
// sets. The proxy drops any the browser sends, verifies that the profile
// belongs to the account (PIN included), and is the only way in, so the
// headers can be trusted.
func (p *Plugin) resolveViewer(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*viewer, *apiError) {
	if aerr := requireAdminRequest(req); aerr != nil {
		return nil, aerr
	}
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	id := header(req, "X-Silo-Profile-Id")
	if id == "" {
		return nil, apiErr(http.StatusConflict, "no_profile", "Choose your administrator's primary profile in Silo.")
	}
	name := header(req, "X-Silo-Profile-Name")
	if name == "" {
		name = "Administrator"
	}
	return &viewer{userID: userID, activeID: id, primary: true, admin: true, household: "self", profiles: []siloProfile{{ID: id, Name: name}}}, nil
}

func (p *Plugin) serveAPI(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	if aerr := requireAdminRequest(req); aerr != nil {
		return errorResponse(aerr)
	}
	method := req.GetMethod()
	if (method == http.MethodPost || method == http.MethodPut) &&
		!strings.HasPrefix(strings.ToLower(header(req, "Content-Type")), "application/json") {
		// Requiring JSON keeps other sites from posting forms here with the
		// viewer's cookie.
		return errorResponse(apiErr(http.StatusUnsupportedMediaType, "unsupported_media_type", "Expected a JSON body."))
	}

	parts := strings.Split(strings.Trim(strings.TrimPrefix(req.GetPath(), "/api/"), "/"), "/")
	if len(parts) == 1 && parts[0] == "openapi.json" && method == http.MethodGet {
		return &pluginv1.HandleHTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-cache"},
			Body:       openAPIJSON,
		}
	}
	if len(parts) == 1 && parts[0] == "state" && method == http.MethodGet {
		return p.handleState(ctx, req)
	}
	if len(parts) == 2 && parts[0] == "admin" && parts[1] == "key-setup" {
		if method != http.MethodPost {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
		}
		return p.handleKeySetup(ctx, req)
	}
	if len(parts) >= 2 && parts[0] == "admin" && parts[1] == "sessions" {
		return p.serveSessionsAPI(ctx, req, parts[2:])
	}
	if len(parts) >= 2 && parts[0] == "admin" {
		return p.serveAdminAPI(ctx, req, strings.Join(parts[1:], "/"))
	}
	if len(parts) < 2 || parts[0] != "profiles" {
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
	}

	v, aerr := p.resolveViewer(ctx, req)
	if aerr != nil {
		return errorResponse(aerr)
	}
	prof, ok := v.profile(parts[1])
	if !ok {
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "That profile isn't one you can manage."))
	}

	switch {
	case len(parts) == 2 && method == http.MethodGet:
		return p.handleGetProfile(ctx, prof)
	case len(parts) == 2 && method == http.MethodPut:
		return p.handlePutProfile(ctx, v, prof, req.GetBody())
	case len(parts) == 2 && method == http.MethodDelete:
		if err := p.store.Delete(ctx, prof.ID); err != nil {
			return storeError(p, err)
		}
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
	case len(parts) == 3 && parts[2] == "test" && method == http.MethodPost:
		return p.handleTest(ctx, v, prof, req.GetBody())
	case len(parts) == 3 && parts[2] == "validate" && method == http.MethodPost:
		return p.handleValidate(ctx, prof, req.GetBody())
	}
	return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
}

type profileSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Active     bool   `json:"active"`
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
}

// handleState answers the page's first request. It reports setup problems as
// data rather than an error so the page can explain them.
func (p *Plugin) handleState(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	if aerr := requireAdminRequest(req); aerr != nil {
		return errorResponse(aerr)
	}
	state := map[string]any{
		"types":              notificationTypes,
		"sounds":             builtinSounds,
		"has_default_token":  p.config().DefaultAppToken != "",
		"default_server_url": p.config().DefaultServerURL,
	}
	v, aerr := p.resolveViewer(ctx, req)
	if aerr != nil {
		if aerr.status == http.StatusUnauthorized {
			return errorResponse(aerr)
		}
		state["problem"] = aerr
		return jsonResponse(http.StatusOK, state)
	}
	profiles := make([]profileSummary, 0, len(v.profiles))
	for _, prof := range v.profiles {
		summary := profileSummary{ID: prof.ID, Name: prof.Name, Active: prof.ID == v.activeID}
		settings, found, err := p.store.Get(ctx, prof.ID)
		if err != nil {
			return storeError(p, err)
		}
		if found {
			summary.Configured, summary.Enabled = true, settings.Enabled
		}
		profiles = append(profiles, summary)
	}
	state["profiles"] = profiles
	state["household"] = v.household
	// admin is Silo's acting admin: the page shows the admin section, whose
	// alerts live under /api/admin/.
	state["admin"] = v.admin
	if v.admin {
		state["admin_keys"] = p.adminKeyStatus()
	}
	if v.notice != nil {
		state["notice"] = v.notice
	}
	return jsonResponse(http.StatusOK, state)
}

// profileView is what the page sees. Keys are never sent back, only a hint.
type profileView struct {
	AdminManaged  bool                    `json:"admin_managed"`
	ProfileID     string                  `json:"profile_id"`
	ProfileName   string                  `json:"profile_name"`
	Configured    bool                    `json:"configured"`
	Enabled       bool                    `json:"enabled"`
	ServerURLHint string                  `json:"server_url_hint"`
	AppTokenHint  string                  `json:"app_token_hint"`
	Device        string                  `json:"device"`
	Sound         string                  `json:"sound"`
	Priority      int                     `json:"priority"`
	HTML          bool                    `json:"html"`
	AttachPoster  bool                    `json:"attach_poster"`
	IncludeLink   bool                    `json:"include_link"`
	Types         map[string]TypeSettings `json:"types"`
	// EpisodeReasons is the reason filter for new episodes (empty: all),
	// GroupMinutes the season-drop window (0: off), and Quiet the profile's
	// quiet hours, in the time zone the browser last reported.
	EpisodeReasons []string   `json:"episode_reasons"`
	GroupMinutes   int        `json:"group_minutes"`
	Quiet          QuietHours `json:"quiet"`
	TimeZone       string     `json:"time_zone"`
	// EpisodeReasonOptions lists the reasons the filter offers.
	EpisodeReasonOptions []option `json:"episode_reason_options"`
}

func newProfileView(prof siloProfile, s *ProfileSettings) profileView {
	view := profileView{
		ProfileID: prof.ID, ProfileName: prof.Name,
		Enabled: true, AttachPoster: false, IncludeLink: true,
		Types:          map[string]TypeSettings{},
		EpisodeReasons: []string{}, Quiet: QuietHours{Start: defaultQuietStart, End: defaultQuietEnd},
		EpisodeReasonOptions: episodeReasonOptions,
	}
	if s != nil {
		view.Configured = true
		view.AdminManaged = s.AdminManaged
		view.Enabled = s.Enabled
		view.ServerURLHint = s.ServerURL
		view.AppTokenHint = keyHint(s.AppToken)
		view.Device, view.Sound, view.Priority = s.Device, s.Sound, s.Priority
		view.HTML, view.AttachPoster, view.IncludeLink = s.HTML, s.AttachPoster, s.IncludeLink
		if len(s.EpisodeReasons) > 0 {
			view.EpisodeReasons = s.EpisodeReasons
		}
		view.GroupMinutes, view.TimeZone = s.GroupMinutes, s.TimeZone
		if s.Quiet != (QuietHours{}) {
			view.Quiet = s.Quiet
		}
	}
	for _, t := range notificationTypes {
		ts := t.defaults()
		if s != nil {
			ts = s.typeSettings(t.Key)
		}
		if t.groupable() {
			// Show the wording a merged notification will use.
			ts.GroupTitle, ts.GroupMessage = ts.groupWording(t)
		}
		view.Types[t.Key] = ts
	}
	return view
}

func keyHint(key string) string {
	if len(key) < 8 {
		return ""
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func (p *Plugin) handleGetProfile(ctx context.Context, prof siloProfile) *pluginv1.HandleHTTPResponse {
	settings, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	return jsonResponse(http.StatusOK, newProfileView(prof, settings))
}

// profileInput is a save or test request. Empty keys keep the stored ones.
type profileInput struct {
	Enabled       bool                    `json:"enabled"`
	ServerURL     string                  `json:"server_url"`
	AppToken      string                  `json:"app_token"`
	ClearAppToken bool                    `json:"clear_app_token"`
	Device        string                  `json:"device"`
	Sound         string                  `json:"sound"`
	Priority      int                     `json:"priority"`
	HTML          bool                    `json:"html"`
	AttachPoster  bool                    `json:"attach_poster"`
	IncludeLink   bool                    `json:"include_link"`
	Types         map[string]TypeSettings `json:"types"`
	// EpisodeReasons, GroupMinutes, TimeZone and Quiet are the 0.3.0 settings.
	// An empty TimeZone (a browser that can't say) keeps the saved one.
	EpisodeReasons []string   `json:"episode_reasons"`
	GroupMinutes   int        `json:"group_minutes"`
	TimeZone       string     `json:"time_zone"`
	Quiet          QuietHours `json:"quiet"`
}

// applyInput merges input over the stored settings and validates the result.
func (p *Plugin) applyInput(v *viewer, prof siloProfile, existing *ProfileSettings, in profileInput) (*ProfileSettings, *apiError) {
	if v == nil || !v.admin || !v.primary || v.userID <= 0 || v.activeID != prof.ID {
		return nil, apiErr(http.StatusForbidden, "forbidden", "Only your administrator connection can be configured.")
	}
	s := &ProfileSettings{}
	if existing != nil {
		s = existing.clone()
	}
	s.ProfileID, s.ProfileName, s.UserID = prof.ID, prof.Name, v.userID
	s.Enabled = in.Enabled
	s.AdminManaged = true
	if k := strings.TrimSpace(in.ServerURL); k != "" {
		s.ServerURL = k
	}
	switch {
	case in.ClearAppToken:
		s.AppToken = ""
	case strings.TrimSpace(in.AppToken) != "":
		s.AppToken = strings.TrimSpace(in.AppToken)
	}
	s.Device = strings.TrimSpace(in.Device)
	s.Sound = strings.TrimSpace(in.Sound)
	s.Priority = in.Priority
	s.HTML, s.AttachPoster, s.IncludeLink = false, false, in.IncludeLink

	bad := func(msg string) *apiError { return apiErr(http.StatusUnprocessableEntity, "invalid", msg) }
	switch {
	case s.ServerURL == "":
		return nil, bad("Enter your Gotify server URL, for example https://gotify.example.com.")
	case !validServerURL(s.ServerURL):
		return nil, bad("Use an HTTP(S) server URL without credentials, query parameters or fragment.")
	case s.AppToken != "" && !validApplicationToken(s.AppToken):
		return nil, bad("Enter a valid Gotify application token.")
	case effectiveToken(s, p.config()) == "":
		return nil, bad("Enter an application token for this Gotify server, or use the configured shared Gotify server URL.")
	case !gotifyDevicePattern.MatchString(s.Device):
		return nil, bad("Device names can only use letters, numbers, dashes and underscores.")
	case !gotifySoundPattern.MatchString(s.Sound):
		return nil, bad("That sound name isn't valid.")
	case !validPriority(s.Priority):
		return nil, bad("Priority must be between -2 and 1.")
	}

	s.EpisodeReasons = nil
	for _, r := range in.EpisodeReasons {
		r = strings.TrimSpace(r)
		if !slices.ContainsFunc(episodeReasonOptions, func(o option) bool { return o.Value == r }) {
			return nil, bad("That isn't a reason Silo gives for a new episode.")
		}
		if !slices.Contains(s.EpisodeReasons, r) {
			s.EpisodeReasons = append(s.EpisodeReasons, r)
		}
	}
	// Keep the page's order so the same choice always stores the same way.
	slices.SortFunc(s.EpisodeReasons, func(a, b string) int {
		return cmp.Compare(slices.IndexFunc(episodeReasonOptions, func(o option) bool { return o.Value == a }),
			slices.IndexFunc(episodeReasonOptions, func(o option) bool { return o.Value == b }))
	})
	if in.GroupMinutes != 0 && (in.GroupMinutes < minGroupMinutes || in.GroupMinutes > maxGroupMinutes) {
		return nil, bad(fmt.Sprintf("Grouping: hold new episodes for between %d and %d minutes, or switch it off.", minGroupMinutes, maxGroupMinutes))
	}
	s.GroupMinutes = in.GroupMinutes
	tz, aerr := cleanTimeZone(in.TimeZone)
	if aerr != nil {
		return nil, aerr
	}
	if tz != "" {
		s.TimeZone = tz
	}
	q, aerr := cleanQuiet(in.Quiet)
	if aerr != nil {
		return nil, aerr
	}
	// Quiet hours nobody has touched (off, with the default times) stay unset,
	// so the saved settings of a profile that doesn't use them don't change.
	if q.Enabled || s.Quiet != (QuietHours{}) || q.Start != defaultQuietStart || q.End != defaultQuietEnd {
		s.Quiet = q
		// Only admin alerts can be emergencies, and they follow the quiet hours
		// of the admin's own profile, so only that profile keeps this switch.
		s.Quiet.SilenceEmergency = q.SilenceEmergency && v.admin && prof.ID == v.activeID
	}

	s.Types = map[string]TypeSettings{}
	for _, t := range notificationTypes {
		ts, ok := in.Types[t.Key]
		if !ok {
			ts = t.defaults()
		}
		ts.Title, ts.Message, ts.Sound = strings.TrimSpace(ts.Title), strings.TrimSpace(ts.Message), strings.TrimSpace(ts.Sound)
		ts.GroupTitle, ts.GroupMessage = strings.TrimSpace(ts.GroupTitle), strings.TrimSpace(ts.GroupMessage)
		if !t.groupable() {
			ts.GroupTitle, ts.GroupMessage = "", ""
		}
		switch {
		case len(ts.Title) > 1024 || len(ts.Message) > 2048 || len(ts.GroupTitle) > 1024 || len(ts.GroupMessage) > 2048:
			return nil, bad(t.Label + ": the template is too long.")
		case ts.Priority != nil && !validPriority(*ts.Priority):
			return nil, bad(t.Label + ": priority must be between -2 and 1.")
		case !gotifySoundPattern.MatchString(ts.Sound):
			return nil, bad(t.Label + ": that sound name isn't valid.")
		}
		if ts.Title == "" && ts.Message == "" {
			ts.Title, ts.Message = t.DefaultTitle, t.DefaultMessage
		}
		if ts.GroupTitle == "" && ts.GroupMessage == "" || ts.GroupTitle == t.DefaultGroupTitle && ts.GroupMessage == t.DefaultGroupMessage {
			// Unchanged wording stays unset, so a later change to the default
			// reaches the profile.
			ts.GroupTitle, ts.GroupMessage = "", ""
		}
		s.Types[t.Key] = ts
	}
	s.UpdatedAt = time.Now().UTC()
	return s, nil
}

// Profile levels use the same internal scale as upstream; level 2 is admin-only.
func validPriority(p int) bool { return p >= -2 && p <= 1 }

func (p *Plugin) handlePutProfile(ctx context.Context, v *viewer, prof siloProfile, body []byte) *pluginv1.HandleHTTPResponse {
	var in profileInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the settings."))
	}
	existing, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	s, aerr := p.applyInput(v, prof, existing, in)
	if aerr != nil {
		return errorResponse(aerr)
	}

	// Check the server version, without sending a message or validating the token.
	// If Gotify cannot be reached, save
	// anyway and say so rather than blocking the change.
	warning := ""
	keysChanged := existing == nil || existing.ServerURL != s.ServerURL || effectiveToken(existing, p.config()) != effectiveToken(s, p.config())
	if keysChanged {
		warning = "Server reachable. Send a test notification to verify the application token."
		if _, _, err := p.gotify.Validate(ctx, effectiveToken(s, p.config()), s.ServerURL); err != nil {
			var perr *gotifyError
			if errors.As(err, &perr) && !perr.Retryable && perr.HTTPStatus != 0 {
				return errorResponse(apiErr(http.StatusUnprocessableEntity, "gotify_rejected", err.Error()))
			}
			warning = "Saved, but the Gotify server could not be verified. Send a test notification to check delivery."
		}
	}

	if err := p.store.Put(ctx, s); err != nil {
		return storeError(p, err)
	}
	return jsonResponse(http.StatusOK, map[string]any{"profile": newProfileView(prof, s), "warning": warning})
}

type testInput struct {
	Settings profileInput `json:"settings"`
	Type     string       `json:"type"`
	// Group tests the wording of a merged notification instead.
	Group bool `json:"group"`
}

// handleTest sends a sample of one notification type using the unsaved form
// values, so wording can be tried before saving.
func (p *Plugin) handleTest(ctx context.Context, v *viewer, prof siloProfile, body []byte) *pluginv1.HandleHTTPResponse {
	var in testInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the test request."))
	}
	existing, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	s, aerr := p.applyInput(v, prof, existing, in.Settings)
	if aerr != nil {
		return errorResponse(aerr)
	}

	t := notificationTypeByKey(in.Type)
	ts := s.typeSettings(t.Key)
	vars := map[string]string{"profile": s.ProfileName, "type": t.Key, "type_label": t.Label}
	sample := t.Sample
	title, message := ts.Title, ts.Message
	if in.Group && t.groupable() {
		sample = t.GroupSample
		title, message = ts.groupWording(t)
	}
	for k, val := range sample {
		vars[k] = val
	}
	msg := gotifyMessage{
		Token:    effectiveToken(s, p.config()),
		User:     s.ServerURL,
		Title:    "[Test] " + renderTemplate(title, vars, s.HTML),
		Message:  renderTemplate(message, vars, s.HTML),
		Device:   s.Device,
		Sound:    s.Sound,
		Priority: s.Priority,
		HTML:     s.HTML,
	}
	if ts.Priority != nil {
		msg.Priority = *ts.Priority
	}
	if ts.Sound != "" {
		msg.Sound = ts.Sound
	}
	if s.IncludeLink {
		if base := p.publicBaseURL(ctx); base != "" {
			msg.URL, msg.URLTitle = base, "Open in Silo"
		}
	}
	if err := p.gotify.Send(ctx, msg); err != nil {
		return errorResponse(apiErr(http.StatusBadGateway, "gotify_failed", err.Error()))
	}
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
}

type validateInput struct {
	ServerURL string `json:"server_url"`
	AppToken  string `json:"app_token"`
	// ClearAppToken checks against the shared token, as saving would.
	ClearAppToken bool `json:"clear_app_token"`
}

// handleValidate checks the key pair and returns the account's devices and
// sounds for the pickers.
func (p *Plugin) handleValidate(ctx context.Context, prof siloProfile, body []byte) *pluginv1.HandleHTTPResponse {
	var in validateInput
	_ = json.Unmarshal(body, &in)
	existing, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	user, token := strings.TrimSpace(in.ServerURL), strings.TrimSpace(in.AppToken)
	if existing != nil {
		if user == "" {
			user = existing.ServerURL
		}
		if token == "" && !in.ClearAppToken {
			token = existing.AppToken
		}
	}
	if token == "" {
		token = effectiveToken(&ProfileSettings{ServerURL: user}, p.config())
	}
	if user == "" || token == "" {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Enter your Gotify server URL and application token first."))
	}
	devices, sounds, err := p.gotify.Validate(ctx, token, user)
	if err != nil {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "gotify_rejected", err.Error()))
	}
	if devices == nil {
		devices = []string{}
	}
	return jsonResponse(http.StatusOK, map[string]any{"devices": devices, "sounds": sounds})
}

// adminViewer is an acting admin whose read-only key check passed.
type adminViewer struct {
	userID    int
	profileID string
	username  string
	base      string // Silo's internal URL
	key       string // the read-only key
}

// adminRoutes lists the admin alert routes under /api/admin/ and their
// methods. POST and PUT bodies are JSON (checked in serveAPI).
var adminRoutes = map[string][]string{
	"alerts":      {http.MethodGet, http.MethodPut, http.MethodDelete},
	"alerts/test": {http.MethodPost},
	"users":       {http.MethodGet},
}

// serveAdminAPI answers the admin alert routes. Each needs an acting admin
// and a working read-only Silo key; without the key it says so.
func (p *Plugin) serveAdminAPI(ctx context.Context, req *pluginv1.HandleHTTPRequest, route string) *pluginv1.HandleHTTPResponse {
	methods, ok := adminRoutes[route]
	if !ok {
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
	}
	method := req.GetMethod()
	if !slices.Contains(methods, method) {
		return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
	}
	a, aerr := p.resolveAdmin(ctx, req)
	if aerr != nil {
		return errorResponse(aerr)
	}
	switch route + " " + method {
	case "alerts GET":
		return p.handleGetAdminAlerts(ctx, a)
	case "alerts PUT":
		return p.handlePutAdminAlerts(ctx, a, req.GetBody())
	case "alerts DELETE":
		if err := p.admins.Delete(ctx, a.userID); err != nil {
			return storeError(p, err)
		}
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
	case "alerts/test POST":
		return p.handleTestAdminAlert(ctx, a, req.GetBody())
	default: // users GET
		return p.handleListUsers(ctx, a)
	}
}

// resolveAdmin checks that the viewer is an acting admin and that the
// read-only key works, by looking up the viewer's own account with it.
func (p *Plugin) resolveAdmin(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*adminViewer, *apiError) {
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if userID <= 0 {
		return nil, apiErr(http.StatusUnauthorized, "unauthenticated", "Your session for this page has expired. Open Surify from the administrator navigation again.")
	}
	if header(req, "X-Silo-User-Role") != "admin" || header(req, "X-Silo-Profile-Primary") != "true" {
		return nil, apiErr(http.StatusForbidden, "forbidden", "Only a server admin, signed in to their primary profile, can set up admin alerts.")
	}
	a := &adminViewer{userID: userID, profileID: header(req, "X-Silo-Profile-Id"), username: header(req, "X-Silo-User-Name")}
	if a.profileID == "" {
		return nil, apiErr(http.StatusConflict, "no_profile", "Choose a profile in Silo, then open Gotify from the sidebar again.")
	}
	a.key = p.config().MonitorAPIKey
	if a.key == "" {
		return nil, monitorKeyProblem(errSiloKeyMissing)
	}
	info, err := p.host(ctx)
	if err != nil || info.InternalBaseURL == "" {
		return nil, apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment.")
	}
	a.base = strings.TrimRight(info.InternalBaseURL, "/")
	user, err := p.silo.CurrentUser(ctx, a.base, a.key, userID)
	switch {
	case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
		return nil, monitorKeyProblem(err)
	case errors.Is(err, errSiloUserNotFound), err == nil && !user.isAdmin():
		return nil, apiErr(http.StatusForbidden, "forbidden", "Silo doesn't list your account as an enabled admin, so you can't set up admin alerts.")
	case err != nil:
		p.logger.Warn("could not check the admin's account with the read-only Silo key", "user_id", userID, "error", err)
		return nil, apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't reach Silo to check the read-only key. Reload the page to try again.")
	}
	if a.username == "" {
		a.username = user.Username
	}
	return a, nil
}

// monitorKeyProblem explains why admin alerts can't run without a working
// read-only key. The page shows the key setup box for these codes.
func monitorKeyProblem(err error) *apiError {
	const fix = " Use “Set up the read-only key” in the admin section of this page, or add a key under " + adminFormPath + "."
	switch {
	case errors.Is(err, errSiloKeyRejected):
		return apiErr(http.StatusConflict, "monitor_key_rejected", "Silo rejected the plugin's read-only Silo key, so no admin alerts are sent. The key may have been revoked."+fix)
	case errors.Is(err, errSiloKeyScope):
		return apiErr(http.StatusConflict, "monitor_key_scope", "The plugin's read-only Silo key isn't allowed to look up accounts, so no admin alerts are sent. It needs the admin:monitor:read scope."+fix)
	}
	return apiErr(http.StatusConflict, "monitor_key_missing", "Admin alerts need a read-only Silo key (scope admin:monitor:read), which isn't set up yet. The plugin uses it to check that each recipient is still an admin."+fix)
}

// adminSettingsView fills in every alert type and empty lists, for the page.
func adminSettingsView(s *AdminSettings) *AdminSettings {
	v := s.clone()
	if v.Alerts == nil {
		v.Alerts = map[string]AlertSettings{}
	}
	for _, t := range adminAlertTypes {
		v.Alerts[t.Key] = s.alert(t.Key)
	}
	if v.Filters.IgnoreUsers == nil {
		v.Filters.IgnoreUsers = []string{}
	}
	if v.Filters.MediaTypes == nil {
		v.Filters.MediaTypes = []string{}
	}
	if v.Filters.Libraries == nil {
		v.Filters.Libraries = []string{}
	}
	// Show what applies: the defaults for anything never set.
	digest := s.digestSchedule()
	v.Digest = &digest
	link := s.siloLink()
	v.SiloLink = &link
	if v.Quiet.Start == "" {
		v.Quiet.Start = defaultQuietStart
	}
	if v.Quiet.End == "" {
		v.Quiet.End = defaultQuietEnd
	}
	return v
}

// adminGotifyReady reports whether the admin's profile has a Gotify user
// key and a token to send with.
func (p *Plugin) adminGotifyReady(ctx context.Context, profileID string) (*ProfileSettings, bool, error) {
	prof, found, err := p.store.Get(ctx, profileID)
	if err != nil || !found {
		return nil, false, err
	}
	return prof, prof.AdminManaged && prof.ServerURL != "" && effectiveToken(prof, p.config()) != "", nil
}

const adminGotifyMissing = "Admin alerts are sent with your profile's Gotify settings. Save your administrator connection on the Connection tab first. Existing connections must be saved once after the admin-only upgrade."

func (p *Plugin) handleGetAdminAlerts(ctx context.Context, a *adminViewer) *pluginv1.HandleHTTPResponse {
	settings, found, err := p.admins.Get(ctx, a.userID)
	if err != nil {
		return storeError(p, err)
	}
	if !found {
		settings = defaultAdminSettings()
		settings.UserID, settings.ProfileID, settings.Username = a.userID, a.profileID, a.username
	} else if p.moveQuietToProfile(ctx, settings) {
		if err := p.admins.Put(ctx, settings); err != nil {
			p.logger.Warn("could not clear admin quiet hours after moving them to the profile", "user_id", a.userID, "error", err)
		}
	}
	_, ready, err := p.adminGotifyReady(ctx, a.profileID)
	if err != nil {
		return storeError(p, err)
	}
	// Libraries for the library filter; the page keeps saved ones it can't
	// name.
	libs, err := p.libraries(ctx)
	if err != nil {
		p.logger.Warn("could not list Silo's libraries for the library filter", "error", err)
	}
	if libs == nil {
		libs = []siloLibrary{}
	}
	out := map[string]any{
		"configured":   found,
		"settings":     adminSettingsView(settings),
		"alert_types":  adminAlertTypes,
		"noise_modes":  noiseModes,
		"media_types":  mediaTypeOptions,
		"libraries":    libs,
		"sounds":       builtinSounds,
		"gotify_ready": ready,
	}
	if !ready {
		out["gotify_message"] = adminGotifyMissing
	}
	return jsonResponse(http.StatusOK, out)
}

func (p *Plugin) handlePutAdminAlerts(ctx context.Context, a *adminViewer, body []byte) *pluginv1.HandleHTTPResponse {
	var in adminInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the settings."))
	}
	existing, _, loadErr := p.admins.Get(ctx, a.userID)
	if loadErr != nil {
		return storeError(p, loadErr)
	}
	// A browser that doesn't report its time zone keeps the saved one.
	if strings.TrimSpace(in.TimeZone) == "" {
		if existing != nil {
			in.TimeZone = existing.TimeZone
		}
	}
	s, aerr := applyAdminInput(a.userID, a.profileID, a.username, in)
	if aerr != nil {
		return errorResponse(aerr)
	}
	if s.alert(alertRecentlyAdded).Enabled {
		s.RecentSince = p.now()
		if existing != nil && existing.alert(alertRecentlyAdded).Enabled && !existing.RecentSince.IsZero() {
			s.RecentSince = existing.RecentSince
		}
	}
	if err := p.admins.Put(ctx, s); err != nil {
		return storeError(p, err)
	}
	warning := ""
	if _, ready, err := p.adminGotifyReady(ctx, a.profileID); err == nil && !ready && s.anyEnabled() {
		warning = "Saved. " + adminGotifyMissing
	}
	return jsonResponse(http.StatusOK, map[string]any{"settings": adminSettingsView(s), "warning": warning})
}

type adminTestInput struct {
	Settings adminInput `json:"settings"`
	Alert    string     `json:"alert"`
}

// handleTestAdminAlert sends a sample of one alert with the unsaved form
// values, to the admin's profile.
func (p *Plugin) handleTestAdminAlert(ctx context.Context, a *adminViewer, body []byte) *pluginv1.HandleHTTPResponse {
	var in adminTestInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the test request."))
	}
	t, ok := adminAlertTypeByKey(in.Alert)
	if !ok {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Unknown alert."))
	}
	s, aerr := applyAdminInput(a.userID, a.profileID, a.username, in.Settings)
	if aerr != nil {
		return errorResponse(aerr)
	}
	prof, ready, err := p.adminGotifyReady(ctx, a.profileID)
	if err != nil {
		return storeError(p, err)
	}
	if !ready {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "gotify_not_set_up", adminGotifyMissing))
	}
	msg := p.adminPush(ctx, s, prof, t.Key, t.Sample, time.Time{})
	msg.Token = effectiveToken(prof, p.config())
	msg.Title = "[Test] " + msg.Title
	// A test shows the wording and sound, so quiet hours don't apply, and an
	// Preserve the upstream test behavior: urgent alerts are tested at high priority.
	if msg.Priority == priorityEmergency {
		msg.Priority = 1
	}
	if err := p.gotify.Send(ctx, msg); err != nil {
		return errorResponse(apiErr(http.StatusBadGateway, "gotify_failed", err.Error()))
	}
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
}

// handleListUsers lists Silo's accounts for the "users to ignore" picker.
func (p *Plugin) handleListUsers(ctx context.Context, a *adminViewer) *pluginv1.HandleHTTPResponse {
	users, err := p.silo.Users(ctx, a.base, a.key)
	switch {
	case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
		return errorResponse(monitorKeyProblem(err))
	case err != nil:
		p.logger.Warn("could not list Silo accounts with the read-only key", "error", err)
		return errorResponse(apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't reach Silo to list accounts. Try again."))
	}
	type userView struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Enabled  bool   `json:"enabled"`
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, userView{ID: u.ID, Username: u.Username, Role: u.Role, Enabled: u.Enabled})
	}
	slices.SortFunc(out, func(x, y userView) int {
		return strings.Compare(strings.ToLower(x.Username), strings.ToLower(y.Username))
	})
	return jsonResponse(http.StatusOK, map[string]any{"users": out})
}

func storeError(p *Plugin, err error) *pluginv1.HandleHTTPResponse {
	if errors.Is(err, errHostUnavailable) {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}
	p.logger.Error("settings storage failed", "error", err)
	return errorResponse(apiErr(http.StatusInternalServerError, "storage_failed", "Couldn't read or save settings."))
}

func header(req *pluginv1.HandleHTTPRequest, name string) string {
	for k, v := range req.GetHeaders() {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func jsonResponse(status int, v any) *pluginv1.HandleHTTPResponse {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"code":"internal","message":"Internal error."}`)
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       body,
	}
}

func errorResponse(e *apiError) *pluginv1.HandleHTTPResponse {
	return jsonResponse(e.status, e)
}
