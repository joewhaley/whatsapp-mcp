package localapp

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// buildMsgstore writes a minimal modern Android msgstore.db (the shape a
// decrypted crypt15 Google Drive backup has) and returns its path.
func buildMsgstore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "msgstore.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE jid (_id INTEGER PRIMARY KEY, raw_string TEXT, user TEXT, server TEXT);
		CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER, subject TEXT, unseen_message_count INTEGER);
		CREATE TABLE message (_id INTEGER PRIMARY KEY, key_id TEXT, from_me INTEGER, timestamp INTEGER,
			message_type INTEGER, text_data TEXT, chat_row_id INTEGER, sender_jid_row_id INTEGER);
		CREATE TABLE message_media (message_row_id INTEGER, file_path TEXT, file_size INTEGER,
			mime_type TEXT, media_duration INTEGER);
		CREATE TABLE message_quoted (message_row_id INTEGER, key_id TEXT);
		CREATE TABLE message_system (message_row_id INTEGER, action_type INTEGER);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	// jids: 1=owner, 2=Pablo (1:1), 3=group, 4=group participant.
	exec(`INSERT INTO jid VALUES
		(1,'16502833196@s.whatsapp.net','16502833196','s.whatsapp.net'),
		(2,'15551112222@s.whatsapp.net','15551112222','s.whatsapp.net'),
		(3,'120363111111111111@g.us','120363111111111111','g.us'),
		(4,'15553334444@s.whatsapp.net','15553334444','s.whatsapp.net')`)

	// chats: 10=1:1 with Pablo, 11=group "Test Group".
	exec(`INSERT INTO chat VALUES (10,2,NULL,0),(11,3,'Test Group',2)`)

	// messages (timestamps in ms).
	type m struct {
		id     int64
		key    string
		fromMe int
		ts     int64
		typ    int
		text   string
		chat   int64
		sender int64
	}
	msgs := []m{
		{1, "AAA", 0, 1700000001000, 0, "hi from pablo", 10, 0},
		{2, "BBB", 1, 1700000002000, 0, "hello back", 10, 1},
		{3, "CCC", 0, 1700000003000, 0, "group msg", 11, 4},
		{4, "DDD", 0, 1700000004000, 1, "", 11, 4},         // image, no caption
		{5, "EEE", 0, 1700000005000, 7, "", 11, 0},         // system event
		{6, "FFF", 1, 1700000006000, 0, "replying", 10, 1}, // reply to AAA
		{7, "GGG", 0, 1700000007000, 2, "", 10, 0},         // audio with duration
	}
	for _, x := range msgs {
		exec(`INSERT INTO message VALUES (?,?,?,?,?,?,?,?)`,
			x.id, x.key, x.fromMe, x.ts, x.typ, x.text, x.chat, x.sender)
	}

	exec(`INSERT INTO message_media VALUES (4,'Media/WhatsApp Images/photo.jpg',12345,'image/jpeg',NULL)`)
	exec(`INSERT INTO message_media VALUES (7,'Media/WhatsApp Voice Notes/ptt.opus',555,'audio/ogg',7)`)
	exec(`INSERT INTO message_quoted VALUES (6,'AAA')`)
	exec(`INSERT INTO message_system VALUES (5,1)`)

	return path
}

func collectAndroid(t *testing.T, s *AndroidStore, f MessageFilter) []MessageRecord {
	t.Helper()
	var out []MessageRecord
	if err := s.IterateMessages(f, func(r MessageRecord) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatalf("IterateMessages: %v", err)
	}
	return out
}

