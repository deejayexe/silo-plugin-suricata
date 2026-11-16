package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Key setup lets an acting admin give the plugin its scoped Silo keys from
// the Gotify page. Silo's Admin > API Keys page only makes full-access
// keys, so the admin pastes one, the plugin uses it to create a key with only
// admin:monitor:read (and optionally admin:sessions:control), and can then
// revoke the pasted key.
//
// The steps run in the order create → verify → save → revoke, so a failure
// never leaves the plugin without a working key. The pasted key and the keys
// Silo creates are never logged, stored (other than the new keys in the
// plugin's config), returned, or put in an error message.

const (
	monitorKeyLabel = "Gotify read-only"
	controlKeyLabel = "Gotify stream control"
	apiKeysPage     = "Admin > API Keys"

	// keySetupFinishBudget bounds the steps after the first key exists.
	// They run without the page request's 10 s deadline, so a slow request
	// can't stop between creating a key and saving it.
	keySetupFinishBudget = 20 * time.Second
)

// siloKeyPattern accepts what Silo treats as an API key: a bearer starting
// with "sa_" (silo-server internal/api/middleware/auth.go RequireAuth).
var siloKeyPattern = regexp.MustCompile(`^sa_[\x21-\x7e]{8,256}$`)

// Routes used to check a key. The profile list is the call the plugin makes
// with its read-only key. Among scoped keys, only admin:monitor:read may read
// the host resources capabilities, so passing both shows that scope.
func monitorProbePaths(userID int) []string {
	return []string{
		"/api/v2/admin/users/" + strconv.Itoa(userID) + "/profiles",
		"/api/v2/admin/system/resources/capabilities",
	}
}

// controlProbePath is a GET that admin:sessions:control allows. Its other
// routes stop or message streams, so they can't be used to check the key.
const controlProbePath = "/api/v2/admin/sessions/command-capabilities"

type keySetupInput struct {
	// Source is "paste" (the default) or "legacy", which uses the key
	// version 0.2.0 saved and always revokes it.
	Source string `json:"source"`
	Key    string `json:"key"`
	// Revoke asks to revoke the pasted key once the new one is saved.
	Revoke bool `json:"revoke"`
	// Control also creates a key with admin:sessions:control.
	Control bool `json:"control"`
}

// adminKeyStatus tells an acting admin's page which keys are stored, never
// the keys themselves.
type adminKeyStatus struct {
	MonitorKeySet   bool `json:"monitor_key_set"`
	ControlKeySet   bool `json:"control_key_set"`
	LegacyKeyStored bool `json:"legacy_key_stored"`
}

func (p *Plugin) adminKeyStatus() adminKeyStatus {
	cfg := p.config()
	return adminKeyStatus{MonitorKeySet: cfg.MonitorAPIKey != "", ControlKeySet: cfg.ControlAPIKey != "", LegacyKeyStored: cfg.LegacyKeyStored}
}

// keySetupResult reports a setup that changed the plugin's settings. Setups
// that changed nothing answer with an error instead.
type keySetupResult struct {
	// Saved is true when a read-only key is now saved.
	Saved   bool           `json:"saved"`
	Level   string         `json:"level"` // "ok" or "warn"
	Title   string         `json:"title"`
	Details []string       `json:"details"`
	Keys    adminKeyStatus `json:"keys"`
}

