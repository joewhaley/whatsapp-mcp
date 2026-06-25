package localapp

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// AndroidStore reads the decrypted Android WhatsApp message database
// (msgstore.db) and exposes it as a canonical message Source, so a Google Drive
// backup can act as a source of history.
//
// The Drive backup ships the message store encrypted as msgstore.db.crypt15;
// the download + decryption is handled by the external `wabdd` tool (see
// localapp/android/README.md), which yields a plaintext SQLite file. Unlike the
// Windows app — whose data is split across two encrypted stores and needs a
// Python join step — the decrypted msgstore.db is a single plaintext SQLite that
// this reader maps directly onto the project's canonical schema, reusing the
// same JID-normalization and placeholder conventions as the macOS reader so
// imported rows are interchangeable with the live whatsmeow sync.
//
// It targets the modern schema (the "message" / "chat" / "jid" tables used since
// 2021, which is what every crypt15 backup contains). Optional tables and
// columns (message_media, message_quoted, message_system, group subjects, unread
// counts) are probed for and used when present, so it degrades gracefully across
// WhatsApp versions.
type AndroidStore struct {
	db *sql.DB

	lidMap   map[string]string       // bare lid -> bare phone number (best-effort)
	jidByRow map[int64]string        // jid._id -> canonical JID
	chatJID  map[int64]string        // chat._id -> canonical JID
	chats    map[string]*androidChat // canonical JID -> chat
	ownerJID string

	// schema capabilities, probed at open time
	hasMedia     bool
	hasQuoted    bool
	hasSystem    bool
	mediaDurExpr string // SQL expression for media duration, or "" when absent
	chatSubject  bool   // chat.subject column present
	chatUnread   string // chat unread column name, or "" when absent
}

type androidChat struct {
	jid         string
	name        string
	isGroup     bool
	lastMessage time.Time
	unread      int
}

// AndroidOptions configures opening an AndroidStore.
type AndroidOptions struct {
	// MsgstorePath is the path to the decrypted msgstore.db (required).
	MsgstorePath string
	// OwnerJID overrides owner detection (bare number or full JID).
	OwnerJID string
}

// OpenAndroid opens and prepares an AndroidStore from a decrypted msgstore.db.
func OpenAndroid(opts AndroidOptions) (*AndroidStore, error) {
	if opts.MsgstorePath == "" {
		return nil, fmt.Errorf("MsgstorePath is required")
	}
	db, err := openSQLite(opts.MsgstorePath, true)
	if err != nil {
		return nil, fmt.Errorf("open msgstore: %w", err)
	}
	if !tableExists(db, "message") {
		db.Close()
		return nil, fmt.Errorf("%q does not look like a modern WhatsApp msgstore.db "+
			"(no \"message\" table). Only the modern schema produced by crypt15 backups "+
			"is supported; decrypt the backup with wabdd (see localapp/android/README.md)", opts.MsgstorePath)
	}

	s := &AndroidStore{
		db:       db,
		lidMap:   map[string]string{},
		jidByRow: map[int64]string{},
		chatJID:  map[int64]string{},
		chats:    map[string]*androidChat{},
	}
	s.probeSchema()

	if err := s.loadJIDs(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load jids: %w", err)
	}
	if err := s.loadChats(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load chats: %w", err)
	}

	if opts.OwnerJID != "" {
		s.ownerJID = normalizeUserJID(ensureServer(opts.OwnerJID), s.lidMap)
	} else {
		s.ownerJID = s.detectOwnerJID()
	}
	return s, nil
}

// Close releases the database handle.
func (s *AndroidStore) Close() error { return s.db.Close() }

// OwnerJID returns the detected/configured account-owner JID (may be empty).
func (s *AndroidStore) OwnerJID() string { return s.ownerJID }

// probeSchema records which optional tables and columns this msgstore.db has.
func (s *AndroidStore) probeSchema() {
	s.hasMedia = tableExists(s.db, "message_media")
	s.hasQuoted = tableExists(s.db, "message_quoted")
	s.hasSystem = tableExists(s.db, "message_system")

	if s.hasMedia {
		switch {
		case columnExists(s.db, "message_media", "media_duration"):
			s.mediaDurExpr = "mm.media_duration"
		case columnExists(s.db, "message_media", "duration"):
			s.mediaDurExpr = "mm.duration"
		}
	}
	s.chatSubject = columnExists(s.db, "chat", "subject")
	for _, col := range []string{"unseen_message_count", "unseen_count", "unread_count"} {
		if columnExists(s.db, "chat", col) {
			s.chatUnread = col
			break
		}
	}
}

