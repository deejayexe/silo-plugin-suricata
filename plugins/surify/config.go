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
// saved value after an upgrade. It can be explicitly adopted from the panel
// after validation; it is not automatically activated or revoked.
const legacyAPIKeyField = "silo_api_key"

// Config fields as stored (manifest.json global_config_schema "gotify").
const (
	fieldAdminAPIKey      = "admin_api_key"
	fieldMonitorAPIKey    = "monitor_api_key"
	fieldControlAPIKey    = "control_api_key"
	fieldDefaultAppToken  = "default_app_token"
	fieldDefaultServerURL = "default_server_url"
	fieldPublicURL        = "public_url"
)

// adminConfig is the server-wide configuration an admin sets under
// Admin > Plugins > Surify.
type adminConfig struct {
	// AdminAPIKey is the one administrator credential used by all Silo calls.
	AdminAPIKey string
	// Old scoped keys remain readable for compatibility until an administrator
	// saves the single replacement key. They are not exposed by the new form.
	MonitorAPIKey string
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
// SetGlobalConfigEntry, which replaces the whole row. It carries the
// current fields. Saving the single administrator key removes old stored keys
// without revoking any credentials on the Silo server.
func (c adminConfig) storedValue() map[string]any {
	value := map[string]any{}
	for field, v := range map[string]string{
		fieldAdminAPIKey:      c.AdminAPIKey,
		fieldDefaultAppToken:  c.DefaultAppToken,
		fieldDefaultServerURL: c.DefaultServerURL,
		fieldPublicURL:        c.PublicURL,
	} {
		if v != "" {
			value[field] = v
		}
	}
	if c.AdminAPIKey == "" {
		for field, secret := range map[string]string{fieldMonitorAPIKey: c.MonitorAPIKey, fieldControlAPIKey: c.ControlAPIKey, legacyAPIKeyField: c.legacyAPIKey} {
			if secret != "" {
				value[field] = secret
			}
		}
	}
	return value
}

func (c adminConfig) monitorKey() string {
	if c.AdminAPIKey != "" {
		return c.AdminAPIKey
	}
	return c.MonitorAPIKey
}

func (c adminConfig) controlKey() string {
	if c.AdminAPIKey != "" {
		return c.AdminAPIKey
	}
	return c.ControlAPIKey
}

// LogValue keeps keys out of logs if a config is ever logged.
func (c adminConfig) LogValue() slog.Value {
	set := func(s string) bool { return s != "" }
	return slog.GroupValue(
		slog.Bool("admin_api_key_set", set(c.AdminAPIKey)),
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
	return fmt.Sprintf("{admin_api_key_set:%t monitor_api_key_set:%t control_api_key_set:%t default_app_token_set:%t public_url:%q legacy_key_stored:%t}",
		c.AdminAPIKey != "", c.MonitorAPIKey != "", c.ControlAPIKey != "", c.DefaultAppToken != "", c.PublicURL, c.LegacyKeyStored)
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
		cfg.AdminAPIKey = str(fieldAdminAPIKey)
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
