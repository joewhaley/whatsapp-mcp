package localapp

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// WindowsStore reads the intermediate SQLite database produced by
// localapp/windows/extract_whatsapp_windows.py and exposes a locally installed
// WhatsApp for Windows app as a canonical message Source.
//
// Unlike macOS (a single plaintext Core Data store), the Windows app splits its
// data across an encrypted SQLCipher store (plaintext message text, recovered
// via ZAPiXDESK) and a WebView2 IndexedDB (rich metadata: direction, sender,
// type, quoted message, real message IDs). The Python pre-step joins those into
// one intermediate database; this reader maps that onto the project's canonical
// schema, reusing the same JID-normalization and placeholder conventions as the
// macOS reader so imported rows are interchangeable with the live whatsmeow sync.
type WindowsStore struct {
	db *sql.DB

	lidMap    map[string]string   // bare lid -> bare phone number
	names     map[string]string   // canonical JID -> display name
	groupSubj map[string]string   // group JID -> subject
	chats     map[string]*winChat // canonical JID -> chat
	ownerJID  string
}

type winChat struct {
	jid         string
	name        string
	isGroup     bool
	lastMessage time.Time
	unread      int
}

// WindowsOptions configures opening a WindowsStore.
type WindowsOptions struct {
	// ExportPath is the path to the intermediate SQLite produced by the
	// extract_whatsapp_windows.py pre-step (required).
	ExportPath string
	// OwnerJID overrides owner detection (bare number or full JID).
	OwnerJID string
}

// OpenWindows opens and prepares a WindowsStore from the intermediate export.
func OpenWindows(opts WindowsOptions) (*WindowsStore, error) {
	if opts.ExportPath == "" {
		return nil, fmt.Errorf("ExportPath is required")
	}
	db, err := openSQLite(opts.ExportPath, true)
	if err != nil {
		return nil, fmt.Errorf("open windows export: %w", err)
	}
	s := &WindowsStore{
		db:        db,
		lidMap:    map[string]string{},
		names:     map[string]string{},
		groupSubj: map[string]string{},
		chats:     map[string]*winChat{},
	}
	if err := s.loadLIDMap(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load lid map: %w", err)
	}
	if err := s.loadGroups(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load groups: %w", err)
	}
	if err := s.loadNames(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load names: %w", err)
	}
	if err := s.loadChats(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load chats: %w", err)
	}

	if opts.OwnerJID != "" {
		s.ownerJID = normalizeUserJID(ensureServer(opts.OwnerJID), s.lidMap)
	} else {
		s.ownerJID = normalizeUserJID(s.metaValue("owner_jid"), s.lidMap)
	}
	return s, nil
}

// Close releases the database handle.
func (s *WindowsStore) Close() error { return s.db.Close() }

// OwnerJID returns the detected/configured account-owner JID (may be empty).
func (s *WindowsStore) OwnerJID() string { return s.ownerJID }

func (s *WindowsStore) metaValue(key string) string {
	var v string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// loadLIDMap loads the bare-lid -> bare-phone map.
func (s *WindowsStore) loadLIDMap() error {
	rows, err := s.db.Query("SELECT lid, phone FROM lid_map")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lid, phone string
		if err := rows.Scan(&lid, &phone); err != nil {
			return err
		}
		lid, phone = bareUser(lid), bareUser(phone)
		if lid != "" && phone != "" {
			s.lidMap[lid] = phone
		}
	}
	return rows.Err()
}

// loadGroups loads group JID -> subject.
func (s *WindowsStore) loadGroups() error {
	rows, err := s.db.Query("SELECT jid, subject FROM groups")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jid, subject string
		if err := rows.Scan(&jid, &subject); err != nil {
			return err
		}
		if jid != "" && strings.TrimSpace(subject) != "" {
			s.groupSubj[jid] = strings.TrimSpace(subject)
		}
	}
	return rows.Err()
}

// loadNames loads contact display names keyed by canonical JID. The extractor
// stores names keyed by the bare identifier (phone number in practice).
func (s *WindowsStore) loadNames() error {
	rows, err := s.db.Query("SELECT ident, name FROM contacts")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ident, name string
		if err := rows.Scan(&ident, &name); err != nil {
			return err
		}
		name = strings.TrimSpace(name)
		if ident == "" || name == "" {
			continue
		}
		jid := normalizeUserJID(ensureServer(ident), s.lidMap)
		if jid != "" {
			s.names[jid] = name
		}
	}
	return rows.Err()
}

