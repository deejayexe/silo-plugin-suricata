<p align="center">
  <img src="logo.png" alt="Surimatrix" width="240">
</p>

<h1 align="center">Surimatrix</h1>

<p align="center">Send new Silo movies and episodes to an encrypted Matrix room.</p>

## Install

Add the Suricata catalog in Silo under **Administration → Plugins → Repositories**, then select Surimatrix:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

## Setup

1. Save the Silo server URL and an unrestricted administrator API key. Leave the username blank for automatic account and primary-profile detection.
2. Use a dedicated Matrix bot and device. Enter the homeserver URL, bot user ID, access token, matching device ID and encrypted room ID. Invite the bot to the room and accept the invitation.
3. Set an absolute, persistent directory writable by Silo, such as `/config/surimatrix`. Keep its encryption keys, token and device across updates.
4. Leave **Notification title** empty to use **Settings → Identity → Server name** from your own Silo server. An optional custom title overrides it; the previous bundled default is automatically retired on upgrade.
5. Open Surimatrix from the administrator navigation. Check the Silo connection and send a poster test; confirm that Element decrypts the image.
6. Enable **Automatic notifications** and schedule **New content** every minute. The first completed run establishes a baseline without announcing existing items.

## Notifications

Movies arrive on the next run. Episodes are grouped by series, season and library, usually five minutes after the last addition, with a thirty-minute maximum. **Send latest content** sends a manual test without changing automatic delivery history.

Posters use public HTTPS image URLs; private image endpoints are unsupported. Back up the private storage directory with the plugin settings. Element may label the dedicated bot device as unverified.

## Requirements

Linux amd64 or arm64 with Ubuntu 24.04-compatible glibc · Administrator on the primary profile · Persistent writable storage. Alpine/musl requires its own build.

See the [detailed setup guide](docs/setup.md) for Matrix login, storage permissions and troubleshooting, and [dependency notices](THIRD_PARTY.md).

[MIT License](LICENSE)