// loadJIDs loads the jid table into jid._id -> canonical JID. It runs in three
// phases: (1) read the raw jid rows, (2) build the @lid -> phone-number map from
// them, then (3) canonicalize every row using that map so @lid identities are
// resolved to phone-number JIDs (matching the live whatsmeow sync).
func (s *AndroidStore) loadJIDs() error {
	// Phase 1: raw jid rows (untouched), keyed by jid._id.
	raw := map[int64]string{}
	rows, err := s.db.Query(`SELECT _id, raw_string, user, server FROM jid`)
	if err != nil {
		return err
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var (
				id             int64
				rs, user, serv sql.NullString
			)
			if err := rows.Scan(&id, &rs, &user, &serv); err != nil {
				return
			}
			r := strings.TrimSpace(rs.String)
			if r == "" {
				u, sv := strings.TrimSpace(user.String), strings.TrimSpace(serv.String)
				if u != "" && sv != "" {
					r = u + "@" + sv
				}
			}
			if r != "" {
				raw[id] = r
			}
		}
	}()
	if err := rows.Err(); err != nil {
		return err
	}

	// Phase 2: build the bare-lid -> bare-phone map.
	s.buildLIDMap(raw)

	// Phase 3: canonicalize every jid row using the lid map.
	for id, r := range raw {
		if canon := normalizeUserJID(r, s.lidMap); canon != "" {
			s.jidByRow[id] = canon
		}
	}
	return nil
}

// buildLIDMap populates s.lidMap (bare @lid user -> bare phone number). The
// authoritative source in a modern msgstore.db is the jid_map table, which links
// a @lid jid row to its phone-number jid row; older/other schema variants are
// also probed. When no mapping exists, @lid identities are left unresolved.
func (s *AndroidStore) buildLIDMap(raw map[int64]string) {
	// Primary: jid_map(lid_row_id -> jid_row_id).
	if rows, err := s.db.Query(`SELECT lid_row_id, jid_row_id FROM jid_map`); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var lidRow, jidRow int64
				if err := rows.Scan(&lidRow, &jidRow); err != nil {
					return
				}
				lidRaw, phoneRaw := raw[lidRow], raw[jidRow]
				if !strings.Contains(lidRaw, "@"+serverLID) || !strings.Contains(phoneRaw, "@"+serverUser) {
					continue
				}
				if l, p := bareUser(lidRaw), bareUser(phoneRaw); l != "" && p != "" {
					s.lidMap[l] = p
				}
			}
		}()
	}

	// Fallbacks for other schema variants.
	for _, q := range []string{
		`SELECT lid_user, pn_user FROM lid_phone_number_mapping`,
		`SELECT lid, phone_number FROM lid_pn_mapping`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			continue // table absent in this schema version
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var lid, phone sql.NullString
				if err := rows.Scan(&lid, &phone); err != nil {
					return
				}
				if l, p := bareUser(lid.String), bareUser(phone.String); l != "" && p != "" {
					if _, ok := s.lidMap[l]; !ok {
						s.lidMap[l] = p
					}
				}
			}
		}()
	}
}

