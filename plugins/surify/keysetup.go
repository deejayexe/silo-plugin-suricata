package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

var siloKeyPattern = regexp.MustCompile(`^sa_[\x21-\x7e]{8,256}$`)

// Old flags are accepted only when false, so a stale page cannot revoke the
// credential that the single-key configuration now keeps and uses.
type keySetupInput struct {
	Source  string `json:"source"`
	Key     string `json:"key"`
	Revoke  bool   `json:"revoke"`
	Control bool   `json:"control"`
}

type adminKeyStatus struct {
	AdminKeySet     bool `json:"admin_key_set"`
	MonitorKeySet   bool `json:"monitor_key_set"`
	ControlKeySet   bool `json:"control_key_set"`
	LegacyKeyStored bool `json:"legacy_key_stored"`
}

func (p *Plugin) adminKeyStatus() adminKeyStatus {
	c := p.config()
	return adminKeyStatus{AdminKeySet: c.AdminAPIKey != "", MonitorKeySet: c.monitorKey() != "", ControlKeySet: c.controlKey() != "", LegacyKeyStored: c.LegacyKeyStored}
}

type keySetupResult struct {
	Saved   bool           `json:"saved"`
	Level   string         `json:"level"`
	Title   string         `json:"title"`
	Details []string       `json:"details"`
	Keys    adminKeyStatus `json:"keys"`
}

// handleKeySetup validates and saves one administrator credential. All probes
// are read-only; no key is generated or revoked, including on validation failure.
func (p *Plugin) handleKeySetup(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	if err := requireAdminRequest(req); err != nil {
		return errorResponse(err)
	}
	var in keySetupInput
	if err := json.Unmarshal(req.GetBody(), &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the request."))
	}
	if in.Revoke || in.Control {
		return errorResponse(apiErr(http.StatusConflict, "key_setup_changed", "Surify now saves one administrator key. Reload the page before saving. No keys were changed."))
	}
	if !p.configured.Load() {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}
	if !p.keySetupMu.TryLock() {
		return errorResponse(apiErr(http.StatusConflict, "busy", "A key is already being saved. Try again in a moment."))
	}
	defer p.keySetupMu.Unlock()
	key := strings.TrimSpace(in.Key)
	switch in.Source {
	case "", "paste":
		if key == "" {
			key = p.config().AdminAPIKey
		}
	case "legacy":
		key = p.config().legacyAPIKey
	default:
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Unknown key source."))
	}
	if !siloKeyPattern.MatchString(key) {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Enter a complete Silo administrator API key starting with sa_."))
	}
	info, err := p.host(ctx)
	if err != nil || info.InternalBaseURL == "" {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}
	base := strings.TrimRight(info.InternalBaseURL, "/")
	// This administrative GET is unavailable to the old scoped monitor/control
	// keys. The response is discarded and never exposed to the browser.
	if err = p.keys.Probe(ctx, base, key, "/api/v2/admin/api-keys?limit=1"); err != nil {
		if errors.Is(err, errSiloKeyRejected) || errors.Is(err, errSiloKeyScope) {
			return errorResponse(apiErr(http.StatusUnprocessableEntity, "key_rejected", "Use an unrestricted API key from an enabled administrator account. The previous key remains unchanged."))
		}
		return errorResponse(apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't validate the administrator key with Silo. Nothing was changed."))
	}
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	user, err := p.silo.CurrentUser(ctx, base, key, userID)
	if err != nil {
		return errorResponse(apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't verify your administrator account with Silo. Nothing was changed."))
	}
	if !user.isAdmin() {
		return errorResponse(apiErr(http.StatusForbidden, "forbidden", "Only an enabled administrator on the primary profile can save this key."))
	}
	next := p.config()
	next.AdminAPIKey = key
	next.MonitorAPIKey = ""
	next.ControlAPIKey = ""
	next.legacyAPIKey = ""
	next.LegacyKeyStored = false
	if err = p.saveConfig(ctx, next); err != nil {
		p.logger.Warn("could not save the administrator API key", "user_id", userID)
		return errorResponse(apiErr(http.StatusInternalServerError, "save_failed", "Couldn't save the administrator key. The previous configuration remains active."))
	}
	return jsonResponse(http.StatusOK, keySetupResult{Saved: true, Level: "ok", Title: "Administrator API key saved.", Details: []string{"The same key is used for monitoring and stream-control actions. No API keys were created or revoked."}, Keys: p.adminKeyStatus()})
}

func isTimeout(err error) bool {
	var nerr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout())
}
