#!/usr/bin/env python3
"""Extract the WhatsApp for Windows desktop app history into a plaintext SQLite.

The native WhatsApp for Windows app (package 5319275A.WhatsAppDesktop) stores its
data very differently from the macOS app:

  * Rich message *metadata* (direction, sender, type, timestamp, quoted message,
    media descriptors, real WhatsApp message IDs) lives in the Edge WebView2
    IndexedDB at LocalCache/EBWebView/Default/IndexedDB/https_web.whatsapp.com_*
    -- a Chromium LevelDB whose values are V8 "structured clone" blobs. The
    message *body text* in there is encrypted at rest (msgRowOpaqueData), so it
    cannot be read from IndexedDB alone.

  * The plaintext message *body text* lives in genericStorage.db (a SQLCipher
    database). It must first be decrypted with ZAPiXDESK
    (https://github.com/kraftdenker/ZAPiXDESK), which yields genericStorage.dec.db
    with a `message(id, chatId, timestamp, text)` table.

This script joins the two on the shared row id (IndexedDB message.rowId ==
genericStorage message.id) and writes a single intermediate SQLite database that
the Go importer (whatsapp-mcp/localapp, GOOS=windows) maps onto the project's
canonical schema. It also extracts contacts (lid<->phone + names), group subjects
and chat metadata.

Prerequisites:
    pip install dfindexeddb python-snappy

Usage:
    python extract_whatsapp_windows.py \
        --idb     ".../LocalCache/EBWebView/Default/IndexedDB/https_web.whatsapp.com_0.indexeddb.leveldb" \
        --generic ".../genericStorage.dec.db" \
        --contacts ".../contacts.dec.db" \
        --out      "wa-windows-export.db"

See localapp/windows/README.md for the full end-to-end procedure.
"""
from __future__ import annotations

import argparse
import logging
import pathlib
import sqlite3
import sys

# dfindexeddb floods stderr with per-record parse warnings; silence them.
logging.disable(logging.CRITICAL)

from dfindexeddb.indexeddb.chromium import v8  # noqa: E402

# WhatsApp on the current WebView2 build serializes IndexedDB values with V8
# structured-clone version 16; dfindexeddb caps at 15 and would reject the header.
# The wire format is unchanged for the plain objects WhatsApp stores, so lifting
# the cap lets them decode. Bump if a future build moves past 16.
if v8.ValueDeserializer.LATEST_VERSION < 16:
    v8.ValueDeserializer.LATEST_VERSION = 16

from dfindexeddb.leveldb import record as ldb  # noqa: E402
from dfindexeddb.indexeddb.chromium.record import (  # noqa: E402
    IndexedDbKey,
    ObjectStoreDataKey,
)
from dfindexeddb.indexeddb import types as idb_types  # noqa: E402

# WhatsApp model database + object store ids (database "model-storage").
MODEL_DB_ID = 3
OS_CONTACT = 4
OS_CHAT = 7
OS_MESSAGE = 8
OS_GROUP_METADATA = 21


def clean(value):
    """Convert dfindexeddb sentinel types into plain Python values."""
    if isinstance(value, (idb_types.Undefined, idb_types.Null)):
        return None
    if isinstance(value, idb_types.JSArray):
        return [clean(v) for v in value.values]
    if isinstance(value, dict):
        return {k: clean(v) for k, v in value.items()}
    if isinstance(value, list):
        return [clean(v) for v in value]
    return value


def jid_str(value):
    """Return the canonical "_serialized" string for a WhatsApp jid value."""
    value = clean(value)
    if isinstance(value, dict):
        return value.get("_serialized") or ""
    if isinstance(value, str):
        return value
    return ""


def parse_message_key(key: str):
    """Split a WhatsApp message key "fromMe_chatId_stanzaId_participant".

    The chat id is the *conversation* (the other party / group) regardless of
    direction -- unlike message.from, which is the owner on sent messages. None
    of the four fields contain '_' (jids use '@'/'-' and ids are alnum), so a
    plain split is unambiguous. Returns (from_me, chat, stanza_id, participant).
    """
    from_me = key.startswith("true_")
    rest = key.split("_", 1)[1] if "_" in key else key
    bits = rest.split("_")
    chat = bits[0] if bits else ""
    stanza = bits[1] if len(bits) > 1 else ""
    participant = bits[2] if len(bits) > 2 else ""
    return from_me, chat, stanza, participant


def load_generic_text(path: str) -> dict[int, tuple[str, int]]:
    """Map genericStorage rowid -> (text, timestamp) from the decrypted FTS store."""
    out: dict[int, tuple[str, int]] = {}
    con = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        for rid, text, ts in con.execute(
            "SELECT id, text, timestamp FROM message"
        ):
            try:
                out[int(rid)] = (text or "", int(ts) if ts is not None else 0)
            except (TypeError, ValueError):
                continue
    finally:
        con.close()
    return out