// loadChats builds the chat list keyed by canonical JID: chat_row_id -> JID,
// optional group subjects and unread counts, plus last-message time aggregated
// from the message table (always reliable, unlike version-specific chat columns).
func (s *AndroidStore) loadChats() error {
	cols := "c._id, c.jid_row_id"
	if s.chatSubject {
		cols += ", c.subject"
	} else {
		cols += ", NULL"
	}
	if s.chatUnread != "" {
		cols += ", c." + s.chatUnread
	} else {
		cols += ", NULL"
	}

	rows, err := s.db.Query(`SELECT ` + cols + ` FROM chat c`)
	if err != nil {
		return err
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var (
				chatRow, jidRow int64
				subject         sql.NullString
				unread          sql.NullInt64
			)
			if err := rows.Scan(&chatRow, &jidRow, &subject, &unread); err != nil {
				return
			}
			jid := s.jidByRow[jidRow]
			if !importableJID(jid) {
				continue
			}
			s.chatJID[chatRow] = jid
			c := s.ensureChat(jid)
			if c == nil {
				continue
			}
			if c.isGroup && strings.TrimSpace(subject.String) != "" {
				c.name = strings.TrimSpace(subject.String)
			}
			c.unread = int(unread.Int64)
		}
	}()

	// Aggregate the last-message time per chat from the message table.
	mrows, err := s.db.Query(`SELECT chat_row_id, MAX(timestamp) FROM message GROUP BY chat_row_id`)
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var chatRow int64
		var maxTS sql.NullInt64
		if err := mrows.Scan(&chatRow, &maxTS); err != nil {
			return err
		}
		jid := s.chatJID[chatRow]
		if jid == "" {
			continue
		}
		if c := s.chats[jid]; c != nil && maxTS.Valid {
			if t := msTimeToTime(maxTS.Int64); t.After(c.lastMessage) {
				c.lastMessage = t
			}
		}
	}
	return mrows.Err()
}

// ensureChat returns the chat for a canonical JID, creating it on first use.
func (s *AndroidStore) ensureChat(jid string) *androidChat {
	if !importableJID(jid) {
		return nil
	}
	c := s.chats[jid]
	if c == nil {
		c = &androidChat{jid: jid, isGroup: isGroupJID(jid)}
		s.chats[jid] = c
	}
	return c
}

// detectOwnerJID infers the account owner's JID from outgoing messages.
//
// Outgoing (from_me=1) messages usually leave sender_jid_row_id = 0 (the chat
// already identifies the parties), but the owner's real jid is recorded on a
// minority of them. The owner is the sender that appears across the most
// *distinct chats* — they send in every conversation, whereas an occasional
// stray sender is confined to one or two chats. Ranking by raw count instead
// would be misled by a single busy chat. Returns "" when undetectable; callers
// should prefer the explicit -me override, especially after a number change.
func (s *AndroidStore) detectOwnerJID() string {
	rows, err := s.db.Query(`
		SELECT sender_jid_row_id, COUNT(DISTINCT chat_row_id) AS dchats, COUNT(*) AS n FROM message
		WHERE from_me = 1 AND sender_jid_row_id IS NOT NULL AND sender_jid_row_id > 0
		GROUP BY sender_jid_row_id ORDER BY dchats DESC, n DESC`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var row, dchats, n int64
		if err := rows.Scan(&row, &dchats, &n); err != nil {
			return ""
		}
		if jid := s.jidByRow[row]; jid != "" {
			return jid
		}
	}
	return ""
}

