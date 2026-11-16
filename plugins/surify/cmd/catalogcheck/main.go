// Validate the combined catalog with the same protobuf representation Silo uses.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	manifestutil "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/proto"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: catalogcheck catalog.json")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var catalog struct {
		Plugins []struct {
			Manifest *pb.PluginManifest `json:"manifest"`
			Repo     string             `json:"repo_url"`
			Binaries map[string]struct {
				URL      string `json:"url"`
				Checksum string `json:"checksum"`
			} `json:"binaries"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		panic(err)
	}
	if len(catalog.Plugins) != 3 {
		panic("expected exactly three plugins")
	}
	seen := map[string]bool{}
	for _, entry := range catalog.Plugins {
		m := entry.Manifest
		if m == nil || seen[m.PluginId] {
			panic("missing or duplicate plugin identity")
		}
		seen[m.PluginId] = true
		if err := manifestutil.ValidateCatalogPresentation(m, entry.Repo); err != nil {
			panic(err)
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			panic(err)
		}
		loaded, err := manifestutil.Load(encoded)
		if err != nil || !proto.Equal(m, loaded) {
			panic(fmt.Sprintf("manifest roundtrip failed for %s: %v", m.PluginId, err))
		}
		for _, platform := range m.SupportedPlatforms {
			binary := entry.Binaries[platform.Os+"/"+platform.Arch]
			if binary.URL == "" || len(binary.Checksum) != 64 {
				panic("missing platform binary or SHA-256")
			}
		}
		fmt.Println("Validated", m.PluginId, m.Version)
	}
}
