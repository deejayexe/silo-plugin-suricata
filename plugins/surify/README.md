<p align="center">
  <img src="web/surify-logo.webp" alt="Surify" width="240">
</p>

<h1 align="center">Surify</h1>

<p align="center">Receive Silo playback, library and server notifications through Gotify.</p>

## Install

Add the Suricata catalog in Silo under **Administration → Plugins → Repositories**, then select Surify:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

## Setup

1. Sign in as an administrator on the primary profile and open **Surify** from the administrator navigation.
2. Create an application on your Gotify server. In **Connection**, save its server URL and private application token, then send a test notification. After upgrading an older connection, save it again to activate administrator delivery.
3. Create one unrestricted Silo administrator API key under **Admin → API Keys**. Save it in Surify’s **Silo API key** tab or plugin settings. The same key handles monitoring, recipient checks and stream-control actions; no second Silo key is needed.
4. Choose the playback and server alerts you want. Bind **health_check** every five minutes and **weekly_digest** every hour.
5. For additions, enable **New library content** and bind **recently_added** every two minutes. Restart Silo after changing task bindings. Existing library items are not announced.

## Notifications

Surify supports grouped episodes, quiet hours, templates, playback failures, stream watchdogs, health and recovery alerts, and weekly digests. Posters use public image URLs exposed by Silo. Open Silo's native **Admin → Activity** page for live sessions.

Gotify application tokens send notifications; use the Gotify client to choose sounds and devices. Surify does not upload binary posters or repeat alerts. The single Silo key remains hidden; leave its field blank when editing to retain it. Surify never creates or revokes API keys automatically. Existing scoped settings keep working until you save the replacement administrator key.

## Compatibility

The visible name is **Surify**. Its existing identifier `silo.gotify` is retained so Silo can preserve settings and update an existing installation. Access and delivery are restricted to administrators on their primary profiles.

Linux amd64, Linux arm64 and macOS arm64 · [API guide](API.md) · [MIT License](LICENSE) · [Upstream attribution](NOTICE)
