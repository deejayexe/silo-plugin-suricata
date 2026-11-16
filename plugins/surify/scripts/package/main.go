// Command package builds the plugin for each supported platform and writes
// the files Admin > Plugins > "Install from a file" accepts into dist/:
//
//	dist/surify_<version>_<os>_<arch>.zip    manifest.json + plugin
//	dist/binaries/surify_<version>_<os>_<arch>   the bare binary
//	dist/SHA256SUMS.txt
//
// With -release-url (the GitHub release's download folder) it also writes the
// assets for a catalog install into dist/release/:
//
//	plugin-<os>-<arch>   the bare binary, as Silo's own catalog names them
//	checksums.txt        their SHA-256 sums
//	catalog.json         a one-plugin Silo catalog listing those binaries
//
// Run from the repository root: go run ./scripts/package
//
// A zip must hold exactly "manifest.json" and "plugin" at its root, and the
// manifest's "checksum" must be the SHA-256 of "plugin", so there is one zip
// per platform. A bare binary needs no checksum: Silo runs it with the
// "manifest" argument and the plugin reports its own. The zip is the safer
// choice because Silo never has to execute it to install it.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/proto"
)

func main() {
	out := flag.String("out", "dist", "output folder")
	targets := flag.String("targets", "linux/amd64,linux/arm64,darwin/arm64", "comma-separated os/arch list")
	manifestPath := flag.String("manifest", "manifest.json", "manifest to package")
	pkg := flag.String("pkg", ".", "Go package to build")
	verify := flag.String("verify", "", "only check this zip (os_arch taken from its name) and exit")
	releaseURL := flag.String("release-url", "", "release download folder, e.g. https://github.com/OWNER/REPO/releases/download/v1.2.3; writes dist/release/")
	repoURL := flag.String("repo-url", "https://github.com/deejayexe/silo-plugin-suricata", "source repository listed in the catalog")
	version := flag.String("version", "", "build as this version instead of manifest.json's; must be manifest.json's version plus a pre-release, e.g. 0.4.0-dev.12")
	mergeCatalog := flag.String("merge-catalog", "", "catalog.json whose entries for this plugin are copied into ours (other versions only); a missing file is skipped")
	flag.Parse()
	if *verify != "" {
		base := strings.TrimSuffix(filepath.Base(*verify), ".zip")
		parts := strings.Split(base, "_")
		if len(parts) < 4 {
			fmt.Fprintln(os.Stderr, "package: name must be <name>_<version>_<os>_<arch>.zip")
			os.Exit(1)
		}
		n := len(parts)
		if err := verifyZip(*verify, "silo.gotify", parts[n-3], parts[n-2], parts[n-1]); err != nil {
			fmt.Fprintln(os.Stderr, "package:", err)
			os.Exit(1)
		}
		fmt.Println("ok", *verify)
		return
	}
	if err := run(*out, *targets, *manifestPath, *pkg, *releaseURL, *repoURL, *version, *mergeCatalog); err != nil {
		fmt.Fprintln(os.Stderr, "package:", err)
		os.Exit(1)
	}
}

