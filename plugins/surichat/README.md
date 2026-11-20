<p align="center">
  <img src="brand/surichat.png" alt="Surichat" width="240">
</p>

<h1 align="center">Surichat</h1>

<p align="center">Private support chat for Silo users and administrators.</p>

## Install

In Silo Web, open **Administration → Plugins → Repositories** and add the Suricata catalog:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

Refresh the catalog and install **Surichat**. For manual installation, download the Surichat binary for your Linux architecture from [Releases](https://github.com/deejayexe/silo-plugin-suricata/releases).

## Setup

1. In **Surichat settings**, set **Absolute SQLite path** to a writable, persistent path inside your Silo server or container. The plugin creates the database when first opened. Copy the database before reinstalling and point the new installation to the same file to keep your chat history.
2. To let administrators start a conversation and look up notification recipients, set **Internal Silo URL** and save a dedicated **Administrator API key** with the `admin:monitor:read` scope. Use the server base URL, for example `http://127.0.0.1:8080` with the standard Silo Docker setup.
3. Open **Surichat** from the Silo Web user menu. Users can write to their Silo administrator. Administrators see unread counts by conversation and can start a chat from the list of enabled user accounts. Press **Enter** to send or **Shift+Enter** to add a line break.

## Email reminders

Enable **Email reminders** and enter your SMTP settings, a support mailbox address, and the public Silo URL. Surichat sends alerts about user messages to the support mailbox and alerts about replies to the user's Silo email address, when available.

The first reminder defaults to **10 minutes** after an unread message and the second to **4 hours** after the first attempt. Both delays can be changed in the settings. Each message can generate up to two attempts. Reading a message stops pending reminders. For the shared support mailbox, a reading confirmation by any administrator stops reminders for that message.

Reminders use the server's public branding and include a sign-in link when a public URL is set. Chat messages remain in the SQLite database.

Linux amd64 and arm64 · English and Spanish · [MIT License](LICENSE)
