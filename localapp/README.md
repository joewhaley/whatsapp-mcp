# localapp — import from the local WhatsApp desktop app

This package reads the database that the **macOS WhatsApp desktop application**
keeps on disk and converts it into the canonical chat/message schema used by the
rest of this project. It lets the locally installed app act as a *source of
messages* — independent of, and usually far more complete than, the whatsmeow
history sync (the desktop app retains your full local history).

## Where the data lives

The app is sandboxed under a shared App Group container:

```
~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/
├── ChatStorage.sqlite      ← messages, chats, media (Core Data store)  [primary]
├── LID.sqlite              ← @lid ↔ phone-number mapping
├── ContactsV2.sqlite       ← address-book contacts
├── CallHistory.sqlite      ← calls
└── ...
```

All of these are ordinary SQLite databases in WAL mode. `ChatStorage.sqlite` and
`LID.sqlite` are the only two this importer reads.

> The app keeps these files open in WAL mode, so the importer **copies the
> `.sqlite` + `-wal` + `-shm` trio to a temp directory** before reading, to get a
> consistent snapshot without disturbing the running app. (`--no-copy` opts out.)

## ChatStorage.sqlite schema (the relevant bits)

It is an Apple Core Data store, so every table is prefixed `Z` and every row has
a `Z_PK` primary key.

### `ZWAMESSAGE` — one row per message

| Column           | Meaning                                                            |
|------------------|--------------------------------------------------------------------|
| `ZSTANZAID`      | WhatsApp message ID (used as the canonical `messages.id`)          |
| `ZTEXT`          | message text / caption                                             |
| `ZMESSAGEDATE`   | timestamp as an **NSDate** (seconds since 2001-01-01 UTC)          |
| `ZISFROMME`      | `1` if sent by the account owner                                   |
| `ZMESSAGETYPE`   | numeric content type (see below)                                   |
| `ZCHATSESSION`   | → `ZWACHATSESSION.Z_PK` (which chat)                               |
| `ZGROUPMEMBER`   | → `ZWAGROUPMEMBER.Z_PK` (sender, for group messages)               |
| `ZMEDIAITEM`     | → `ZWAMEDIAITEM.Z_PK` (attachment)                                 |
| `ZPARENTMESSAGE` | → `ZWAMESSAGE.Z_PK` (quoted/replied-to message)                    |
| `ZFROMJID`/`ZTOJID` | raw JIDs; for incoming group messages `ZFROMJID` is the *group* |

**Timestamp conversion:** `unix = ZMESSAGEDATE + 978307200`.

**`ZMESSAGETYPE` codes** (derived empirically, cross-checked against attached
media file extensions):

| Code | Meaning            | Code | Meaning                  |
|------|--------------------|------|--------------------------|
| 0    | text               | 8    | document                 |
| 1    | image              | 11   | GIF (stored as `.mp4`)   |
| 2    | video              | 14   | system / e2e notification|
| 3    | audio / voice note | 15   | sticker (`.webp`)        |
| 4    | contact (vCard)    | 6    | group event (add/remove) |
| 5    | location           | 7    | text with link preview   |

System/group-event types (6, 14) are skipped by default (`--include-system`
keeps them). When a media item is attached, its file extension is used to refine
the type label, since it is more reliable than the numeric code across app
versions.

### `ZWACHATSESSION` — one row per chat

`ZCONTACTJID` is the chat JID, `ZPARTNERNAME` the display name (contact name or
group subject), `ZLASTMESSAGEDATE`/`ZUNREADCOUNT` the obvious. `ZSESSIONTYPE`:
`0` = 1:1, `1` = group, `2` = broadcast, `3` = status, `4` = community group.
Broadcast/status pseudo-chats are not imported.

### `ZWAGROUPMEMBER` — group participants

`ZMEMBERJID` is the sender's JID (usually a `@lid`). For incoming group
messages, the real sender is found here via `ZWAMESSAGE.ZGROUPMEMBER`, **not**
`ZFROMJID` (which holds the group JID).

### `ZWAMEDIAITEM` — attachments

`ZMEDIALOCALPATH` (relative to the container's `Media/` tree), `ZFILESIZE`,
`ZMOVIEDURATION`, `ZTITLE`, `ZVCARDNAME/ZVCARDSTRING`, `ZLATITUDE/ZLONGITUDE`.
The importer records name/size/MIME/duration with `download_status = "external"`
and does **not** copy the files — they remain in the app's own store.

### `ZWAPROFILEPUSHNAME` — display names

`ZJID` → `ZPUSHNAME`. Imported into the `push_names` table.

## LID.sqlite — resolving `@lid` JIDs

Modern WhatsApp addresses people by an opaque **Linked ID** (`…@lid`) rather than
their phone number. To keep imported data consistent with the whatsmeow sync
(which normalizes to phone-number JIDs), the importer builds a `@lid → phone`
map from `LID.sqlite`:

- `ZWAPHONENUMBERLIDPAIR(ZLID, ZPHONENUMBER)` — older layout, and
- `ZWAZACCOUNT(ZIDENTIFIER, ZPHONENUMBER)` — current layout, where
  `ZIDENTIFIER` is the `…@lid` JID.

Unmapped `@lid` JIDs are kept as-is.

## How identities are resolved

| Message kind          | `sender_jid`                                            |
|-----------------------|---------------------------------------------------------|
| sent by me            | the owner JID (`--me`, else auto-detected)              |
| incoming 1:1          | the chat's contact JID                                  |
| incoming group        | `ZWAGROUPMEMBER.ZMEMBERJID`, normalized via the LID map |

The **owner JID** is resolved in order: the `--me` flag → the most common
`is_from_me` sender already in the destination DB → the "Message yourself" chat
(`ZWACHATSESSION` whose partner name is "You").

## Usage

See `cmd/localimport` and the project README. In short:

```bash
go run ./cmd/localimport --dry-run      # report what would be imported
go run ./cmd/localimport                # merge into data/db/messages.db
```
