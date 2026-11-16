# Suricata · Release 0.0.1

## Source revisions

| Plugin | Source branch | Commit |
| --- | --- | --- |
| surimatrix | `testing` | `bfdfd4eaab5c1f2dc0009225a38c57c3124b809c` |
| surify | `testing/0.0.1` | `b7824276512774ffe21c5bff9d977ce99a6d4bee` |
| duplicata | `main` | `4f803391707ee102b86b5b2ea7d3be57314f52fd` |

Duplicata's `main` includes the latest history scrolling correction and stable 0.1.0; its older `duplicata-0.0.1` branch was compared before selecting `main`. The `main` branches of Surimatrix and Gotify contain only their initial README files; their testing branches contain the implementations.

## Layout and compatibility

Each plugin keeps its own Go module and pinned dependencies under `plugins/`. Original module names and plugin IDs are retained to preserve internal imports and existing Silo configuration. Surify is the visible Gotify plugin name; `silo.gotify` remains its technical identity.

Readmes use Duplicata's centered logo/title, short installation instructions and practical setup sections. The general README uses 56-pixel logos. Surimatrix's longer operational guide is retained under `plugins/surimatrix/docs/`.

Catalog presentation and source links point to this repository. Original licenses, attribution and runtime assets are preserved. Notification and deletion behavior is inherited from the source revisions above.

## Build and publication

The workflow tests all three plugins with the race detector and `go vet` on native Linux amd64 and arm64. Surimatrix uses CGO, SQLite and `goolm` on Ubuntu 24.04. Surify also builds macOS arm64. It validates each native runtime manifest and the combined catalog with the Silo SDK, then publishes real binaries, per-platform zips and SHA-256 checksums.

On `main`, the root `VERSION` controls the Suricata release tag, starting at `v0.0.1`. Each plugin keeps its own version in its source manifest; Duplicata retains its higher 0.1.1 version for update compatibility. Source and public catalog manifests carry matching versions. The generated `manifest.json` and `repository.json` catalogs are published back to the source branch only after all checks pass.

For a future release, increment the root `VERSION` and the manifest versions of the plugins changed. Update Duplicata's own `VERSION` and default main version alongside its manifest. Unchanged plugins retain their versions. Release assets are immutable; existing release tags are not overwritten. A source-head check prevents publishing over newer source.

The existing `0.0.1` testing branch and its catalog remain separate. Production installations should use only the `main` catalog for these plugin IDs.

## Release fixes

All bundled plugin text is in English, including sample data used by tests. English Unicode fixtures retain coverage for non-ASCII notification content.

Surimatrix reads the server identity using Silo's public branding API. An explicit title remains an optional override. A fingerprint recognizes the retired bundled default on upgrade without retaining a personal name in source. Server renames are picked up on the next send. Tests cover identity lookup, renaming, custom overrides, unavailable branding and credential-free public requests.

Surify's control key was present in JSON schema without secret metadata. It is now declared as a password field and a write-only secret, alongside the monitoring key and Gotify application token. This lets Silo omit saved values from the configuration response and preserve them on blank edits. Regression checks cover secret declarations and ensure the status API exposes only configuration flags.

## Local verification

Use Go 1.27.4 and GCC for Surimatrix. In each plugin folder run `go test -race ./...` and `go vet ./...`, adding `-tags goolm` for Surimatrix. From the root run `python3 -m unittest discover -s tests -v`. The workflow contains the full build commands.
