package main

import (
	"fmt"
	"log/slog"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// adminConfigKey is the global_config_schema key in manifest.json.
const adminConfigKey = "gotify"

// legacyAPIKeyField is the unscoped admin key 0.2.0 asked for. Silo keeps a
// saved value after an upgrade (an admin save merges over the stored row and
// can't clear undeclared fields), so it still arrives in Configure. Its value
// is used for one thing only: the page's "Replace the old full-access key"
// setup, which revokes it and rewrites the config without it.
const legacyAPIKeyField = "silo_api_key"

// Config fields as stored (manifest.json global_config_schema "gotify").
const (
	fieldMonitorAPIKey    = "monitor_api_key"
	fieldControlAPIKey    = "control_api_key"
	fieldDefaultAppToken  = "default_app_token"
	fieldDefaultServerURL = "default_server_url"
	fieldPublicURL        = "public_url"
)

// adminConfig is the server-wide configuration an admin sets under
// Admin > Plugins > Surify.
type adminConfig struct {
	// MonitorAPIKey is an optional Silo API key with the admin:monitor:read
	// scope. Required to confirm that background notification recipients
	// remain enabled administrators.
	MonitorAPIKey string
	// ControlAPIKey is an optional Silo API key with the
	// admin:sessions:control scope, for the stop-stream page.
	ControlAPIKey string
	// DefaultAppToken is an optional shared Gotify application token for
	// profiles that don't bring their own.
	DefaultAppToken  string
	DefaultServerURL string
	// PublicURL overrides Silo's configured public URL for item links.
	PublicURL string
	// LegacyKeyStored reports that 0.2.0's silo_api_key is still saved.
	LegacyKeyStored bool
	// legacyAPIKey is that key's value, held in memory for the one-click
	// replacement only. Unexported and left out of LogValue so it can't be
	// logged or sent anywhere by accident.
	legacyAPIKey string
}

// storedValue is the config row as the plugin writes it back with the host's
// SetGlobalConfigEntry, which replaces the whole row. It carries every
// declared field and leaves out silo_api_key.
func (c adminConfig) storedValue() map[string]any {
	value := map[string]any{}
	for field, v := range map[string]string{
		fieldMonitorAPIKey:    c.MonitorAPIKey,
		fieldControlAPIKey:    c.ControlAPIKey,
		fieldDefaultAppToken:  c.DefaultAppToken,
		fieldDefaultServerURL: c.DefaultServerURL,
		fieldPublicURL:        c.PublicURL,
	} {
		if v != "" {
			value[field] = v
		}
	}
	return value
}

// withoutLegacyKey is c after silo_api_key has been dropped from the stored
// row.
func (c adminConfig) withoutLegacyKey() adminConfig {
	c.LegacyKeyStored, c.legacyAPIKey = false, ""
	return c
}

// LogValue keeps keys out of logs if a config is ever logged.
func (c adminConfig) LogValue() slog.Value {
	set := func(s string) bool { return s != "" }
	return slog.GroupValue(
		slog.Bool("monitor_api_key_set", set(c.MonitorAPIKey)),
		slog.Bool("control_api_key_set", set(c.ControlAPIKey)),
		slog.Bool("default_app_token_set", set(c.DefaultAppToken)),
		slog.String("public_url", c.PublicURL),
		slog.Bool("legacy_key_stored", c.LegacyKeyStored),
	)
}

// String and GoString keep keys out of fmt output (%v, %+v, %#v), the same
// way LogValue does for logs.
func (c adminConfig) String() string {
	return fmt.Sprintf("{monitor_api_key_set:%t control_api_key_set:%t default_app_token_set:%t public_url:%q legacy_key_stored:%t}",
		c.MonitorAPIKey != "", c.ControlAPIKey != "", c.DefaultAppToken != "", c.PublicURL, c.LegacyKeyStored)
}

func (c adminConfig) GoString() string { return "adminConfig" + c.String() }

func parseAdminConfig(entries []*pluginv1.ConfigEntry) adminConfig {
	var cfg adminConfig
	for _, entry := range entries {
		if entry.GetKey() != adminConfigKey {
			continue
		}
		fields := entry.GetValue().GetFields()
		str := func(key string) string { return strings.TrimSpace(fields[key].GetStringValue()) }
		cfg.MonitorAPIKey = str(fieldMonitorAPIKey)
		cfg.ControlAPIKey = str(fieldControlAPIKey)
		cfg.DefaultAppToken = str(fieldDefaultAppToken)
		cfg.DefaultServerURL = strings.TrimRight(str(fieldDefaultServerURL), "/")
		cfg.PublicURL = strings.TrimRight(str(fieldPublicURL), "/")
		cfg.legacyAPIKey = str(legacyAPIKeyField)
		cfg.LegacyKeyStored = cfg.legacyAPIKey != ""
	}
	return cfg
}
