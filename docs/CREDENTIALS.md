# Plugin credentials

| Plugin | Silo credential | External credential | Conditions |
| --- | --- | --- | --- |
| Surimatrix | One unrestricted API key owned by an administrator | A dedicated Matrix bot access token and matching device ID | A monitor-only key cannot read account/profile and item details. Keep the Matrix device and writable persistent directory across updates. |
| Duplicata Versions | One administrator API key | None | Use a dedicated key with 100 requests/s, 6000/min and burst 100. Deletion additionally needs authorized absolute folders, explicit enablement and a writable media mount. |
| Surify | One unrestricted administrator API key | A private Gotify application token | The same Silo key performs monitoring, recipient checks and stream-control actions. No second Silo key is needed. |
| Surichat | A dedicated administrator API key with `admin:monitor:read` | SMTP credentials if the mail provider requires them | The key lists enabled users and looks up recipient addresses. Chat history is kept in a persistent SQLite file. |

All Silo keys must belong to an administrator account. Use a different key for each plugin. In Surify, save the key in the **Silo API key** tab or the plugin's settings. The saved value is hidden; a blank edit retains it. Surify validates the supplied key without creating or revoking other API keys. Existing scoped installations continue working until a replacement administrator key is saved.

The Gotify application token and Silo administrator key are write-only secret fields with password controls. The plugin's status API returns only configured/not-configured flags. Previously used scoped keys are not automatically revoked because another application may still use them.

Matrix and Gotify tokens are credentials for their respective services; a Silo administrator key cannot replace them. The server name for Surimatrix notifications comes from Silo's public branding endpoint and needs no additional API key.