func TestAndroidStore(t *testing.T) {
	path := buildMsgstore(t)
	s, err := OpenAndroid(AndroidOptions{MsgstorePath: path})
	if err != nil {
		t.Fatalf("OpenAndroid: %v", err)
	}
	defer s.Close()

	// Owner detected from outgoing messages' sender jid.
	if s.OwnerJID() != "16502833196@s.whatsapp.net" {
		t.Errorf("OwnerJID = %q, want 16502833196@s.whatsapp.net", s.OwnerJID())
	}

	// No per-contact names from a Drive backup; group subject surfaces via chat.
	names, err := s.PushNames()
	if err != nil {
		t.Fatalf("PushNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("PushNames = %v, want empty", names)
	}

	chats := map[string]ChatRecord{}
	for _, c := range s.Chats() {
		chats[c.JID] = c
	}
	if c, ok := chats["15551112222@s.whatsapp.net"]; !ok || c.IsGroup {
		t.Errorf("1:1 chat = %+v, ok=%v", c, ok)
	}
	if c, ok := chats["120363111111111111@g.us"]; !ok || c.Name != "Test Group" || !c.IsGroup || c.UnreadCount != 2 {
		t.Errorf("group chat = %+v, ok=%v", c, ok)
	}

	// Default filter skips the system event (6 of 7 messages).
	msgs := collectAndroid(t, s, MessageFilter{})
	if len(msgs) != 6 {
		t.Fatalf("got %d messages, want 6 (system skipped)", len(msgs))
	}
	by := map[string]MessageRecord{}
	for _, m := range msgs {
		by[m.ID] = m
	}

	// Incoming 1:1: sender is the chat partner.
	if m := by["AAA"]; m.SenderJID != "15551112222@s.whatsapp.net" || m.IsFromMe ||
		m.ChatJID != "15551112222@s.whatsapp.net" || m.Type != "text" {
		t.Errorf("AAA = %+v", m)
	}
	// Outgoing 1:1: sender is the owner.
	if m := by["BBB"]; m.SenderJID != "16502833196@s.whatsapp.net" || !m.IsFromMe {
		t.Errorf("BBB = %+v", m)
	}
	// Incoming group: sender is the group participant.
	if m := by["CCC"]; m.SenderJID != "15553334444@s.whatsapp.net" || m.ChatJID != "120363111111111111@g.us" {
		t.Errorf("CCC = %+v", m)
	}
	// Image: placeholder text + media record.
	if m := by["DDD"]; m.Type != "image" || m.Text != "[Image]" || m.Media == nil ||
		m.Media.MimeType != "image/jpeg" || m.Media.FileName != "photo.jpg" || m.Media.FileSize != 12345 {
		t.Errorf("DDD = %+v media=%+v", m, m.Media)
	}
	// Audio: placeholder text + duration.
	if m := by["GGG"]; m.Type != "audio" || m.Text != "[Audio]" || m.Media == nil ||
		m.Media.Duration == nil || *m.Media.Duration != 7 {
		t.Errorf("GGG = %+v media=%+v", m, m.Media)
	}
	// Reply reference preserved.
	if m := by["FFF"]; m.ReplyToID != "AAA" {
		t.Errorf("FFF reply = %q, want AAA", m.ReplyToID)
	}

	// IncludeSystem keeps the system event.
	if all := collectAndroid(t, s, MessageFilter{IncludeSystem: true}); len(all) != 7 {
		t.Errorf("with IncludeSystem got %d, want 7", len(all))
	}

	// Chat filter restricts to the 1:1 conversation (AAA, BBB, FFF, GGG).
	only := collectAndroid(t, s, MessageFilter{ChatJID: "15551112222@s.whatsapp.net"})
	if len(only) != 4 {
		t.Errorf("chat-filtered got %d, want 4", len(only))
	}

	// OwnerJID override is honored.
	s2, err := OpenAndroid(AndroidOptions{MsgstorePath: path, OwnerJID: "19998887777"})
	if err != nil {
		t.Fatalf("OpenAndroid override: %v", err)
	}
	defer s2.Close()
	if s2.OwnerJID() != "19998887777@s.whatsapp.net" {
		t.Errorf("override OwnerJID = %q", s2.OwnerJID())
	}
}

// TestAndroidStoreMinimalSchema verifies graceful degradation when the optional
// tables/columns (media, quoted, system, subject, unread) are absent.
func TestAndroidStoreMinimalSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "msgstore.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE jid (_id INTEGER PRIMARY KEY, raw_string TEXT, user TEXT, server TEXT);
		CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER);
		CREATE TABLE message (_id INTEGER PRIMARY KEY, key_id TEXT, from_me INTEGER, timestamp INTEGER,
			message_type INTEGER, text_data TEXT, chat_row_id INTEGER, sender_jid_row_id INTEGER);
		INSERT INTO jid VALUES (1,'15551112222@s.whatsapp.net','15551112222','s.whatsapp.net');
		INSERT INTO chat VALUES (10,1);
		INSERT INTO message VALUES (1,'AAA',0,1700000001000,0,'hello',10,0);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db.Close()

	s, err := OpenAndroid(AndroidOptions{MsgstorePath: path, OwnerJID: "16502833196"})
	if err != nil {
		t.Fatalf("OpenAndroid: %v", err)
	}
	defer s.Close()

	msgs := collectAndroid(t, s, MessageFilter{})
	if len(msgs) != 1 || msgs[0].Text != "hello" || msgs[0].ChatJID != "15551112222@s.whatsapp.net" {
		t.Errorf("minimal schema messages = %+v", msgs)
	}
}

