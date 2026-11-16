<h1 align="center">Suricata · Silo Plugins</h1>

<p align="center">Three plugins for your Silo server, available from one catalog.</p>

## Plugins

| | Plugin | What it does |
| :---: | --- | --- |
| <img src="plugins/surimatrix/logo.png" alt="Surimatrix" width="56"> | [Surimatrix](plugins/surimatrix/README.md) | Sends new movies and grouped episodes to an encrypted Matrix room, including posters. |
| <img src="plugins/surify/web/surify-logo.webp" alt="Surify" width="56"> | [Surify](plugins/surify/README.md) | Sends playback, new-content and server alerts through Gotify, with administrator controls. |
| <img src="plugins/duplicata/internal/review/web/logo.png" alt="Duplicata Versions" width="56"> | [Duplicata Versions](plugins/duplicata/README.md) | Reviews movie and episode versions and optionally deletes a duplicate while retaining a verified copy. |

## Install

In Silo, open **Administration → Plugins → Repositories** and add:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

Refresh the catalog and install the plugins you want. This one URL serves all three plugins and future updates. `repository.json` is an equivalent catalog alias. The individual `plugins/*/manifest.json` files describe plugins; they are not installation catalogs.

Open each plugin's setup guide above for its credentials and scheduled tasks. Linux amd64 and arm64 are supported by all three; Surify also supports macOS arm64. Surimatrix requires Ubuntu 24.04-compatible glibc and persistent writable storage.

## Release 0.0.2

The production catalog is published from **main** after automated checks pass. Surify is version **0.0.2**, Surimatrix remains **0.0.1**; Duplicata Versions is **0.1.1** so existing installations can update from its earlier stable and testing versions. Plugin identities are retained, including Surify's `silo.gotify`.

Surimatrix reads the notification title from your Silo server's identity settings by default. No personal server name is bundled. Surify uses one hidden administrator API key for monitoring and stream control, plus its separate Gotify application token.

Replace the previous testing repository URL with the production URL above. Keep only one catalog enabled for these plugin IDs. See [credentials](docs/CREDENTIALS.md) and [source revisions and releases](docs/INTEGRATION.md).

Each plugin retains its license and dependency notices in its own folder.
