# Suricata integration · 0.0.1

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

`Suricata testing` tests all three plugins with the race detector and `go vet` on native Linux amd64 and arm64 runners. Surimatrix uses CGO, SQLite and `goolm` on Ubuntu 24.04. Surify also cross-compiles a static macOS arm64 package. Duplicata runs its existing packaging and JavaScript checks.

Every build runs the native binary's `manifest` command and verifies identity, version and presentation. Combined catalog tests cover all plugin IDs, platform maps, real binary hashes, executable zip contents, missing binaries and invalid release metadata. The Silo SDK validates each catalog manifest after serialization.

After all checks pass, the workflow publishes an immutable prerelease with binaries, per-platform installation zips, checksums and catalog files. It updates only the `0.0.1` branch with identical `manifest.json` and `repository.json` catalogs. A source-commit check prevents publishing a stale catalog over newer source. It never pushes to `main` and does not mark testing releases as latest.

Testing versions use `0.0.1-dev.RUN_ID.ATTEMPT` for Surify/Surimatrix. Duplicata uses `0.1.1-dev.RUN_ID.ATTEMPT` so that an existing stable 0.1.0 can receive the candidate update. Branch and suite name `0.0.1` are independent of the individual plugin versions. Avoid keeping the original and unified catalogs enabled together for the same plugin ID.

To run checks locally, use Go 1.27.4 and GCC for Surimatrix. In each plugin folder run `go test -race ./...` and `go vet ./...`, adding `-tags goolm` for Surimatrix. From the root run `python3 -m unittest discover -s tests -v`. The workflow contains the full build commands.

## Validation on Silo

Automated validation does not establish real-server behavior. Before promoting to `main`, use the shared catalog URL from the README and check:

1. Silo lists all three plugins, chooses the correct platform and installs/updates each without losing settings.
2. Surimatrix checks the Silo account, sends an encrypted poster visible in Element, and still decrypts after a Silo restart using the same persistent storage. Schedule New content and test a new movie and grouped episodes.
3. Surify saves a Gotify administrator connection and sends a test. Check scoped-key validation, playback/server alerts, new-library-content delivery and task bindings. Ordinary users and secondary profiles must remain blocked.
4. Duplicata connects and scans a library, displays versions and restores saved history. Keep deletion disabled for the first scan. If you test deletion, use disposable media inside an authorized folder and confirm that another verified copy remains.
5. Record the Silo build, architecture and plugin versions used. Merge and production release remain pending real-server validation.
