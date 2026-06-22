package localapp

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides read access to a local WhatsApp desktop installation: the
// ChatStorage.sqlite Core Data store plus the optional LID.sqlite that maps
// linked-identity (@lid) JIDs back to phone numbers.
//
// A Store is prepared once (loading the small lookup tables into memory) and
// can then stream the full message history without holding it all at once.
type Store struct {
	chatDB *sql.DB
	lidDB  *sql.DB // optional; nil when no LID database is available

	mediaRoot string // directory holding the "Media/..." tree referenced by messages

	lidMap      map[string]string  // bare LID user -> bare phone number
	sessions    map[int64]*session // ZWACHATSESSION.Z_PK -> resolved chat
	groupSender map[int64]string   // ZWAGROUPMEMBER.Z_PK -> normalized sender JID
	ownerJID    string             // canonical JID of the account owner ("me")
}

type session struct {
	jid         string
	name        string
	isGroup     bool
	importable  bool
	lastMessage time.Time
	unreadCount int
}

// Options configures how a Store is opened.
type Options struct {
	// ChatStoragePath is the path to ChatStorage.sqlite (required).
	ChatStoragePath string
	// LIDPath is the path to LID.sqlite. When empty, a sibling file named
	// "LID.sqlite" next to ChatStoragePath is used if present.
	LIDPath string
	// OwnerJID overrides auto-detection of the account owner's JID. Accepts a
	// bare phone number or a full JID.
	OwnerJID string
	// ReadOnly opens the databases in read-only mode. Callers operating
	// directly on a live store should instead copy the files first.
	ReadOnly bool
}

// ChatRecord is a chat/conversation in canonical form.
type ChatRecord struct {
	JID         string
	Name        string
	LastMessage time.Time
	UnreadCount int
	IsGroup     bool
}

// MediaRecord describes a media attachment in canonical form.
type MediaRecord struct {
	FileName  string
	FileSize  int64
	MimeType  string
	Duration  *int
	LocalPath string // absolute path to the file on disk, when resolvable
}

// MessageRecord is a single message in canonical form.
type MessageRecord struct {
	ID        string
	ChatJID   string
	SenderJID string
	Text      string
	Timestamp time.Time
	IsFromMe  bool
	Type      string
	ReplyToID string
	Media     *MediaRecord
}

// Open opens and prepares a Store from the given options.
func Open(opts Options) (*Store, error) {
	if opts.ChatStoragePath == "" {
		return nil, fmt.Errorf("ChatStoragePath is required")
	}

	chatDB, err := openSQLite(opts.ChatStoragePath, opts.ReadOnly)
	if err != nil {
		return nil, fmt.Errorf("open ChatStorage: %w", err)
	}

	s := &Store{
		chatDB:    chatDB,
		mediaRoot: filepath.Dir(opts.ChatStoragePath),
		lidMap:    map[string]string{},
	}

	// Locate the LID database (explicit path, or sibling of ChatStorage).
	lidPath := opts.LIDPath
	if lidPath == "" {
		sibling := filepath.Join(filepath.Dir(opts.ChatStoragePath), "LID.sqlite")
		if fileExists(sibling) {
			lidPath = sibling
		}
	}
	if lidPath != "" {
		if s.lidDB, err = openSQLite(lidPath, opts.ReadOnly); err != nil {
			chatDB.Close()
			return nil, fmt.Errorf("open LID database: %w", err)
		}
	}

	if err := s.prepare(); err != nil {
		s.Close()
		return nil, err
	}

	// Resolve the owner JID: explicit override wins, else self-chat detection.
	if opts.OwnerJID != "" {
		s.ownerJID = normalizeUserJID(ensureServer(opts.OwnerJID), s.lidMap)
	} else {
		s.ownerJID = s.detectOwnerJID()
	}

	return s, nil
}