// TestAndroidOwnerDetectionByDistinctChats verifies the owner is the from-me
// sender spanning the most distinct chats, not the one with the highest raw
// count (which a single busy chat could dominate with a stray sender).
func TestAndroidOwnerDetectionByDistinctChats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "msgstore.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE jid (_id INTEGER PRIMARY KEY, raw_string TEXT, user TEXT, server TEXT);
		CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER);
		CREATE TABLE message (_id INTEGER PRIMARY KEY, key_id TEXT, from_me INTEGER, timestamp INTEGER,
			message_type INTEGER, text_data TEXT, chat_row_id INTEGER, sender_jid_row_id INTEGER);
		INSERT INTO jid VALUES
			(1,'16502833196@s.whatsapp.net','16502833196','s.whatsapp.net'),  -- owner
			(2,'15559999999@s.whatsapp.net','15559999999','s.whatsapp.net'),  -- noisy stray sender
			(3,'15551112222@s.whatsapp.net','15551112222','s.whatsapp.net'),
			(4,'15553334444@s.whatsapp.net','15553334444','s.whatsapp.net'),
			(5,'15555556666@s.whatsapp.net','15555556666','s.whatsapp.net');
		INSERT INTO chat VALUES (10,3),(11,4),(12,5),(13,2);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	// Owner (jid 1) sends across 3 distinct chats (3 messages total).
	// A stray sender (jid 2) appears on 10 from-me messages but all in ONE chat.
	exec := func(q string, a ...any) {
		if _, err := db.Exec(q, a...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO message VALUES (1,'m1',1,1700000001000,0,'a',10,1)`)
	exec(`INSERT INTO message VALUES (2,'m2',1,1700000002000,0,'b',11,1)`)
	exec(`INSERT INTO message VALUES (3,'m3',1,1700000003000,0,'c',12,1)`)
	for i := 0; i < 10; i++ {
		exec(`INSERT INTO message VALUES (?,?,1,?,0,'x',13,2)`, 100+i, "n", 1700000100000+int64(i)*1000)
	}
	db.Close()

	s, err := OpenAndroid(AndroidOptions{MsgstorePath: path})
	if err != nil {
		t.Fatalf("OpenAndroid: %v", err)
	}
	defer s.Close()
	if s.OwnerJID() != "16502833196@s.whatsapp.net" {
		t.Errorf("OwnerJID = %q, want 16502833196@s.whatsapp.net (distinct-chats winner, not raw-count)", s.OwnerJID())
	}
}

// TestAndroidStoreRejectsNonMsgstore verifies a clear error for a database that
// lacks the modern "message" table.
func TestAndroidStoreRejectsNonMsgstore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-msgstore.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (_id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db.Close()

	if _, err := OpenAndroid(AndroidOptions{MsgstorePath: path}); err == nil {
		t.Fatal("expected error for legacy/non-msgstore database, got nil")
	}
}
