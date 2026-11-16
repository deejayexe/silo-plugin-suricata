# Plugin credentials

| Plugin | Silo credential | External credential | Conditions |
| --- | --- | --- | --- |
| Surimatrix | One unrestricted API key owned by an administrator | A dedicated Matrix bot access token and matching device ID | A monitor-only key cannot read account/profile and item details. Keep the Matrix device and writable persistent directory across updates. |
| Duplicata Versions | One administrator API key | None | Use a dedicated key with 100 requests/s, 6000/min and burst 100. Deletion additionally needs authorized absolute folders, explicit enablement and a writable media mount. |
| Surify | A key with `admin:monitor:read` | A private Gotify application token | Background delivery verifies that recipients are still enabled administrators. An optional second Silo key with `admin:sessions:control` is needed only for stream-control actions. |

All Silo keys must belong to an administrator account. You can create an administrator key for Surify and pass it to the panel's key-setup assistant, which generates and saves the monitoring key. Its setup API also supports creating the optional control key. You can supply an existing scoped monitoring key instead. The one-time full-access key is not saved by the assistant; it can be revoked after setup if requested.

Use different keys for each plugin. If a full-access key is also used elsewhere, do not select the option to revoke it during Surify setup. Saved Surify application, monitoring and control credentials are declared as write-only secrets and use password controls; edits with a blank field retain the existing secret. The plugin's status API returns only configured/not-configured flags.

Matrix and Gotify tokens are credentials for their respective services; a Silo administrator key cannot replace them. The server name for Surimatrix notifications comes from Silo's public branding endpoint and needs no additional API key.