// handleKeySetup is POST /api/admin/key-setup. serveAPI has already checked
// that the body is JSON.
func (p *Plugin) handleKeySetup(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if userID <= 0 {
		return errorResponse(apiErr(http.StatusUnauthorized, "unauthenticated", "Your session for this page has expired. Open Surify from the administrator navigation again."))
	}
	// Silo's proxy sets this to "admin" only for an admin account on its
	// primary profile.
	if header(req, "X-Silo-User-Role") != "admin" || header(req, "X-Silo-Profile-Primary") != "true" {
		return errorResponse(apiErr(http.StatusForbidden, "forbidden", "Only a server admin, signed in to their primary profile, can set up Silo keys."))
	}
	var in keySetupInput
	if err := json.Unmarshal(req.GetBody(), &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the request."))
	}
	if !p.configured.Load() {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}
	if !p.keySetupMu.TryLock() {
		return errorResponse(apiErr(http.StatusConflict, "busy", "A key setup is already running. Wait for it to finish, then reload the page."))
	}
	defer p.keySetupMu.Unlock()

	s := &keySetup{p: p, userID: userID, control: in.Control}
	switch in.Source {
	case "legacy":
		key := p.config().legacyAPIKey
		if key == "" {
			return errorResponse(apiErr(http.StatusConflict, "no_legacy_key", "There's no full-access key from version 0.2.0 stored any more. Reload the page."))
		}
		s.key, s.legacy, s.revoke = key, true, true
	case "", "paste":
		key := strings.TrimSpace(in.Key)
		switch {
		case key == "":
			return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Paste a full-access Silo API key first."))
		case !siloKeyPattern.MatchString(key):
			return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "That doesn't look like a Silo API key. Silo keys start with sa_ and are shown once, when the key is created."))
		}
		s.key, s.revoke = key, in.Revoke
	default:
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Unknown key source."))
	}
	s.secrets = []string{s.key}

	info, err := p.host(ctx)
	if err != nil || info.InternalBaseURL == "" {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}
	s.base = strings.TrimRight(info.InternalBaseURL, "/")

	res, aerr := s.run(ctx)
	if aerr != nil {
		return errorResponse(aerr)
	}
	res.Keys = p.adminKeyStatus()
	return jsonResponse(http.StatusOK, res)
}

// keySetup is one run of the setup. key is the full-access key: it is only
// ever sent to Silo as a bearer token.
type keySetup struct {
	p      *Plugin
	base   string
	userID int
	key    string
	// legacy means key is the one version 0.2.0 saved.
	legacy, revoke, control bool
	// secrets are scrubbed from every message: the full-access key and the
	// keys this run created.
	secrets []string

	details []string
	warned  bool
}

func (s *keySetup) run(ctx context.Context) (*keySetupResult, *apiError) {
	keys, err := s.p.keys.List(ctx, s.base, s.key)
	switch {
	case errors.Is(err, errSiloKeyRejected):
		if s.legacy {
			return s.forgetLegacyKey(ctx)
		}
		return nil, apiErr(http.StatusUnprocessableEntity, "key_rejected", "Silo didn't accept this key. Check that you copied all of it and that it hasn't been revoked.")
	case errors.Is(err, errSiloKeyScope):
		// Scoped keys may not list keys, and neither may non-admin accounts.
		return s.useScopedKey(ctx)
	case err != nil:
		return nil, s.siloFailed("check the key with Silo", err)
	}

	matches := matchingKeys(keys, s.key)
	if len(matches) == 1 && slices.Contains(matches[0].Scopes, scopeMonitorRead) {
		// Today's Silo never gets here (a scoped key can't list keys).
		return s.saveExistingKey(ctx)
	}
	return s.createKeys(ctx, matches)
}

// matchingKeys returns the listed keys whose prefix the full key starts with.
// Silo lists the first 11 characters ("sa_" and 8 more).
func matchingKeys(keys []siloAPIKey, key string) []siloAPIKey {
	var out []siloAPIKey
	for _, k := range keys {
		if len(k.KeyPrefix) >= 8 && strings.HasPrefix(key, k.KeyPrefix) {
			out = append(out, k)
		}
	}
	return out
}

// useScopedKey handles a key Silo won't let list keys: saved as is if it
// already has admin:monitor:read, refused otherwise.
func (s *keySetup) useScopedKey(ctx context.Context) (*keySetupResult, *apiError) {
	ok, err := s.works(ctx, s.key, monitorProbePaths(s.userID)...)
	if err != nil {
		return nil, s.siloFailed("check the key with Silo", err)
	}
	if !ok {
		msg := "This key can't create API keys. Paste a full-access key made on an admin account under " + apiKeysPage + "."
		if s.legacy {
			msg = "The full-access key saved by version 0.2.0 can't create API keys any more (its account may no longer be an admin). Paste a full-access key made on an admin account instead."
		}
		return nil, apiErr(http.StatusUnprocessableEntity, "key_not_full_access", msg)
	}
	return s.saveExistingKey(ctx)
}