def load_userstatus_names(path: str):
    """From contacts.dec.db UserStatuses, return (names, lid_map).

    names:   bare-jid/lid -> best display name
    lid_map: lid -> phone number (bare)
    """
    names: dict[str, str] = {}
    lid_map: dict[str, str] = {}
    con = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        rows = con.execute(
            "SELECT Jid, DbLid, ContactName, FirstName, PushName FROM UserStatuses"
        ).fetchall()
    except sqlite3.OperationalError:
        rows = []
    finally:
        con.close()
    for jid, dblid, contact_name, first_name, push_name in rows:
        name = (contact_name or first_name or push_name or "").strip()
        phone = bare_user(jid)
        lid = bare_user(dblid)
        if lid and phone:
            lid_map[lid] = phone
        if name:
            if phone:
                names[phone] = name
            if lid:
                names.setdefault(lid, name)
    return names, lid_map


def bare_user(raw) -> str:
    """Return the user part of a jid-ish string, stripping server/device."""
    if not raw:
        return ""
    raw = str(raw).strip()
    if "@" in raw:
        raw = raw.split("@", 1)[0]
    for sep in (":", "."):
        if sep in raw:
            raw = raw.split(sep, 1)[0]
    return raw


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--idb", required=True,
                    help="path to the https_web.whatsapp.com_*.indexeddb.leveldb folder")
    ap.add_argument("--generic", required=True,
                    help="path to the ZAPiXDESK-decrypted genericStorage.dec.db")
    ap.add_argument("--contacts", default="",
                    help="path to the decrypted contacts.dec.db (names + lid map)")
    ap.add_argument("--out", required=True, help="output intermediate SQLite path")
    args = ap.parse_args(argv[1:])

    idb_dir = pathlib.Path(args.idb)
    if not idb_dir.is_dir():
        ap.error(f"--idb is not a directory: {idb_dir}")
    for p in (args.generic,):
        if not pathlib.Path(p).is_file():
            ap.error(f"file not found: {p}")

    print(f"Loading plaintext text from {args.generic} ...", file=sys.stderr)
    text_by_row = load_generic_text(args.generic)
    print(f"  {len(text_by_row)} text rows", file=sys.stderr)

    names: dict[str, str] = {}
    lid_map: dict[str, str] = {}
    if args.contacts and pathlib.Path(args.contacts).is_file():
        print(f"Loading names/lid map from {args.contacts} ...", file=sys.stderr)
        names, lid_map = load_userstatus_names(args.contacts)
        print(f"  {len(names)} names, {len(lid_map)} lid mappings", file=sys.stderr)

    # Accumulators keyed for dedup; keep the highest-sequence record per key.
    messages: dict[str, tuple[int, dict]] = {}     # message key -> (seq, value)
    groups: dict[str, str] = {}                    # group jid -> subject
    chats: dict[str, tuple[float, int]] = {}       # chat jid -> (last_t, unread)
    contacts_lidphone: dict[str, str] = {}         # lid -> phone (bare)
    owner_votes: dict[str, int] = {}

    print(f"Scanning IndexedDB {idb_dir} ...", file=sys.stderr)
    scanned = 0
    for lr in ldb.FolderReader(idb_dir).GetRecords():
        try:
            key = IndexedDbKey.FromBytes(lr.record.key, base_offset=lr.record.offset)
        except Exception:
            continue
        if not isinstance(key, ObjectStoreDataKey):
            continue
        kp = key.key_prefix
        if kp.database_id != MODEL_DB_ID:
            continue
        if not lr.record.value:  # tombstone / deleted
            continue
        osid = kp.object_store_id
        if osid not in (OS_MESSAGE, OS_CONTACT, OS_CHAT, OS_GROUP_METADATA):
            continue
        try:
            parsed = key.ParseValue(lr.record.value)
            val = clean(getattr(parsed, "value", None))
        except Exception:
            continue
        if not isinstance(val, dict):
            continue
        seq = lr.record.sequence_number or 0
        scanned += 1

        if osid == OS_MESSAGE:
            try:
                uk = key.encoded_user_key.value
            except Exception:
                continue
            if not isinstance(uk, str):
                continue
            prev = messages.get(uk)
            if prev is None or seq >= prev[0]:
                messages[uk] = (seq, val)
        elif osid == OS_GROUP_METADATA:
            subj = val.get("subject")
            if subj:
                groups[str(val.get("id") or "")] = str(subj)
        elif osid == OS_CHAT:
            cid = str(val.get("id") or "")
            if cid:
                t = val.get("t") or 0
                unread = val.get("unreadCount") or 0
                try:
                    t = float(t)
                except (TypeError, ValueError):
                    t = 0.0
                cur = chats.get(cid)
                if cur is None or t >= cur[0]:
                    chats[cid] = (t, int(unread or 0))
        elif osid == OS_CONTACT:
            lid = bare_user(val.get("id"))
            phone = bare_user(val.get("phoneNumber"))
            if lid and phone:
                contacts_lidphone[lid] = phone

    print(f"  scanned {scanned} model records; "
          f"{len(messages)} messages, {len(groups)} groups, "
          f"{len(chats)} chats, {len(contacts_lidphone)} contact lid->phone",
          file=sys.stderr)

    # Merge contact lid->phone into the lid map (UserStatuses takes precedence).
    for lid, phone in contacts_lidphone.items():
        lid_map.setdefault(lid, phone)

    # ---- Write the intermediate SQLite -------------------------------------
    out_path = pathlib.Path(args.out)
    if out_path.exists():
        out_path.unlink()
    db = sqlite3.connect(str(out_path))
    db.executescript(
        """
        CREATE TABLE messages (
            stanza_id   TEXT,
            row_id      INTEGER,
            chat        TEXT,
            sender      TEXT,
            from_me     INTEGER,
            type        TEXT,
            t           INTEGER,
            text        TEXT,
            quoted_id   TEXT,
            media_mimetype TEXT,
            media_filename TEXT,
            media_filesize INTEGER,
            media_duration INTEGER
        );
        CREATE TABLE chats   (jid TEXT PRIMARY KEY, last_t INTEGER, unread INTEGER, is_group INTEGER);
        CREATE TABLE groups  (jid TEXT PRIMARY KEY, subject TEXT);
        CREATE TABLE contacts(ident TEXT PRIMARY KEY, name TEXT);
        CREATE TABLE lid_map (lid TEXT PRIMARY KEY, phone TEXT);
        CREATE TABLE meta    (key TEXT PRIMARY KEY, value TEXT);
        """
    )

    n_msg = n_text = 0
    rows = []
    for uk, (_seq, m) in messages.items():
        from_me, chat, stanza, participant = parse_message_key(uk)
        if not chat or not stanza:
            continue
        author = jid_str(m.get("author"))
        to_jid = jid_str(m.get("to"))
        # On INCOMING messages, "to" is the account owner (me). On sent messages
        # "to" is the chat/group, so only vote from incoming non-group messages.
        if not from_me and to_jid and not to_jid.endswith("@g.us"):
            owner_votes[to_jid] = owner_votes.get(to_jid, 0) + 1
        sender = participant or author
        row_id = m.get("rowId")
        try:
            row_id = int(row_id) if row_id is not None else None
        except (TypeError, ValueError):
            row_id = None
        text = ""
        if row_id is not None and row_id in text_by_row:
            text = text_by_row[row_id][0]
            n_text += 1
        t = m.get("t") or 0
        try:
            t = int(float(t))
        except (TypeError, ValueError):
            t = 0
        rows.append((
            stanza, row_id, chat, sender, 1 if from_me else 0,
            str(m.get("type") or ""), t, text,
            str(m.get("quotedStanzaID") or ""),
            str(m.get("mimetype") or "") or None,
            str(m.get("filename") or "") or None,
            int(m["size"]) if isinstance(m.get("size"), (int, float)) else None,
            int(m["duration"]) if isinstance(m.get("duration"), (int, float)) else None,
        ))
        n_msg += 1

    db.executemany(
        "INSERT INTO messages VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", rows
    )
    db.executemany(
        "INSERT OR REPLACE INTO groups VALUES (?,?)", list(groups.items())
    )
    db.executemany(
        "INSERT OR REPLACE INTO chats VALUES (?,?,?,?)",
        [(j, int(t), u, 1 if j.endswith("@g.us") else 0) for j, (t, u) in chats.items()],
    )
    db.executemany(
        "INSERT OR REPLACE INTO contacts VALUES (?,?)",
        [(ident, name) for ident, name in names.items()],
    )
    db.executemany(
        "INSERT OR REPLACE INTO lid_map VALUES (?,?)", list(lid_map.items())
    )

    owner = max(owner_votes, key=owner_votes.get) if owner_votes else ""
    db.executemany(
        "INSERT OR REPLACE INTO meta VALUES (?,?)",
        [("owner_jid", owner),
         ("message_count", str(n_msg)),
         ("text_matched", str(n_text)),
         ("source", "whatsapp-windows/genericStorage+indexeddb")],
    )
    db.commit()
    db.close()

    print(f"Wrote {out_path}: {n_msg} messages "
          f"({n_text} with text), {len(groups)} group subjects, "
          f"{len(chats)} chats, {len(lid_map)} lid mappings; owner={owner!r}",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