// Chats returns all importable chats, most-recent first.
func (s *AndroidStore) Chats() []ChatRecord {
	out := make([]ChatRecord, 0, len(s.chats))
	for _, c := range s.chats {
		out = append(out, ChatRecord{
			JID:         c.jid,
			Name:        c.name,
			LastMessage: c.lastMessage,
			UnreadCount: c.unread,
			IsGroup:     c.isGroup,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMessage.After(out[j].LastMessage) })
	return out
}

// PushNames returns the JID -> display-name map. The Google Drive backup does
// not include the contacts database (wa.db is not backed up to Drive), so no
// per-contact display names are available; group subjects are surfaced via the
// chat name instead. Returns an empty map.
func (s *AndroidStore) PushNames() (map[string]string, error) {
	return map[string]string{}, nil
}

// IterateMessages streams messages (oldest first) matching the filter.
func (s *AndroidStore) IterateMessages(filter MessageFilter, fn func(MessageRecord) error) error {
	mediaCols := "NULL, NULL, NULL, NULL"
	mediaJoin := ""
	if s.hasMedia {
		dur := "NULL"
		if s.mediaDurExpr != "" {
			dur = s.mediaDurExpr
		}
		mediaCols = "mm.file_path, mm.file_size, mm.mime_type, " + dur
		mediaJoin = " LEFT JOIN message_media mm ON mm.message_row_id = m._id"
	}
	quotedCol, quotedJoin := "NULL", ""
	if s.hasQuoted {
		quotedCol = "q.key_id"
		quotedJoin = " LEFT JOIN message_quoted q ON q.message_row_id = m._id"
	}
	systemCol, systemJoin := "NULL", ""
	if s.hasSystem {
		systemCol = "sys.action_type"
		systemJoin = " LEFT JOIN message_system sys ON sys.message_row_id = m._id"
	}

	query := `SELECT m._id, m.key_id, m.from_me, m.timestamp, m.message_type, m.text_data,
	                 m.chat_row_id, m.sender_jid_row_id, ` +
		mediaCols + ", " + quotedCol + ", " + systemCol + `
	          FROM message m` + mediaJoin + quotedJoin + systemJoin

	var (
		where []string
		args  []any
	)
	if !filter.Since.IsZero() {
		where = append(where, "m.timestamp >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if filter.ChatJID != "" {
		rowIDs := s.chatRowsForJID(filter.ChatJID)
		if len(rowIDs) == 0 {
			return nil // unknown chat -> nothing to do
		}
		ph := make([]string, len(rowIDs))
		for i, id := range rowIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, "m.chat_row_id IN ("+strings.Join(ph, ",")+")")
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY m.timestamp ASC, m._id ASC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			pk           int64
			keyID        sql.NullString
			fromMe       sql.NullInt64
			ts           sql.NullInt64
			msgType      sql.NullInt64
			textData     sql.NullString
			chatRow      sql.NullInt64
			senderRow    sql.NullInt64
			filePath     sql.NullString
			fileSize     sql.NullInt64
			mimeType     sql.NullString
			duration     sql.NullInt64
			quotedKey    sql.NullString
			systemAction sql.NullInt64
		)
		if err := rows.Scan(&pk, &keyID, &fromMe, &ts, &msgType, &textData,
			&chatRow, &senderRow, &filePath, &fileSize, &mimeType, &duration,
			&quotedKey, &systemAction); err != nil {
			return err
		}

		chatJID := s.chatJID[chatRow.Int64]
		if !importableJID(chatJID) {
			continue
		}

		label := androidMessageTypeLabel(msgType.Int64, mimeType.String, filePath.String)
		isSystem := systemAction.Valid || androidIsSystemCode(msgType.Int64) || label == "system"
		if !filter.IncludeSystem && isSystem {
			continue
		}

		isFromMe := fromMe.Int64 == 1
		var senderJID string
		switch {
		case isFromMe:
			senderJID = s.ownerJID
		case isGroupJID(chatJID):
			senderJID = s.jidByRow[senderRow.Int64]
		default:
			senderJID = chatJID
		}

		body := strings.TrimSpace(textData.String)
		if body == "" {
			body = placeholderText(label, "")
		}

		rec := MessageRecord{
			ID:        messageID(keyID.String, pk),
			ChatJID:   chatJID,
			SenderJID: senderJID,
			Text:      body,
			Timestamp: msTimeToTime(ts.Int64),
			IsFromMe:  isFromMe,
			Type:      label,
			ReplyToID: strings.TrimSpace(quotedKey.String),
		}

		if isMediaLabel(label) {
			m := &MediaRecord{FileSize: fileSize.Int64}
			if filePath.String != "" {
				m.FileName = baseName(filePath.String)
			} else {
				m.FileName = label
			}
			if mimeType.String != "" {
				m.MimeType = mimeType.String
			} else {
				m.MimeType = "application/octet-stream"
			}
			if duration.Valid && duration.Int64 > 0 {
				d := int(duration.Int64)
				m.Duration = &d
			}
			rec.Media = m
		}

		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// chatRowsForJID returns the chat_row_ids whose canonical JID matches.
func (s *AndroidStore) chatRowsForJID(jid string) []int64 {
	var ids []int64
	for row, j := range s.chatJID {
		if j == jid {
			ids = append(ids, row)
		}
	}
	return ids
}

// --- small helpers ---------------------------------------------------------

// baseName returns the final path component of a (possibly slash- or
// backslash-separated) file path.
func baseName(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// tableExists reports whether a table with the given name exists.
func tableExists(db *sql.DB, name string) bool {
	var n string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return err == nil
}

// columnExists reports whether the given table has the given column.
func columnExists(db *sql.DB, table, column string) bool {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	return false
}
