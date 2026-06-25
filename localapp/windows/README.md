# Importing from the WhatsApp **for Windows** desktop app

This is the Windows counterpart to the macOS importer in [`../`](../README.md).
It is considerably more involved than macOS, because the native WhatsApp for
Windows app (Microsoft Store package `5319275A.WhatsAppDesktop`, the WebView2
architecture shipped since Dec 2025) stores nothing in plaintext.

## Why Windows is different

| | macOS | Windows |
|---|---|---|
| Message store | `ChatStorage.sqlite`, plaintext Core Data | split across two encrypted stores |
| Read it directly? | yes | no — needs decryption **and** deserialization |

On Windows the data lives in two places, and you need **both**:

1. **`genericStorage.db`** — a **SQLCipher**-encrypted SQLite under
   `LocalState\sessions\<sha1>\`. Its `message(id, chatId, timestamp, text)`
   table holds the plaintext **body text** (it backs the app's full-text
   search). Decrypt it with **ZAPiXDESK** (below).

2. The **WebView2 IndexedDB** at
   `LocalCache\EBWebView\Default\IndexedDB\https_web.whatsapp.com_0.indexeddb.leveldb`
   — a Chromium LevelDB whose values are V8 *structured-clone* blobs. Its
   `message` object store holds the rich **metadata**: direction (sent/received),
   sender, message type, real WhatsApp message IDs, quoted-message references and
   media descriptors. (The body text *is* in here too, but encrypted at rest in
   `msgRowOpaqueData`, so we take the text from `genericStorage` instead.)

The two are joined on a shared row id: IndexedDB `message.rowId` ==
`genericStorage.message.id`. The Python pre-step
[`extract_whatsapp_windows.py`](extract_whatsapp_windows.py) does that join and
the IndexedDB deserialization, writing one **intermediate SQLite** that the Go
importer (`cmd/localimport -platform windows`) maps onto the project's canonical
schema — the same schema the macOS importer and the live whatsmeow sync write,
so the rows are interchangeable and the import is idempotent.

```
  ┌─ genericStorage.db (SQLCipher) ──ZAPiXDESK──► genericStorage.dec.db ─┐
  │                                                                      ├─ extract_whatsapp_windows.py ─► wa-windows-export.db ─► localimport ─► data/db/messages.db
  └─ EBWebView IndexedDB (LevelDB + V8 SSV) ─────dfindexeddb────────────┘
```

## Prerequisites

* **Python 3.11 or 3.12** (not 3.13+, where `dfindexeddb`'s wheels aren't yet
  available). Then:

  ```bash
  python -m pip install -r localapp/windows/requirements.txt
  ```

  `dfindexeddb` is Google's IndexedDB/LevelDB forensic library; it does the V8
  structured-clone deserialization. `python-snappy` (pure-Python wheel, backed by
  `cramjam`) handles LevelDB block decompression.

* **PowerShell** + local administrator rights for ZAPiXDESK (it closes WhatsApp,
  reads the device key via the TPM, and decrypts the SQLCipher files).

## Step 1 — decrypt the SQLCipher databases (ZAPiXDESK)

Clone <https://github.com/kraftdenker/ZAPiXDESK> (GPLv3) and run it **as the
logged-in Windows user**, as administrator (it self-elevates). It will close
WhatsApp, copy the `LocalState` tree, derive the keys and decrypt every database:

```powershell
# unblock the bundled BouncyCastle dll first
Get-ChildItem .\ZAPiXDESK -Recurse | Unblock-File
.\ZAPiXDESK.ps1 `
  -WhatsAppPath "$env:LOCALAPPDATA\Packages\5319275A.WhatsAppDesktop_cv1g1gvanyjgm\LocalState" `
  -OutputPath   "$env:TEMP\wa-decrypt"
```

The output zip contains the decrypted databases; you need two of them:

* `sessions\<sha1>\genericStorage.dec.db` — message text
* `sessions\<sha1>\contacts.dec.db` — contact display names

> ZAPiXDESK must run on the original machine while it is powered on: the
> SQLCipher key is derived from a device-bound id (TPM/registry). See the
> ZAPiXDESK README for offline/`-GetID` options.

## Step 2 — locate the IndexedDB

No decryption is needed for the IndexedDB, but WhatsApp keeps it open. For a
consistent snapshot, close WhatsApp and copy the folder:

```powershell
$idb = "$env:LOCALAPPDATA\Packages\5319275A.WhatsAppDesktop_cv1g1gvanyjgm\LocalCache\EBWebView\Default\IndexedDB\https_web.whatsapp.com_0.indexeddb.leveldb"
robocopy $idb "$env:TEMP\wa-idb" /E
```

(ZAPiXDESK in step 1 already closed WhatsApp, so copying right after it is ideal.)

## Step 3 — extract + join into the intermediate database

```bash
python localapp/windows/extract_whatsapp_windows.py \
  --idb      "%TEMP%/wa-idb" \
  --generic  "%TEMP%/wa-decrypt/.../sessions/<sha1>/genericStorage.dec.db" \
  --contacts "%TEMP%/wa-decrypt/.../sessions/<sha1>/contacts.dec.db" \
  --out      "wa-windows-export.db"
```

It prints a summary like:

```
Wrote wa-windows-export.db: 62558 messages (50198 with text), 362 group subjects,
796 chats, 11274 lid mappings; owner='16502833196@c.us'
```

## Step 4 — import into messages.db

```bash
go run ./cmd/localimport -platform windows -export wa-windows-export.db
# dry run first if you like:
go run ./cmd/localimport -platform windows -export wa-windows-export.db -dry-run
```

`-platform` defaults to `auto` (your OS), so on a Windows host you can omit it.
Other flags (`-db`, `-me`, `-since`, `-chat`, `-limit`, `-include-system`,
`-dry-run`) work exactly as for macOS. The import is idempotent — keyed by the
real WhatsApp message IDs — so it is safe to re-run and to run alongside the live
sync.

## What you get / known limits

* **Direction, sender, type, quotes, real message IDs** — all recovered.
* **Group subjects** and **1:1 contact names** are resolved; `@lid` identities
  are normalized to phone-number JIDs (via the IndexedDB `contact` store and
  `contacts.dec.db`), matching the live sync.
* **Body text** comes from `genericStorage`, which is a rolling full-text-search
  window. In practice ~90% of messages have text; media-only messages and the
  most recent messages outside the window fall back to a `[Image]`/`[Video]`/…
  placeholder (same convention as the live sync).
* **Media files are not copied.** Media rows are recorded as metadata with
  `download_status = "external"`; the bytes stay in the app's own store.
* **System/notification messages** (`gp2`, `e2e_notification`, `call_log`,
  `revoked`, …) are skipped unless you pass `-include-system`.
* **V8 structured-clone version:** the current app serializes IndexedDB values
  with V8 SSV **v16**; the extractor lifts `dfindexeddb`'s version cap to read it.
  If a future build moves past v16, bump the constant near the top of
  `extract_whatsapp_windows.py`.
