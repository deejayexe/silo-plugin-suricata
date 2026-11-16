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
3. Configure the scoped Silo read-only key with **admin:monitor:read**. Background delivery uses it to verify that recipients remain enabled administrators.
4. Choose the playback and server alerts you want. Bind **health_check** every five minutes and **weekly_digest** every hour.
5. For additions, enable **New library content** and bind **recently_added** every two minutes. Restart Silo after changing task bindings. Existing library items are not announced.

## Notifications

Surify supports grouped episodes, quiet hours, templates, playback failures, stream watchdogs, health and recovery alerts, and weekly digests. Posters use public image URLs exposed by Silo. Open Silo's native **Admin → Activity** page for live sessions.

Gotify application tokens send notifications; use the Gotify client to choose sounds and devices. Surify does not upload binary posters or repeat alerts. Optional stream-control actions require a separate **admin:sessions:control** key. Saved keys are masked; leave a secret field blank when editing to retain it.

## Compatibility

The visible name is **Surify**. Its existing identifier `silo.gotify` is retained so Silo can preserve settings and update an existing installation. Access and delivery are restricted to administrators on their primary profiles.

Linux amd64, Linux arm64 and macOS arm64 · [API guide](API.md) · [MIT License](LICENSE) · [Upstream attribution](NOTICE)
