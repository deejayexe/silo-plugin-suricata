package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// These identity headers are set by Silo's authenticated plugin proxy, which
// strips browser-supplied copies. Never accept identity in query/body fields.
func requireAdminRequest(req *pluginv1.HandleHTTPRequest) *apiError {
	id, err := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if err != nil || id <= 0 {
		return apiErr(http.StatusUnauthorized, "unauthenticated", "Sign in to Silo as an administrator to use Surify.")
	}
	if header(req, "X-Silo-User-Role") != "admin" || header(req, "X-Silo-Profile-Primary") != "true" {
		return apiErr(http.StatusForbidden, "forbidden", "Surify is available only to administrators on their primary profile.")
	}
	return nil
}

// Fail closed on pre-upgrade connections and check the owner's current role
// with Silo immediately before background delivery. A missing/revoked key
// pauses delivery; a temporary outage asks Silo (or the flusher) to retry.
func (p *Plugin) adminDestinationAllowed(ctx context.Context, s *ProfileSettings) (bool, error) {
	if s == nil || !s.AdminManaged || s.UserID <= 0 {
		return false, nil
	}
	key := p.config().monitorKey()
	if key == "" {
		return false, nil
	}
	base, err := p.siloBase(ctx)
	if err != nil {
		return false, err
	}
	user, err := p.silo.CurrentUser(ctx, base, key, s.UserID)
	switch {
	case errors.Is(err, errSiloUserNotFound), errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
		return false, nil
	case err != nil:
		return false, err
	default:
		return user.isAdmin(), nil
	}
}