// saveExistingKey saves a key that already has admin:monitor:read. Nothing
// is created or revoked.
func (s *keySetup) saveExistingKey(ctx context.Context) (*keySetupResult, *apiError) {
	prev := s.p.config()
	next := prev.withoutLegacyKey()
	next.MonitorAPIKey = s.key
	if err := s.p.saveConfig(ctx, next); err != nil {
		s.logFailure("save", err)
		return nil, apiErr(http.StatusInternalServerError, "save_failed", "Couldn't save the key in the plugin's settings, so nothing was changed. Try again.")
	}
	what := "This key"
	if s.legacy {
		what = "The key saved by version 0.2.0"
	}
	s.note(false, what+" already has the admin:monitor:read scope, so it's now saved as the plugin's read-only key. Nothing was created or revoked.")
	if s.control {
		s.note(true, "A scoped key can't create other keys, so no stream-control key was made. To add one, paste a full-access key.")
	}
	s.noteReplaced(prev, next)
	s.p.logger.Info("read-only Silo key saved from the plugin page", "user_id", s.userID, "created", false)
	return s.result(true, "Read-only key saved."), nil
}

// createKeys makes the scoped key(s), checks and saves them, then revokes
// the full-access key if asked.
func (s *keySetup) createKeys(ctx context.Context, matches []siloAPIKey) (*keySetupResult, *apiError) {
	monitor, err := s.p.keys.Create(ctx, s.base, s.key, s.userID, monitorKeyLabel, []string{scopeMonitorRead})
	if monitor != nil && monitor.Key != "" {
		s.secrets = append(s.secrets, monitor.Key)
	}
	if err != nil {
		left := ""
		if monitor != nil && validKeyID(monitor.ID) {
			left = s.cleanupText(s.discard(context.WithoutCancel(ctx), monitor))
		}
		s.logFailure("create read-only key", err)
		if isTimeout(err) {
			// Silo may have made the key before the answer was lost.
			return nil, apiErr(http.StatusBadGateway, "create_failed", s.scrub("Silo took too long to answer, so the plugin can't tell whether it created the read-only key. An unused “"+monitorKeyLabel+"” key may be left under "+apiKeysPage+"; delete it there if you see one. No key was saved. "+left))
		}
		return nil, apiErr(http.StatusBadGateway, "create_failed", s.scrub("Silo couldn't create the read-only key: "+describeSiloError(err)+". Nothing was changed. "+left))
	}

	// A key exists now: finish without the page request's deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keySetupFinishBudget)
	defer cancel()

	var control *createdAPIKey
	controlFailed := false
	if s.control {
		control, err = s.p.keys.Create(ctx, s.base, s.key, s.userID, controlKeyLabel, []string{scopeSessionsControl})
		if control != nil && control.Key != "" {
			s.secrets = append(s.secrets, control.Key)
		}
		if err != nil {
			controlFailed = true
			left := ""
			if control != nil && validKeyID(control.ID) {
				left = " " + s.cleanupText(s.discard(ctx, control))
			}
			s.logFailure("create stream-control key", err)
			if isTimeout(err) {
				left += " An unused “" + controlKeyLabel + "” key may be left under " + apiKeysPage + "; delete it there if you see one."
			}
			s.note(true, "Silo couldn't create the stream-control key: "+describeSiloError(err)+"."+left)
			control = nil
		}
	}

	if ok, err := s.works(ctx, monitor.Key, monitorProbePaths(s.userID)...); !ok {
		created := []*createdAPIKey{monitor}
		if control != nil {
			created = append(created, control)
		}
		left := s.cleanupText(s.discard(ctx, created...))
		reason := "Silo refused it"
		if err != nil {
			reason = describeSiloError(err)
		}
		s.logFailure("check the new read-only key", errors.New(reason))
		return nil, apiErr(http.StatusBadGateway, "verify_failed", s.scrub("The new read-only key didn't work when the plugin tried it ("+reason+"), so nothing was saved. "+left))
	}
	if control != nil {
		if ok, err := s.works(ctx, control.Key, controlProbePath); !ok {
			reason := "Silo refused it"
			if err != nil {
				reason = describeSiloError(err)
			}
			controlFailed = true
			s.logFailure("check the new stream-control key", errors.New(reason))
			s.note(true, "The new stream-control key didn't work when the plugin tried it ("+reason+"), so it wasn't saved. "+s.cleanupText(s.discard(ctx, control)))
			control = nil
		}
	}

	prev := s.p.config()
	next := prev.withoutLegacyKey()
	next.MonitorAPIKey = monitor.Key
	if control != nil {
		next.ControlAPIKey = control.Key
	}
	if err := s.p.saveConfig(ctx, next); err != nil {
		s.logFailure("save", err)
		created := []*createdAPIKey{monitor}
		if control != nil {
			created = append(created, control)
		}
		return nil, apiErr(http.StatusInternalServerError, "save_failed", "Couldn't save the new key in the plugin's settings, so nothing was changed. "+s.cleanupText(s.discard(ctx, created...)))
	}
	s.details = append([]string{"Created “" + monitorKeyLabel + "” on your account with only the admin:monitor:read scope, checked that it works, and saved it."}, s.details...)
	if control != nil {
		s.note(false, "Also created and saved “"+controlKeyLabel+"” (admin:sessions:control) for stopping streams.")
	}
	s.noteReplaced(prev, next)

	revokedID := s.revokeFullAccessKey(ctx, matches, controlFailed)
	logArgs := []any{"user_id", s.userID, "created", true, "monitor_key_id", monitor.ID, "revoked_key_id", revokedID}
	if control != nil {
		logArgs = append(logArgs, "control_key_id", control.ID)
	}
	s.p.logger.Info("read-only Silo key set up from the plugin page", logArgs...)
	return s.result(true, "Read-only key set up."), nil
}