// openSQLite opens a SQLite database with a busy timeout, optionally read-only.
func openSQLite(path string, readOnly bool) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)"
	if readOnly {
		dsn += "&mode=ro"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close releases all database handles.
func (s *Store) Close() error {
	var first error
	if s.chatDB != nil {
		if err := s.chatDB.Close(); err != nil {
			first = err
		}
	}
	if s.lidDB != nil {
		if err := s.lidDB.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// OwnerJID returns the detected (or configured) account-owner JID. It may be
// empty when detection failed and no override was supplied.
func (s *Store) OwnerJID() string { return s.ownerJID }

// prepare loads the in-memory lookup tables used to canonicalize messages.
func (s *Store) prepare() error {
	if err := s.loadLIDMap(); err != nil {
		return fmt.Errorf("load LID map: %w", err)
	}
	if err := s.loadSessions(); err != nil {
		return fmt.Errorf("load chat sessions: %w", err)
	}
	if err := s.loadGroupMembers(); err != nil {
		return fmt.Errorf("load group members: %w", err)
	}
	return nil
}

// loadLIDMap builds the LID -> phone-number map from the LID database. Newer
// stores keep the mapping in ZWAZACCOUNT (ZIDENTIFIER/ZPHONENUMBER); older ones
// use ZWAPHONENUMBERLIDPAIR. Both are read when present.
func (s *Store) loadLIDMap() error {
	if s.lidDB == nil {
		return nil
	}

	queries := []string{
		`SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR WHERE ZPHONENUMBER IS NOT NULL AND ZPHONENUMBER <> ''`,
		`SELECT ZIDENTIFIER, ZPHONENUMBER FROM ZWAZACCOUNT WHERE ZIDENTIFIER LIKE '%@lid' AND ZPHONENUMBER IS NOT NULL AND ZPHONENUMBER <> ''`,
	}
	for _, q := range queries {
		rows, err := s.lidDB.Query(q)
		if err != nil {
			// Table may not exist in this schema version; skip it.
			continue
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var lid, phone string
				if err := rows.Scan(&lid, &phone); err != nil {
					continue
				}
				lidUser := bareUser(lid)
				phone = bareUser(phone)
				if lidUser != "" && phone != "" {
					s.lidMap[lidUser] = phone
				}
			}
		}()
	}
	return nil
}

// loadSessions loads every chat session and resolves it to a canonical JID.
func (s *Store) loadSessions() error {
	rows, err := s.chatDB.Query(`
		SELECT Z_PK, ZSESSIONTYPE, ZCONTACTJID, ZPARTNERNAME, ZLASTMESSAGEDATE, ZUNREADCOUNT
		FROM ZWACHATSESSION`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.sessions = map[int64]*session{}
	for rows.Next() {
		var (
			pk          int64
			sessionType sql.NullInt64
			contactJID  sql.NullString
			partnerName sql.NullString
			lastMsgDate sql.NullFloat64
			unread      sql.NullInt64
		)
		if err := rows.Scan(&pk, &sessionType, &contactJID, &partnerName, &lastMsgDate, &unread); err != nil {
			return err
		}

		jid := normalizeUserJID(contactJID.String, s.lidMap)
		s.sessions[pk] = &session{
			jid:         jid,
			name:        strings.TrimSpace(partnerName.String),
			isGroup:     isGroupJID(jid),
			importable:  importableJID(jid),
			lastMessage: nsDateToTime(lastMsgDate.Float64),
			unreadCount: int(unread.Int64),
		}
	}
	return rows.Err()
}

// loadGroupMembers maps each group-member row to its normalized sender JID.
func (s *Store) loadGroupMembers() error {
	rows, err := s.chatDB.Query(`SELECT Z_PK, ZMEMBERJID FROM ZWAGROUPMEMBER`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.groupSender = map[int64]string{}
	for rows.Next() {
		var pk int64
		var memberJID sql.NullString
		if err := rows.Scan(&pk, &memberJID); err != nil {
			return err
		}
		if memberJID.Valid && memberJID.String != "" {
			s.groupSender[pk] = normalizeUserJID(memberJID.String, s.lidMap)
		}
	}
	return rows.Err()
}

// detectOwnerJID finds the "Message yourself" chat (partner name "You") and
// returns its canonical JID. Returns "" when it cannot be determined.
func (s *Store) detectOwnerJID() string {
	for _, sess := range s.sessions {
		if sess.isGroup {
			continue
		}
		if isSelfName(sess.name) {
			return sess.jid
		}
	}
	return ""
}

// Chats returns all importable chat sessions ordered by most recent activity.
func (s *Store) Chats() []ChatRecord {
	var out []ChatRecord
	for _, sess := range s.sessions {
		if !sess.importable {
			continue
		}
		out = append(out, ChatRecord{
			JID:         sess.jid,
			Name:        sess.name,
			LastMessage: sess.lastMessage,
			UnreadCount: sess.unreadCount,
			IsGroup:     sess.isGroup,
		})
	}
	return out
}

// PushNames returns the JID -> WhatsApp display-name map from the store.
func (s *Store) PushNames() (map[string]string, error) {
	rows, err := s.chatDB.Query(`
		SELECT ZJID, ZPUSHNAME FROM ZWAPROFILEPUSHNAME
		WHERE ZPUSHNAME IS NOT NULL AND ZPUSHNAME <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}
		canon := normalizeUserJID(jid, s.lidMap)
		if canon != "" {
			out[canon] = strings.TrimSpace(name)
		}
	}
	return out, rows.Err()
}

// MessageFilter narrows the set of messages returned by IterateMessages.
type MessageFilter struct {
	// Since, when non-zero, restricts to messages at or after this time.
	Since time.Time
	// ChatJID, when non-empty, restricts to a single canonical chat JID.
	ChatJID string
	// Limit, when > 0, caps the number of messages returned.
	Limit int
	// IncludeSystem includes group/system event messages (skipped by default).
	IncludeSystem bool
}

// IterateMessages streams messages (oldest first) matching the filter, invoking
// fn for each. Messages whose chat session is not importable are skipped.
func (s *Store) IterateMessages(filter MessageFilter, fn func(MessageRecord) error) error {
	var (
		where []string
		args  []any
	)
	if !filter.Since.IsZero() {
		where = append(where, "m.ZMESSAGEDATE >= ?")
		args = append(args, float64(filter.Since.Unix()-coreDataEpochOffset))
	}
	if filter.ChatJID != "" {
		pks := s.sessionPKsForJID(filter.ChatJID)
		if len(pks) == 0 {
			return nil // unknown chat -> nothing to do
		}
		placeholders := make([]string, len(pks))
		for i, pk := range pks {
			placeholders[i] = "?"
			args = append(args, pk)
		}
		where = append(where, "m.ZCHATSESSION IN ("+strings.Join(placeholders, ",")+")")
	}

	query := `
		SELECT m.ZSTANZAID, m.ZISFROMME, m.ZMESSAGETYPE, m.ZMESSAGEDATE, m.ZTEXT,
		       m.ZCHATSESSION, m.ZGROUPMEMBER, m.Z_PK,
		       p.ZSTANZAID AS parent_stanza,
		       mi.ZMEDIALOCALPATH, mi.ZFILESIZE, mi.ZMOVIEDURATION, mi.ZTITLE, mi.ZVCARDNAME
		FROM ZWAMESSAGE m
		LEFT JOIN ZWAMESSAGE p ON m.ZPARENTMESSAGE = p.Z_PK
		LEFT JOIN ZWAMEDIAITEM mi ON m.ZMEDIAITEM = mi.Z_PK`
	if len(where) > 0 {
		query += "\n\t\tWHERE " + strings.Join(where, " AND ")
	}
	query += "\n\t\tORDER BY m.ZMESSAGEDATE ASC, m.Z_PK ASC"
	if filter.Limit > 0 {
		query += fmt.Sprintf("\n\t\tLIMIT %d", filter.Limit)
	}

	rows, err := s.chatDB.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			stanza       sql.NullString
			isFromMe     sql.NullInt64
			msgType      sql.NullInt64
			msgDate      sql.NullFloat64
			text         sql.NullString
			chatSession  sql.NullInt64
			groupMember  sql.NullInt64
			pk           int64
			parentStanza sql.NullString
			mediaPath    sql.NullString
			fileSize     sql.NullInt64
			duration     sql.NullInt64
			title        sql.NullString
			vcardName    sql.NullString
		)
		if err := rows.Scan(&stanza, &isFromMe, &msgType, &msgDate, &text,
			&chatSession, &groupMember, &pk, &parentStanza,
			&mediaPath, &fileSize, &duration, &title, &vcardName); err != nil {
			return err
		}

		sess := s.sessions[chatSession.Int64]
		if sess == nil || !sess.importable {
			continue
		}
		if !filter.IncludeSystem && isSystemType(msgType.Int64) {
			continue
		}

		label := messageTypeLabel(msgType.Int64, mediaPath.String)

		rec := MessageRecord{
			ID:        messageID(stanza.String, pk),
			ChatJID:   sess.jid,
			SenderJID: s.resolveSender(isFromMe.Int64 == 1, sess, groupMember),
			Text:      strings.TrimSpace(text.String),
			Timestamp: nsDateToTime(msgDate.Float64),
			IsFromMe:  isFromMe.Int64 == 1,
			Type:      label,
			ReplyToID: strings.TrimSpace(parentStanza.String),
		}

		if mediaPath.Valid && mediaPath.String != "" || fileSize.Int64 > 0 {
			rec.Media = s.buildMedia(label, mediaPath, fileSize, duration, title, vcardName)
		}

		// Fall back to a readable placeholder for media/system messages with
		// no caption, mirroring the live sync's convention.
		if rec.Text == "" {
			rec.Text = placeholderText(label, vcardName.String)
		}

		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// resolveSender determines the canonical sender JID for a message.
func (s *Store) resolveSender(isFromMe bool, sess *session, groupMember sql.NullInt64) string {
	if isFromMe {
		return s.ownerJID
	}
	if sess.isGroup {
		if groupMember.Valid {
			if jid := s.groupSender[groupMember.Int64]; jid != "" {
				return jid
			}
		}
		return "" // unknown group participant
	}
	return sess.jid
}

// buildMedia constructs a MediaRecord from the joined media-item columns.
func (s *Store) buildMedia(label string, path sql.NullString, fileSize, duration sql.NullInt64, title, vcardName sql.NullString) *MediaRecord {
	m := &MediaRecord{FileSize: fileSize.Int64}

	if path.Valid && path.String != "" {
		m.FileName = filepath.Base(path.String)
		m.MimeType = mimeFromExt(path.String)
		m.LocalPath = filepath.Join(s.mediaRoot, path.String)
	}
	if m.FileName == "" {
		if title.Valid && title.String != "" {
			m.FileName = title.String
		} else if vcardName.Valid && vcardName.String != "" {
			m.FileName = vcardName.String
		} else {
			m.FileName = label
		}
	}
	if m.MimeType == "" {
		m.MimeType = "application/octet-stream"
	}
	if duration.Valid && duration.Int64 > 0 {
		d := int(duration.Int64)
		m.Duration = &d
	}
	return m
}

// sessionPKsForJID returns the session primary keys whose resolved JID matches.
func (s *Store) sessionPKsForJID(jid string) []int64 {
	var pks []int64
	for pk, sess := range s.sessions {
		if sess.jid == jid {
			pks = append(pks, pk)
		}
	}
	return pks
}

// --- small helpers ---------------------------------------------------------

// bareUser returns the user portion of a JID-like string ("123@lid" -> "123"),
// stripping any device/agent suffix.
func bareUser(raw string) string {
	raw = strings.TrimSpace(raw)
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		raw = raw[:at]
	}
	if i := strings.IndexAny(raw, ":."); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// ensureServer appends "@s.whatsapp.net" to a bare phone number.
func ensureServer(jid string) string {
	if strings.Contains(jid, "@") {
		return jid
	}
	return jid + "@" + serverUser
}

// importableJID reports whether a chat with this JID should be imported. Status
// and broadcast pseudo-chats are excluded; empty JIDs are excluded.
func importableJID(jid string) bool {
	if jid == "" {
		return false
	}
	at := strings.LastIndex(jid, "@")
	if at < 0 {
		return false
	}
	server := jid[at+1:]
	if server == serverStatus || server == serverBroadcast {
		return false
	}
	if strings.HasSuffix(server, ".status") { // e.g. "...@lid.status"
		return false
	}
	return true
}

// isSelfName reports whether a chat partner name marks the "Message yourself"
// chat. WhatsApp prefixes it with a Unicode bidi mark.
func isSelfName(name string) bool {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '‎', '‏', '‪', '‫', '‬':
			return -1
		}
		return r
	}, name)
	return strings.EqualFold(strings.TrimSpace(cleaned), "You")
}

// messageID returns the stanza ID, or a synthetic fallback when it is missing.
func messageID(stanza string, pk int64) string {
	if s := strings.TrimSpace(stanza); s != "" {
		return s
	}
	return fmt.Sprintf("mac-%d", pk)
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}
