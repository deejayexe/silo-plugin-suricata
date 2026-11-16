package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogNativeDecoding(t *testing.T) {
	raw, e := os.ReadFile("../../manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	if e = os.WriteFile(filepath.Join(root, "manifest.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	for _, a := range []string{"amd64", "arm64"} {
		if e = os.WriteFile(filepath.Join(root, "plugin-linux-"+a), []byte(a), 0600); e != nil {
			t.Fatal(e)
		}
	}
	t.Chdir(root)
	if e = run(root, "0.0.1-dev.2", "https://github.com/deejayexe/silo-plugin-suricata/releases/download/testing-0.0.1-2"); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(root, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	var c catalog
	if e = json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	if len(c.Plugins) != 1 || len(c.Plugins[0].Binaries) != 2 || c.Plugins[0].Manifest.Version != "0.0.1-dev.2" {
		t.Fatal("invalid update catalog")
	}
}
