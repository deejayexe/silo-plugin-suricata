// Generate Silo's repository document. The stable manifest.json URL holds a
// catalog envelope (plugins + binaries), not the executable's bare manifest.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	manifestutil "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/proto"
)

type binary struct {
	URL      string `json:"url"`
	Checksum string `json:"checksum"`
}
type entry struct {
	Manifest  *pb.PluginManifest `json:"manifest"`
	Repo      string             `json:"repo_url"`
	Checksums string             `json:"checksums_url"`
	Binaries  map[string]binary  `json:"binaries"`
}
type catalog struct {
	Plugins []entry `json:"plugins"`
}

func run(dir, version, release string) error {
	repo := "https://github.com/deejayexe/silo-plugin-suricata"
	raw, e := os.ReadFile("manifest.json")
	if e != nil {
		return e
	}
	m, e := manifestutil.Load(raw)
	if e != nil {
		return e
	}
	m.Version = version
	m.Checksum = ""
	if e = manifestutil.ValidateCatalogPresentation(m, repo); e != nil {
		return e
	}
	c := catalog{Plugins: []entry{{Manifest: m, Repo: repo, Checksums: release + "/checksums.txt", Binaries: map[string]binary{}}}}
	sums := []string{}
	for _, p := range m.SupportedPlatforms {
		platform := p.Os + "/" + p.Arch
		name := "plugin-" + p.Os + "-" + p.Arch
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil {
			return e
		}
		sum := sha256.Sum256(b)
		checksum := hex.EncodeToString(sum[:])
		c.Plugins[0].Binaries[platform] = binary{URL: release + "/" + name, Checksum: checksum}
		sums = append(sums, checksum+"  "+name)
	}
	body, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	var back catalog
	if e = json.Unmarshal(body, &back); e != nil {
		return e
	}
	if len(back.Plugins) != 1 || !proto.Equal(back.Plugins[0].Manifest, m) {
		return fmt.Errorf("Silo catalog roundtrip failed")
	}
	for _, name := range []string{"manifest.json", "catalog.json"} {
		if e = os.WriteFile(filepath.Join(dir, name), append(body, '\n'), 0644); e != nil {
			return e
		}
	}
	return os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Join(sums, "\n")+"\n"), 0644)
}
func main() {
	dir := flag.String("dir", "dist/release", "directory containing native binaries")
	version := flag.String("version", "0.0.1", "version")
	release := flag.String("release-url", "", "immutable release base URL")
	flag.Parse()
	if !strings.HasPrefix(*release, "https://github.com/deejayexe/silo-plugin-suricata/releases/download/") {
		panic("expected immutable Surimatrix release URL")
	}
	if e := run(*dir, *version, strings.TrimRight(*release, "/")); e != nil {
		panic(e)
	}
}
