# Importing from a **Google Drive backup** (Android)

This is the Android counterpart to the [macOS](../README.md) and
[Windows](../windows/README.md) importers. WhatsApp on **Android** backs your
full chat history up to **Google Drive**, encrypted end-to-end. Once you download
and decrypt that backup you get a plaintext **`msgstore.db`** — the Android chat
database — which the Go importer (`cmd/localimport -platform android`) maps onto
this project's canonical schema, the same one the macOS importer and the live
whatsmeow sync write. The import is idempotent and keyed by the real WhatsApp
message IDs, so it is safe to re-run and to run alongside the live sync.

```
  Google Drive (msgstore.db.crypt15) ──wabdd download──► …/Databases/msgstore.db.crypt15
                                       ──wabdd decrypt ──► …-decrypted/Databases/msgstore.db
                                       ──localimport ────► data/db/messages.db
```

The download + crypt15 decryption is done by the excellent, maintained
**[`wabdd`](https://github.com/giacomoferretti/whatsapp-backup-downloader-decryptor)**
tool (Apache-2.0) — exactly as the Windows importer leans on ZAPiXDESK and
`dfindexeddb` for its decryption pre-step. This project does **not** re-implement
the Google authentication or the crypt15 cipher; it consumes `wabdd`'s output.

> [!IMPORTANT]
> **Your backup must be end-to-end encrypted (`.crypt15`).** This is the part
> that trips people up. WhatsApp uploads one of two formats to Drive:
>
> * **`msgstore.db.crypt15`** — when **end-to-end encrypted backup** is **on**.
>   Decryptable with your **64-digit key**. ✅ supported here.
> * **`msgstore.db.crypt14`** — the default when E2E backup is **off**. Encrypted
>   with the app's on-device `key` file, which is **not** in the Drive backup and
>   needs a rooted device to extract. ❌ `wabdd` cannot decrypt it.
>
> If your most recent backup is `crypt14`, turn on **WhatsApp → Settings → Chats
> → Chat backup → End-to-end encrypted backup** (choose *Use 64-digit encryption
> key*), let it back up once, and then follow the steps below. You can confirm
> the format with the listing snippet in [Checking your backup](#checking-your-backup).

## Prerequisites

1. **Python 3.9+** and `wabdd`. The easiest install is with
   [`uv`](https://docs.astral.sh/uv/) or `pipx`:

   ```bash
   uv tool install wabdd       # or: pipx install wabdd
   # one-off without installing: uvx wabdd --help
   ```

2. Your **64-digit hex encryption key**. `wabdd` only supports backups secured
   with the 64-digit key (not the password variant). On your phone:

   > **WhatsApp → Settings → Chats → Chat backup → End-to-end encrypted backup**

   Turn it on (or switch it to) **"Use 64-digit encryption key instead"** and
   save the key. Put those 64 hex characters in a file, e.g. `key.txt`.

3. The Google account that holds the backup, and access to it in a browser to
   copy a one-time `oauth_token` cookie (next step).

## Step 1 — get a Google token

```bash
wabdd token YOUR_GOOGLE_EMAIL@gmail.com
```

It prints a URL — visit <https://accounts.google.com/EmbeddedSetup>, log in with
that Google account, and copy the **`oauth_token`** cookie value from your
browser's developer tools (Application → Cookies). Paste it back. `wabdd` saves a
master token under `tokens/`.

### Checking your backup

Before downloading gigabytes, confirm a backup exists and — crucially — that its
chat database is **`.crypt15`** (see the callout above). This lists your backups
and the chat DB filename without downloading anything:

```bash
uv run --no-project --with wabdd python - <<'PY'
import json, pathlib
from wabdd.wabackup import WaBackup
tok = pathlib.Path("tokens")
auth = next(tok.glob("*_token.txt")).read_text().strip()
master = next(tok.glob("*_mastertoken.txt")).read_text().strip()
wa = WaBackup(auth, master)
for b in wa.get_backups():
    md = json.loads(b.get("metadata", "{}"))
    print(b["name"].split("/")[-1], b.get("updateTime"),
          "E2E:", md.get("encryptedBackupEnabled"),
          f'{int(md.get("backupSize",0))/1e9:.1f}GB')
    for f in wa.backup_files(b):
        rel = "/".join(f["name"].split("/")[5:])
        if rel.startswith("Databases/"):
            print("  ", rel, f'{int(f.get("sizeBytes",0))/1e6:.0f}MB')
PY
```

If the chat DB ends in `.crypt14`, stop and enable the E2E encrypted backup first
(see the callout above) — the steps below need `.crypt15`.

## Step 2 — download the backup

```bash
wabdd download --master-token tokens/YOUR_GOOGLE_EMAIL_gmail_com_mastertoken.txt
```

This downloads the backup into `backups/<phone>_<date>/`. The chat database lands
at `backups/<phone>_<date>/Databases/msgstore.db.crypt15`. (Media, if present,
downloads under `Media/` — the importer does not need it; see limitations.)

> To skip large media and fetch only the chat database faster, you can pass
> `--decryption-key-file key.txt --exclude 'Media/*'`.

## Step 3 — decrypt to a plaintext `msgstore.db`

```bash
wabdd decrypt --key-file key.txt dump backups/<phone>_<date>
```

This writes a sibling folder `backups/<phone>_<date>-decrypted/` containing the
decrypted **`Databases/msgstore.db`**.

## Step 4 — import into `messages.db`

```bash
# Dry run first (no writes):
go run ./cmd/localimport -platform android \
    -msgstore "backups/<phone>_<date>-decrypted/Databases/msgstore.db" -dry-run

# Then import (recommended alongside a live sync: -no-overwrite):
go run ./cmd/localimport -platform android \
    -msgstore "backups/<phone>_<date>-decrypted/Databases/msgstore.db" -no-overwrite
```

All the shared flags work exactly as for macOS/Windows: `-db`, `-me`, `-since`,
`-chat`, `-limit`, `-include-system`, `-dry-run`, `-no-overwrite`.

### `-no-overwrite` (recommended when a live sync already populated the DB)

By default the import upserts every row. `-no-overwrite` instead **only inserts
messages the database lacks**, leaving existing rows untouched — *except* that a
row whose body is a live-sync placeholder (`[Protocol]`, `[Image]`, …) is
upgraded to the real text when the backup has it. This is the safe way to enrich
a database the whatsmeow sync already populated.

## What you get / known limits

* **Full chat history**, **direction**, **timestamps**, **per-message type**,
  **group senders**, **quoted/replied-to message IDs**, and the **real WhatsApp
  message IDs** — all recovered, so the import dedupes cleanly against the live
  sync.
* **Group subjects** are recovered (from the `chat` table when present). **1:1
  contact display names are *not*** — WhatsApp's contacts database (`wa.db`) is
  *not* part of the Google Drive backup (it is rebuilt from the phone's address
  book on restore), so DMs are identified by phone-number JID. Run `-no-overwrite`
  to keep any names a live sync already captured.
* **`@lid` identities** are resolved to phone-number JIDs via the `jid_map`
  table inside `msgstore.db` (the modern store carries its own lid↔phone map). In
  practice resolution is near-complete; only participants with no mapping remain
  under their `@lid`.
* **Your own number / number changes.** The owner JID is auto-detected as the
  outgoing-message sender spanning the most chats. WhatsApp's `lid` is stable
  across number changes, so if you have changed numbers, auto-detection (and the
  backup's filename) may show a different number than your current one. Pass
  `-me <your-number>` to set it explicitly.
* **Media files are not copied.** Media rows are recorded as metadata with
  `download_status = "external"`; the bytes stay in the downloaded backup tree.
* **System/notification messages** (group events, e2e notices, …) are skipped
  unless you pass `-include-system`.
* **`crypt15` (end-to-end encrypted) backups only.** A default non-E2E backup is
  `crypt14`, which `wabdd` cannot decrypt (see the callout at the top). Enable the
  E2E encrypted backup to get a `crypt15`.
* **Modern schema only.** `crypt15` backups always use the modern
  `message`/`chat`/`jid` schema (WhatsApp ≥ 2021). A legacy
  `messages`-table-only database is rejected with a clear error.

## How the schema is mapped

The decrypted `msgstore.db` is read directly in Go ([`../androidimport.go`](../androidimport.go)):

| canonical | Android `msgstore.db` |
|---|---|
| chat JID | `chat.jid_row_id` → `jid.raw_string` (normalized) |
| message id | `message.key_id` |
| from-me / timestamp | `message.from_me`, `message.timestamp` (ms) |
| sender (1:1) | the chat's JID |
| sender (group) | `message.sender_jid_row_id` → `jid` |
| type | `message.message_type`, refined by `message_media.mime_type` |
| text / caption | `message.text_data` |
| media | `message_media` (`file_path`, `file_size`, `mime_type`, duration) |
| reply-to | `message_quoted.key_id` |
| system event | `message_system.action_type` (skipped by default) |
| `@lid` → phone | `jid_map(lid_row_id → jid_row_id)` |

Optional tables/columns (`message_media`, `message_quoted`, `message_system`,
group subjects, unread counts) are probed for at open time and used when present,
so the reader degrades gracefully across WhatsApp versions.
