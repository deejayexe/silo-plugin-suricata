<h1 align="center">Suricata · Silo Plugins</h1>

<p align="center">Plugins for your Silo server, available from one catalog.</p>

## Plugins

| | Plugin | What it does |
| :---: | --- | --- |
| <img src="plugins/surimatrix/logo.png" alt="Surimatrix" width="56"> | [Surimatrix](plugins/surimatrix/README.md) | Sends encrypted Matrix alerts with posters for new movies, additional movie files and groups of episodes. |
| <img src="plugins/surify/web/surify-logo.webp" alt="Surify" width="56"> | [Surify](plugins/surify/README.md) | Sends playback, new-content and server alerts through Gotify, with administrator controls. |
| <img src="plugins/duplicata/internal/review/web/logo.png" alt="Duplicata Versions" width="56"> | [Duplicata Versions](plugins/duplicata/README.md) | Reviews movie and episode versions and optionally deletes a duplicate while retaining a verified copy. |
| <img src="plugins/surichat/brand/surichat.png" alt="Surichat" width="56"> | [Surichat](plugins/surichat/README.md) | Private support chat for users and administrators, with optional email reminders. |

## Install

In Silo, open **Administration → Plugins → Repositories** and add:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

Refresh the catalog and install the plugins you want. This one URL serves all plugins and future updates. `repository.json` is an equivalent catalog alias. The individual `plugins/*/manifest.json` files describe plugins; they are not installation catalogs.

Open each plugin's setup guide above for its credentials and scheduled tasks. Linux amd64 and arm64 are supported by all plugins; Surify also supports macOS arm64. Surimatrix requires Ubuntu 24.04-compatible glibc and persistent writable storage.

## Project links

- [Release notes and manual downloads](https://github.com/deejayexe/silo-plugin-suricata/releases)
- [Plugin credentials](docs/CREDENTIALS.md)
- [Source and build documentation](docs/INTEGRATION.md)

Each plugin retains its license and dependency notices in its own folder.
