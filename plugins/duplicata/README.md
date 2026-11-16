<p align="center">
  <img src="internal/review/web/logo.png" alt="Duplicata" width="240">
</p>

<h1 align="center">Duplicata Versions</h1>

<p align="center">Review movie and episode versions in your Silo libraries.</p>

## Install

Add the Suricata catalog in Silo to install the plugin and receive updates:

```text
https://raw.githubusercontent.com/deejayexe/silo-plugin-suricata/main/manifest.json
```

For manual installation, download the Linux binary for your architecture from [Releases](https://github.com/deejayexe/silo-plugin-suricata/releases).

## Setup

1. Save a dedicated administrator API key configured in Silo for **100 requests/s, 6000/min and burst 100**.
2. Leave the server URL and profile blank to use Silo's internal address and the account's primary profile.
3. Select a library, run a scan and review its duplicate versions.

## Delete files

Deletion is disabled by default. To enable it, configure **Authorized folders** using real, absolute paths inside the Silo container, enable **File deletion** and allow write access to the media mount.

Choose **Review deletion**, check the file to remove and the copy to retain, then confirm with **Delete file**. Deletion is permanent. The plugin rechecks both files and requires another verified file path to remain. Released space is shown as an estimate; hardlinks may retain the data.

## History

**Save to history** saves results without deleting media. **Continue review** reopens a saved analysis without rescanning; only complete analyses allow deletion. History is kept in memory and clears when Silo or the plugin process restarts.

Linux amd64 and arm64 · Administrator access · [MIT License](LICENSE)
