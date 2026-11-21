# Surimatrix

Surimatrix sends new Silo library content directly to an encrypted Matrix room. It includes native poster attachments, episode grouping, persistent encryption keys and administrator-only delivery controls. No Gotify or separate notification container is required.

## Install and update

Add this stable URL under **Administration → Plugins → Repositories**:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

Refresh the repository and install Surimatrix. Silo selects Linux amd64 or arm64. Keep this URL for future updates. Stable releases use the plugin version from its manifest and are published after both architectures pass their checks.

The URL serves Silo's repository document with manifests, platform binaries and SHA-256 checksums. The source file `plugins/surimatrix/manifest.json` is not the update URL. Packages use CGO for SQLite and are built on Ubuntu 24.04; the Silo container needs compatible glibc. Alpine/musl or older environments need a build for their runtime.

## Connection settings

| Field | What to enter |
|---|---|
| Silo server URL | HTTPS base URL of your Silo server |
| Silo administrator API key | One API key owned by an administrator, without scope restrictions |
| Silo username | Optional normal username of the API key owner; leave blank for automatic detection |
| Matrix homeserver | HTTPS base URL of the Matrix server, not the Element website |
| Matrix bot account | Full user ID, such as `@silo-bot:example.com` |
| Matrix access token | Access token for a dedicated Surimatrix device |
| Matrix device ID | Device ID associated with that access token |
| Encrypted room ID | Internal room ID, such as `!room:example.com` |
| Persistent storage directory | Absolute writable path inside the Silo container, such as `/config/surimatrix` |
| Notification title | Leave empty to use Settings → Identity → Server name; optional custom override |

The Silo API key is used for library additions, account detection and item details with signed poster URLs. The primary profile is selected automatically. You do not need to enter a profile ID or a separate poster API key. If a Silo username is supplied, it must match the key's owner.

On Silo build `4336abfd`, `admin:monitor:read` can list additions but cannot read item details or the account/profile endpoints. There is no catalog-read scope to add in that version, so the shared key must be an administrator key without scope restrictions. Surimatrix only makes GET requests to Silo, although this credential has broader permissions. Keep it in the private plugin settings.

Silo and Matrix require separate credentials: one Silo API key and one Matrix bot access token. Image downloads never receive either credential.

## Matrix account and storage

Use a dedicated bot account and a fresh device exclusively for Surimatrix. Invite the bot to an encrypted room and accept the invitation. Do not reuse an Element device's identity. The optional `../scripts/matrix-login.py` utility can create a new password-login session from a computer with Python; the plugin does not depend on it.

Keep the same device, token and storage directory across updates. If Silo already has a persistent `/config` volume, `/config/surimatrix` does not require another mount. The directory must be writable by the UID/GID of the Silo process, which may differ from the root user used by `docker exec`.

Back up the entire private directory together with Silo's plugin configuration. Stop Silo for a consistent SQLite backup. Never publish the keys, database or notification state. A different Matrix account, device or room requires another storage directory.

## Administrator panel

**Connection checks** verifies the Silo account and primary profile, sends a sample library poster, or sends the Surimatrix logo as an encrypted image. The connection check displays names, never tokens or profile IDs.

**Send latest content** lets you select a library by name and send its latest movie or episode. The default covers all configured libraries. Each click deliberately sends another message, making it useful for delivery checks or manual recovery. It does not change automatic notification history, pending groups or the first-run baseline.

Latest content is determined by `added_at`, inspecting all pages in the newest nonempty date window. Silo's administrator catalog is ordered by content ID, so its first row is not assumed to be the newest. Date windows expand from one day through one year, then the full catalog. A request has a 90-second budget and a 200-page limit per window; select one library if the result set is too large. When a library is selected, Silo's library-specific addition time is used.

A manual notification without available artwork is sent as encrypted text. A failed poster lookup or download reports an error. The sample-poster check requires an image and reports when its sample has none.

## Automatic notifications

1. Save the connection settings with automatic delivery disabled.
2. Open Surimatrix and check the Silo connection.
3. Send a real poster and confirm it appears in Element. Restart Silo and repeat to check persistent keys.
4. Enable automatic notifications and schedule **New content** every minute.
5. Let the first run finish, then add content. Existing catalog items are not announced on enrollment.

