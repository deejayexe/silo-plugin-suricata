# Surify API — 0.0.3

Every route, including the settings page, state, documentation and logo, requires
an administrator on their primary Silo profile. The manifest uses admin access and
admin navigation. The runtime independently checks trusted Silo proxy headers.
Only the active administrator connection can be read, changed, deleted or tested;
household profiles and other administrators' settings are not exposed.

Saving an administrator connection stamps `admin_managed` in encrypted Silo state.
Pre-upgrade connections remain inactive until saved once by their administrator.
No saved token is deleted during this migration. Inbox notifications, pending episode
groups and server/scheduled alerts cannot use legacy unapproved connections.

Automatic delivery uses one unrestricted Silo administrator API key and verifies the recipient's current
role and enabled status with Silo without trusting the cached role. Missing/revoked
keys pause delivery; temporary role lookup failures keep notifications retryable.
Normal users, deleted/disabled accounts and demoted administrators receive no alerts.
Monitoring still covers server-wide playback by all users; recipients are admins.

The plugin retains the upstream alert events, session controls and tasks. All calls use the Silo SDK v0.32.1.

Each administrator connection uses `server_url` and a secret `app_token`. An empty token can use the
admin default only when `server_url` equals `default_server_url`. Secrets are
stored using Silo instance state; API views return token hints.

`POST api/profiles/{id}/validate` checks `/version` without sending a notification
or disclosing the token. It only confirms server reachability. The profile test
route sends a real notification and verifies the application token.

Delivery: `POST {server_url}/message`, JSON body, `X-Gotify-Key` header.
Legacy tokens are sent unchanged. Enhanced `gtfya.<private-seed>` application
tokens sign each request using Gotify's Ed25519ph timestamp format. Only the public
key, timestamp and signature are transmitted. Client tokens (`gtfyc.`) and
pre-signed expiring credentials are not accepted as stored application tokens.
Use the private token shown when creating the Gotify application. The public
identifier has the same shape but cannot authenticate; a send test detects this.
Server path prefixes are supported. HTTP redirects are rejected.
Internal priorities -2/-1/0/1/2 map to Gotify 0/2/5/8/10. Level 2 is a single urgent
message. Quiet hours lower priority; Gotify clients determine sound behavior.
Silo links use Markdown. Device selection, per-message sounds, HTML and binary
poster uploads are unavailable. URL-based posters are supported for library additions. The corresponding legacy API fields must be
empty or false. Gotify has no Pushover monthly quota endpoints.

Retryable failures: network errors, timeouts, HTTP 408/429 and 5xx. Other HTTP
errors are permanent. Silo events retain upstream deduplication and retry handling.
A lost delivery acknowledgement can cause a duplicate when Silo retries.

Subscribed events: `notification.created`, `session.started`, `session.paused`,
`session.resumed`, `session.stopped`, `scan.failed`, `job.failed`.
Tasks: `health_check` every 5 minutes, `weekly_digest` hourly. Restart Silo after
changing task bindings. The same administrator API key handles monitoring and session control. Administrators can use separate Gotify applications for
private delivery.

## Library additions

`recently_added` is a scheduled task (suggested interval: 2 minutes). Its
`library.added` alert is off by default. Enabling it records a server-owned
activation time; existing items are excluded. The task reads bounded pages from
`GET /api/v2/admin/catalog/items?added_after=...&limit=20&cursor=...`, persists
its cursor and fixed scan window, and advances its watermark only on success.
Duplicates are recorded per administrator, content ID and first-seen timestamp.
A cluster lease limits concurrent scans, with the same at-least-once delivery
limitation as the existing event pipeline. A response lost after Gotify accepted
a message can still cause a duplicate. Twenty rows per administrator/run keep
large scans bounded; larger backlogs take several runs.

Library and media-type filters apply; playback-only filters do not. Movie and
episode rows are announced; a series container does not produce a duplicate
alert alongside its episodes. The title, year, episode numbers, resolution and
codecs, genres and available TMDB/IMDb ratings come from Silo. The optional host ListLibraryMedia/ResolveCatalogImageURLs
methods enrich exact content-ID matches with synopsis and artwork. Hosts that do
not implement them still receive text notifications. Availability of synopsis and artwork varies by Silo version, so images cannot be guaranteed. Public/resolved artwork URLs use Gotify Markdown and
`client::notification.bigImageUrl`; rendering depends on the receiving client.
No Silo API credential is placed in an image URL.

The catalog's `added_at` is a first-seen time, not a complete change cursor;
imports with deliberately backdated first-seen times may not produce an alert.
The two-minute overlap handles normal scan commit/timestamp boundaries.

Navigation now uses `/admin` and `/admin/activity`. Legacy Surify live-stream
page URLs redirect to native Silo activity. The old session-control JSON API is
retained for compatibility but is no longer advertised or configured by the UI.
The original transparent logo is distributed as a 360-pixel WebP (~28 KB).

## Single administrator key

`POST /api/admin/key-setup` now validates and saves the supplied administrator key itself. Request: `{"key":"sa_…"}`. An omitted or blank key preserves an already saved administrator key. `source: "legacy"` explicitly adopts a previously saved full-access credential after validation. The endpoint makes read-only checks against Silo; it never creates or revokes keys. Old requests asking to create scoped keys or revoke the pasted key receive HTTP 409 and must reload the UI.

`admin_keys.admin_key_set` indicates the new configuration. Compatibility flags `monitor_key_set` and `control_key_set` indicate effective availability without returning values. Existing scoped installations continue using their previous keys until the administrator supplies a single replacement key. Saving that replacement through the panel removes old credentials from the plugin configuration without revoking them on Silo. Gotify destinations and tokens are preserved.

Only `admin_api_key` is exposed as the Silo credential in the settings form. It is a password control and write-only secret. The old `monitor_api_key`, `control_api_key` and `silo_api_key` settings remain undeclared protected compatibility values, never public settings fields.