func run(out, targets, manifestPath, pkg, releaseURL, repoURL, version, mergeCatalog string) error {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var head struct {
		PluginID string `json:"plugin_id"`
		Version  string `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Version == "" {
		return fmt.Errorf("read version from %s: %v", manifestPath, err)
	}
	ldflags := "-s -w"
	if version != "" {
		if !devVersion.MatchString(version) || !strings.HasPrefix(version, head.Version+"-") {
			return fmt.Errorf("version %q must be %s-<pre-release>, e.g. %s-dev.1", version, head.Version, head.Version)
		}
		if raw, err = setManifestField(raw, "version", version); err != nil {
			return err
		}
		head.Version = version
		ldflags += " -X main.version=" + version
	}
	binDir := filepath.Join(out, "binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	var sums []string
	release := map[string][]byte{} // "os/arch" -> binary
	for _, t := range strings.Split(targets, ",") {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(t), "/")
		if !ok {
			return fmt.Errorf("bad target %q", t)
		}
		name := fmt.Sprintf("surify_%s_%s_%s", head.Version, goos, goarch)
		binPath := filepath.Join(binDir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", binPath, pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", goos, goarch, err)
		}
		bin, err := os.ReadFile(binPath)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(bin)
		checksum := hex.EncodeToString(sum[:])
		zipPath := filepath.Join(out, name+".zip")
		if err := writeZip(zipPath, raw, bin, checksum); err != nil {
			return err
		}
		if err := verifyZip(zipPath, head.PluginID, head.Version, goos, goarch); err != nil {
			return fmt.Errorf("verify %s: %w", zipPath, err)
		}
		zb, _ := os.ReadFile(zipPath)
		zsum := sha256.Sum256(zb)
		sums = append(sums, fmt.Sprintf("%s  %s", hex.EncodeToString(zsum[:]), filepath.Base(zipPath)),
			fmt.Sprintf("%s  binaries/%s", checksum, name))
		fmt.Printf("%s  (%d bytes, plugin sha256 %s)\n", zipPath, len(zb), checksum)
		release[goos+"/"+goarch] = bin
	}
	sort.Strings(sums)
	if err := os.WriteFile(filepath.Join(out, "SHA256SUMS.txt"), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if releaseURL == "" {
		return nil
	}
	var extra []catalogPackage
	if mergeCatalog != "" {
		if extra, err = readCatalog(mergeCatalog, head.PluginID, head.Version); err != nil {
			return err
		}
	}
	return writeRelease(filepath.Join(out, "release"), strings.TrimRight(releaseURL, "/"), repoURL, raw, release, extra)
}

// devVersion is a semver version with a pre-release, such as 0.4.0-dev.12.
var devVersion = regexp.MustCompile(`^\d+\.\d+\.\d+-[0-9A-Za-z.-]+$`)

// setManifestField replaces one top-level field of the manifest JSON.
func setManifestField(manifest []byte, key string, value any) ([]byte, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	v, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m[key] = v
	return json.MarshalIndent(m, "", "  ")
}

// readCatalog returns the entries of another catalog (the stable one, for the
// dev catalog) for this plugin, leaving out the version being built. A missing
// file means there is no such catalog yet.
func readCatalog(path, pluginID, skipVersion string) ([]catalogPackage, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Printf("%s not found; nothing to merge\n", path)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c struct {
		Plugins []catalogPackage `json:"plugins"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []catalogPackage
	for _, p := range c.Plugins {
		if p.Manifest.GetPluginId() == pluginID && p.Manifest.GetVersion() != skipVersion {
			out = append(out, p)
			fmt.Printf("merged %s %s from %s\n", pluginID, p.Manifest.GetVersion(), path)
		}
	}
	return out, nil
}

// catalogPackage is one entry of a Silo plugin catalog, with the field names
// Silo reads (internal/plugins/catalog_service.go).
type catalogPackage struct {
	Manifest     *pluginv1.PluginManifest `json:"manifest"`
	RepoURL      string                   `json:"repo_url,omitempty"`
	ChecksumsURL string                   `json:"checksums_url,omitempty"`
	Binaries     map[string]catalogBinary `json:"binaries"`
}

type catalogBinary struct {
	URL      string `json:"url"`
	Checksum string `json:"checksum"`
}

// writeRelease writes the bare binaries, checksums.txt and catalog.json for a
// GitHub release. Silo decodes a catalog with encoding/json, not protojson, so
// the manifest is encoded the same way (enums as numbers, as Silo's own
// catalog has them). The catalog is decoded again and compared with the
// manifest to prove Silo will read back what manifest.json says.
func writeRelease(dir, releaseURL, repoURL string, manifestJSON []byte, bins map[string][]byte, extra []catalogPackage) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if err := publicmanifest.ValidateCatalogPresentation(m, repoURL); err != nil {
		return fmt.Errorf("manifest presentation: %w", err)
	}
	m.Checksum = "" // each binary reports its own
	platforms := map[string]bool{}
	for _, p := range m.GetSupportedPlatforms() {
		platforms[p.GetOs()+"/"+p.GetArch()] = true
	}
	pkg := catalogPackage{
		Manifest:     m,
		RepoURL:      repoURL,
		ChecksumsURL: releaseURL + "/checksums.txt",
		Binaries:     map[string]catalogBinary{},
	}
	var sums []string
	for target, bin := range bins {
		if !platforms[target] {
			return fmt.Errorf("%s is built but not in supported_platforms", target)
		}
		name := "plugin-" + strings.ReplaceAll(target, "/", "-")
		if err := os.WriteFile(filepath.Join(dir, name), bin, 0o755); err != nil {
			return err
		}
		sum := sha256.Sum256(bin)
		checksum := hex.EncodeToString(sum[:])
		sums = append(sums, checksum+"  "+name)
		pkg.Binaries[target] = catalogBinary{URL: releaseURL + "/" + name, Checksum: checksum}
	}
	for target := range platforms {
		if _, ok := pkg.Binaries[target]; !ok {
			return fmt.Errorf("%s is in supported_platforms but wasn't built", target)
		}
	}
	sort.Strings(sums)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	catalog := struct {
		Plugins []catalogPackage `json:"plugins"`
	}{Plugins: append([]catalogPackage{pkg}, extra...)}
	body, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	var back struct {
		Plugins []catalogPackage `json:"plugins"`
	}
	if err := json.Unmarshal(body, &back); err != nil {
		return fmt.Errorf("catalog doesn't decode: %w", err)
	}
	if len(back.Plugins) != 1+len(extra) || !proto.Equal(back.Plugins[0].Manifest, m) {
		return fmt.Errorf("catalog manifest doesn't decode back to manifest.json")
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), append(body, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s  (%s %s, %d platforms)\n", filepath.Join(dir, "catalog.json"), m.GetPluginId(), m.GetVersion(), len(pkg.Binaries))
	return nil
}

// writeZip stores the manifest with its checksum filled in, then the binary.
func writeZip(path string, manifest, bin []byte, checksum string) error {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return err
	}
	cs, _ := json.Marshal(checksum)
	m["checksum"] = cs
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, e := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{{"manifest.json", 0o644, body}, {"plugin", 0o755, bin}} {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}

// verifyZip repeats the checks Silo's installer makes: both files at the
// root, a manifest the SDK accepts, the checksum matching the binary, and the
// platform listed.
func verifyZip(path, pluginID, version, goos, goarch string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		files[f.Name] = b
	}
	mb, pb := files["manifest.json"], files["plugin"]
	if mb == nil || pb == nil || len(files) != 2 {
		return fmt.Errorf("archive must hold exactly manifest.json and plugin")
	}
	m, err := publicmanifest.Load(mb)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pb)
	if m.GetChecksum() != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("checksum does not match the binary")
	}
	if m.GetPluginId() != pluginID || m.GetVersion() != version || m.GetSiloApiVersion() != "v1" {
		return fmt.Errorf("unexpected identity %s %s api %s", m.GetPluginId(), m.GetVersion(), m.GetSiloApiVersion())
	}
	for _, p := range m.GetSupportedPlatforms() {
		if p.GetOs() == goos && p.GetArch() == goarch {
			return nil
		}
	}
	return fmt.Errorf("%s/%s is not in supported_platforms", goos, goarch)
}