// revokeFullAccessKey revokes the key the setup used, when asked and when it
// can be identified for certain: exactly one listed key has its prefix, and
// that key is unscoped. It returns the revoked key's ID, or "".
func (s *keySetup) revokeFullAccessKey(ctx context.Context, matches []siloAPIKey, keepForRetry bool) string {
	what := "The full-access key you pasted"
	if s.legacy {
		what = "The full-access key saved by version 0.2.0"
	}
	switch {
	case !s.revoke:
		s.note(false, what+" still works. Revoke it under "+apiKeysPage+" when you no longer need it.")
		return ""
	case keepForRetry:
		s.note(true, what+" wasn't revoked, so you can try the stream-control key again. Revoke it under "+apiKeysPage+" when you're done.")
		return ""
	case len(matches) == 0:
		s.note(true, what+" wasn't revoked: it isn't in Silo's key list, so the plugin couldn't tell which key it is. Revoke it under "+apiKeysPage+".")
		return ""
	case len(matches) > 1:
		s.note(true, fmt.Sprintf("%s wasn't revoked: %d keys in Silo start with the same characters, so the plugin couldn't tell which one it is. Revoke it under %s.", what, len(matches), apiKeysPage))
		return ""
	case len(matches[0].Scopes) > 0:
		s.note(true, what+" wasn't revoked: Silo lists it as a scoped key, not a full-access one. Revoke it under "+apiKeysPage+" if you don't need it.")
		return ""
	}
	target := matches[0]
	named := strings.ToLower(what[:1]) + what[1:]
	if label := strings.TrimSpace(target.Label); label != "" {
		named += " (“" + label + "”)"
	}
	err := s.p.keys.Delete(ctx, s.base, s.key, target.ID)
	var serr *siloStatusError
	switch {
	case err == nil:
		s.note(false, "Revoked "+named+" in Silo.")
		return target.ID
	case errors.As(err, &serr) && serr.Status == http.StatusNotFound:
		s.note(false, "Silo had already revoked "+named+".")
		return ""
	default:
		s.logFailure("revoke the full-access key", err)
		s.note(true, "The new key is saved, but "+named+" couldn't be revoked ("+describeSiloError(err)+"). Revoke it under "+apiKeysPage+".")
		return ""
	}
}

// forgetLegacyKey drops 0.2.0's key from the plugin's config once Silo no
// longer accepts it, so the page stops offering it.
func (s *keySetup) forgetLegacyKey(ctx context.Context) (*keySetupResult, *apiError) {
	if err := s.p.saveConfig(ctx, s.p.config().withoutLegacyKey()); err != nil {
		s.logFailure("save", err)
		return nil, apiErr(http.StatusInternalServerError, "save_failed", "Silo no longer accepts the full-access key saved by version 0.2.0, but the plugin couldn't remove it from its settings. Try again.")
	}
	s.p.logger.Info("removed the rejected 0.2.0 Silo key from the plugin config", "user_id", s.userID)
	s.note(false, "Silo no longer accepts the full-access key saved by version 0.2.0 (it was probably revoked already), so the plugin removed it from its settings.")
	s.note(true, "No read-only key was created. To create one, paste a full-access key below.")
	return s.result(false, "Old key removed."), nil
}