// loadChats builds the chat list from the chats table, group subjects and the
// distinct chats referenced by messages, all keyed by canonical JID.
func (s *WindowsStore) loadChats() error {
	ensure := func(rawJID string) (*winChat, bool) {
		jid := normalizeUserJID(rawJID, s.lidMap)
		if !importableJID(jid) {
			return nil, false
		}
		c := s.chats[jid]
		if c == nil {
			c = &winChat{jid: jid, isGroup: isGroupJID(jid)}
			if c.isGroup {
				c.name = s.groupSubj[jid]
			} else {
				c.name = s.names[jid]
			}
			s.chats[jid] = c
		}
		return c, true
	}

	// Chats table: authoritative last-message time + unread count.
	rows, err := s.db.Query("SELECT jid, last_t, unread FROM chats")
	if err != nil {
		return err
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var jid string
			var lastT, unread sql.NullInt64
			if err := rows.Scan(&jid, &lastT, &unread); err != nil {
				return
			}
			c, ok := ensure(jid)
			if !ok {
				continue
			}
			if lastT.Valid && lastT.Int64 > 0 {
				c.lastMessage = time.Unix(lastT.Int64, 0)
			}
			c.unread = int(unread.Int64)
		}
	}()

	// Fold in any chats that only appear in messages, and refine last-message.
	mrows, err := s.db.Query("SELECT chat, MAX(t) FROM messages GROUP BY chat")
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var jid string
		var maxT sql.NullInt64
		if err := mrows.Scan(&jid, &maxT); err != nil {
			return err
		}
		c, ok := ensure(jid)
		if !ok {
			continue
		}
		if maxT.Valid && maxT.Int64 > 0 {
			if t := time.Unix(maxT.Int64, 0); t.After(c.lastMessage) {
				c.lastMessage = t
			}
		}
	}
	return mrows.Err()
}

// Chats returns all importable chats, most-recent first.
func (s *WindowsStore) Chats() []ChatRecord {
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

// PushNames returns the JID -> display-name map.
func (s *WindowsStore) PushNames() (map[string]string, error) {
	out := make(map[string]string, len(s.names))
	for jid, name := range s.names {
		out[jid] = name
	}
	return out, nil
}

// IterateMessages streams messages (oldest first) matching filter.
func (s *WindowsStore) IterateMessages(filter MessageFilter, fn func(MessageRecord) error) error {
	query := `SELECT stanza_id, chat, sender, from_me, type, t, text, quoted_id,
	                 media_mimetype, media_filename, media_filesize, media_duration
	          FROM messages`
	var args []any
	if !filter.Since.IsZero() {
		query += " WHERE t >= ?"
		args = append(args, filter.Since.Unix())
	}
	query += " ORDER BY t ASC, rowid ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	emitted := 0
	for rows.Next() {
		var (
			stanza, chat, sender, waType, text, quoted sql.NullString
			fromMe                                     sql.NullInt64
			t                                          sql.NullInt64
			mimeType, fileName                         sql.NullString
			fileSize, duration                         sql.NullInt64
		)
		if err := rows.Scan(&stanza, &chat, &sender, &fromMe, &waType, &t, &text, &quoted,
			&mimeType, &fileName, &fileSize, &duration); err != nil {
			return err
		}

		if !filter.IncludeSystem && winIsSystemType(waType.String) {
			continue
		}
		chatJID := normalizeUserJID(chat.String, s.lidMap)
		if !importableJID(chatJID) {
			continue
		}
		if filter.ChatJID != "" && chatJID != filter.ChatJID {
			continue
		}

		label := winMessageTypeLabel(waType.String)
		isFromMe := fromMe.Int64 == 1

		var senderJID string
		switch {
		case isFromMe:
			senderJID = s.ownerJID
		case isGroupJID(chatJID):
			senderJID = normalizeUserJID(sender.String, s.lidMap)
		default:
			senderJID = chatJID
		}

		body := strings.TrimSpace(text.String)
		if body == "" {
			body = placeholderText(label, "")
		}

		rec := MessageRecord{
			ID:        messageID(stanza.String, 0),
			ChatJID:   chatJID,
			SenderJID: senderJID,
			Text:      body,
			Timestamp: time.Unix(t.Int64, 0),
			IsFromMe:  isFromMe,
			Type:      label,
			ReplyToID: strings.TrimSpace(quoted.String),
		}

		if isMediaLabel(label) {
			m := &MediaRecord{FileSize: fileSize.Int64}
			if fileName.Valid && fileName.String != "" {
				m.FileName = fileName.String
			} else {
				m.FileName = label
			}
			if mimeType.Valid && mimeType.String != "" {
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
		emitted++
		if filter.Limit > 0 && emitted >= filter.Limit {
			break
		}
	}
	return rows.Err()
}

// isMediaLabel reports whether a canonical label denotes an attachment that
// warrants a media_metadata record.
func isMediaLabel(label string) bool {
	switch label {
	case "image", "video", "audio", "document", "sticker", "gif":
		return true
	default:
		return false
	}
}