New movies are sent on the next run. When another file appears for a movie already checked, Surimatrix sends the usual movie notification with the metadata available for that file. Each file has its own Silo ID, including files with identical technical details. On the first check of each movie, Surimatrix records the files already present. Large libraries can take several scheduled runs to check in full.

Episodes with a stable series ID are grouped by series, season and library. The task sends them five minutes after the last addition, capped at thirty minutes. Groups wait for their catalog pagination window to finish. Episodes without a stable series ID are sent individually.

Notifications use your Silo server name by default, or an explicit custom title, followed by media type, library, title/year and available metadata. For example:

```text
Silo
🎬 🆕 New movie in Movies
Title: Example (2026)
Genre: Comedy
Rating: TMDB 6.2 / 10
--------------------
Resolution: 1080p · hevc · dts
```

Empty metadata lines are omitted. There is no Silo URL footer. A synopsis may be included when available. Complete-season claims are not made without verification.

Automatic delivery covers all libraries by default. The advanced `libraries` setting can restrict library IDs; manual delivery provides a name-based selector. Changing this filter does not create a new first-run enrollment.

## Encryption and delivery

Posters are obtained from Silo's item-detail API. Relative image URLs are resolved against the Silo URL. Downloads accept public HTTPS JPEG, PNG, GIF or WebP images up to 10 MiB, with DNS and redirect checks. Private image endpoints or cookies are not supported.

Images are encrypted before upload. Notifications use native `m.image` attachments with captions and a separate filename, explicitly sent inside `m.room.encrypted`. Without an image, `m.text` is also encrypted. `mxc://` is Matrix's normal internal media reference.

Keys, catalog cursors, pending groups and transaction IDs are persisted. Failed automatic poster lookups/downloads remain pending for retry. Already delivered notifications are not repeated automatically after an update. Matrix retry deduplication depends on retaining the token.

Interactive SAS/QR verification, cross-signing and Matrix backup recovery are not included. Element may flag an unverified bot device even when it can decrypt its images. Sync runs during tasks and tests rather than continuously.

## Troubleshooting and validation

Use the panel's connection and image checks first. Task failures are logged as `Surimatrix: content task failed`. A permission-denied error in the storage directory requires correcting its owner to the Silo process UID/GID. Authentication failures need the correct credential for the service being queried.

The owner has confirmed encrypted poster delivery on a QNAP TVS-h1688X, Linux amd64, Silo `2488 · 4336abfd`. Automated tests cover one-key account/profile discovery, administrator-only routes, real Olm/Megolm encryption and SQLite reopening, attachment integrity, persistent groups, latest-item pagination and the update catalog. Verify new builds on your server before relying on automatic delivery.

Relevant Silo implementation: [scope allowlist](https://github.com/Silo-Server/silo-server/blob/4336abfdd6f5eb4f48ca3731131a6ca717142b70/internal/api/middleware/api_key_scopes.go) and [RuntimeHost](https://github.com/Silo-Server/silo-server/blob/4336abfdd6f5eb4f48ca3731131a6ca717142b70/internal/pluginhost/runtime_host_server.go). The image RPCs declared in the SDK are not implemented by that host build; Surimatrix uses the item-detail API.

## Build

On Linux with Go 1.26+, GCC and Python 3:

```sh
cd plugins/surimatrix
go test -tags goolm ./...
go vet -tags goolm ./...
./scripts/package.sh
```

GitHub Actions tests, builds and publishes amd64/arm64 packages and the stable update document. Source dependencies are pinned in `go.mod` and `go.sum`; see `THIRD_PARTY.md` for licenses.

## Automatic server identity

With the notification title left empty, Surimatrix reads `server_name` from Silo's public `GET /api/v2/theme/branding` endpoint without sending an API key. It reflects server-name changes on the next notification. The plugin host's identity is a compatibility fallback; if identity cannot be read, the neutral title `Silo` keeps delivery working. A previously bundled default is recognized on upgrade and treated as automatic. No personal server name is stored in the release source or defaults.
