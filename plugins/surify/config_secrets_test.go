package main

import (
	"encoding/json"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

func TestSavedCredentialsAreDeclaredAsSecrets(t *testing.T) {
	m, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	schema := m.GetGlobalConfigSchema()[0]
	var document struct {
		Properties map[string]struct {
			WriteOnly bool `json:"writeOnly"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema.JsonSchema), &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{fieldAdminAPIKey, fieldDefaultAppToken} {
		found := false
		for _, field := range schema.AdminForm.Fields {
			if field.Key == key {
				found = true
				if !field.Secret || field.Control != pluginv1.AdminFormControl_ADMIN_FORM_CONTROL_PASSWORD {
					t.Errorf("%s would be exposed in Silo's configuration form", key)
				}
			}
		}
		if !found || !document.Properties[key].WriteOnly {
			t.Errorf("%s must be hidden in both form metadata and JSON schema", key)
		}
	}
}

func TestKeyStatusNeverReturnsSavedCredentials(t *testing.T) {
	p := testPlugin(t)
	p.setConfig(adminConfig{AdminAPIKey: "private-admin-key"})
	raw, err := json.Marshal(p.adminKeyStatus())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-") {
		t.Fatal("key status exposed a saved credential")
	}
	if !p.adminKeyStatus().ControlKeySet || !p.adminKeyStatus().MonitorKeySet {
		t.Fatal("key status must report only whether credentials are configured")
	}
}
