<h1 align="center">Suricata · Silo Plugins</h1>

<p align="center">Three plugins for your Silo server, available from one catalog.</p>

## Plugins

| | Plugin | What it does |
| :---: | --- | --- |
| <img src="plugins/surimatrix/logo.png" alt="Surimatrix" width="56"> | [Surimatrix](plugins/surimatrix/README.md) | Sends new movies and grouped episodes to an encrypted Matrix room, including posters. |
| <img src="plugins/surify/web/surify-logo.webp" alt="Surify" width="56"> | [Surify](plugins/surify/README.md) | Sends playback, new-content and server alerts through Gotify, with administrator controls. |
| <img src="plugins/duplicata/internal/review/web/logo.png" alt="Duplicata Versions" width="56"> | [Duplicata Versions](plugins/duplicata/README.md) | Reviews movie and episode versions and optionally deletes a duplicate while retaining a verified copy. |

## Install for testing

In Silo, open **Administration → Plugins → Repositories** and add:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/0.0.1/manifest.json
```

Refresh the catalog and install the plugins you want. This one URL serves all three plugins and future testing updates. `repository.json` is an equivalent catalog alias. The individual `plugins/*/manifest.json` files describe plugins; they are not installation catalogs.

Open each plugin's setup guide above for its credentials and scheduled tasks. Linux amd64 and arm64 are supported by all three; Surify also supports macOS arm64. Surimatrix requires Ubuntu 24.04-compatible glibc and persistent writable storage.

## Testing branch

All integration work lives on **0.0.1**. Builds publish prerelease packages only after automated checks pass. The catalog contains real binary URLs and SHA-256 checksums. Existing plugin identifiers are retained to preserve configuration; Surify keeps `silo.gotify`.

Validate installation, configuration and delivery on your Silo server before promoting this branch to `main`. See [source revisions and validation](docs/INTEGRATION.md).

Each plugin retains its license and dependency notices in its own folder.