// works reports whether key may call every path. 401 and 403 mean no; other
// failures are returned as errors.
func (s *keySetup) works(ctx context.Context, key string, paths ...string) (bool, error) {
	for _, path := range paths {
		err := s.p.keys.Probe(ctx, s.base, key, path)
		switch {
		case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
			return false, nil
		case err != nil:
			return false, err
		}
	}
	return true, nil
}

// discard revokes keys this run created and returns the labels of any that
// couldn't be revoked.
func (s *keySetup) discard(ctx context.Context, keys ...*createdAPIKey) []string {
	var left []string
	for _, k := range keys {
		err := s.p.keys.Delete(ctx, s.base, s.key, k.ID)
		var serr *siloStatusError
		if err != nil && !(errors.As(err, &serr) && serr.Status == http.StatusNotFound) {
			s.logFailure("remove a key the setup created", err)
			left = append(left, k.Label)
		}
	}
	return left
}

func (s *keySetup) cleanupText(left []string) string {
	if len(left) == 0 {
		return "The plugin removed the key it had just created."
	}
	return "The plugin couldn't remove “" + strings.Join(left, "” and “") + "”, which it had just created; revoke it under " + apiKeysPage + "."
}

// noteReplaced points out keys the new config no longer uses.
func (s *keySetup) noteReplaced(prev, next adminConfig) {
	if prev.MonitorAPIKey != "" && prev.MonitorAPIKey != next.MonitorAPIKey {
		s.note(false, "The plugin's previous read-only key is no longer used. Revoke it under "+apiKeysPage+" unless something else uses it.")
	}
	if prev.ControlAPIKey != "" && prev.ControlAPIKey != next.ControlAPIKey {
		s.note(false, "The plugin's previous stream-control key is no longer used. Revoke it under "+apiKeysPage+" unless something else uses it.")
	}
	if prev.LegacyKeyStored && !s.legacy && prev.legacyAPIKey != s.key {
		s.note(true, "The full-access key saved by version 0.2.0 was removed from the plugin's settings, but it still works in Silo. Revoke it under "+apiKeysPage+".")
	}
}

func (s *keySetup) siloFailed(action string, err error) *apiError {
	s.logFailure(action, err)
	return apiErr(http.StatusBadGateway, "silo_failed", s.scrub("Couldn't "+action+": "+describeSiloError(err)+". Nothing was changed."))
}

func (s *keySetup) note(warn bool, text string) {
	s.details = append(s.details, s.scrub(text))
	s.warned = s.warned || warn
}

func (s *keySetup) result(saved bool, title string) *keySetupResult {
	level := "ok"
	if s.warned {
		level = "warn"
	}
	return &keySetupResult{Saved: saved, Level: level, Title: title, Details: s.details}
}

// logFailure logs a failed step without any key.
func (s *keySetup) logFailure(step string, err error) {
	s.p.logger.Warn("Silo key setup step failed", "step", step, "user_id", s.userID, "error", s.scrub(err.Error()))
}

// scrub replaces any key this run handles. Silo's messages shouldn't contain
// one, but they are passed on to the page, so make sure.
func (s *keySetup) scrub(text string) string {
	for _, secret := range s.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[key]")
		}
	}
	return text
}

// isTimeout reports whether a Silo call ran out of time, so its outcome is
// unknown.
func isTimeout(err error) bool {
	var nerr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout())
}

// describeSiloError explains a failed Silo call in a few words.
func describeSiloError(err error) string {
	var serr *siloStatusError
	switch {
	case errors.Is(err, errSiloKeyRejected):
		return "Silo didn't accept the key (HTTP 401)"
	case errors.Is(err, errSiloKeyScope):
		return "the key isn't allowed to do that (HTTP 403)"
	case errors.Is(err, context.DeadlineExceeded):
		return "Silo took too long to answer"
	case errors.As(err, &serr):
		return serr.Error()
	}
	return err.Error()
}
