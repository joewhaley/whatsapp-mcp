package localapp

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// buildWinExport writes a minimal intermediate export database (the shape the
// extract_whatsapp_windows.py pre-step produces) and returns its path.
func buildWinExport(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wa-windows-export.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE messages (stanza_id TEXT, row_id INTEGER, chat TEXT, sender TEXT,
			from_me INTEGER, type TEXT, t INTEGER, text TEXT, quoted_id TEXT,
			media_mimetype TEXT, media_filename TEXT, media_filesize INTEGER, media_duration INTEGER);
		CREATE TABLE chats   (jid TEXT PRIMARY KEY, last_t INTEGER, unread INTEGER, is_group INTEGER);
		CREATE TABLE groups  (jid TEXT PRIMARY KEY, subject TEXT);
		CREATE TABLE contacts(ident TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE lid_map (lid TEXT PRIMARY KEY, phone TEXT);
		CREATE TABLE meta    (key TEXT PRIMARY KEY, value TEXT);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO lid_map VALUES ('100304817270818','17348348224'),('200000000000001','15551112222')`)
	exec(`INSERT INTO contacts VALUES ('17348348224','Yuyan'),('16502833196','Me'),('15551112222','Pablo')`)
	exec(`INSERT INTO groups VALUES ('120363111111111111@g.us','Test Group')`)
	exec(`INSERT INTO meta VALUES ('owner_jid','16502833196@c.us')`)
	exec(`INSERT INTO chats VALUES ('100304817270818@lid',1700000100,2,0),('120363111111111111@g.us',1700000300,0,1)`)

	rows := [][]any{
		{"AAA", 1, "100304817270818@lid", "", 0, "chat", 1700000001, "hi from yuyan", "", nil, nil, nil, nil},
		{"BBB", 2, "100304817270818@lid", "", 1, "chat", 1700000002, "hello back", "", nil, nil, nil, nil},
		{"CCC", 3, "120363111111111111@g.us", "200000000000001@lid", 0, "chat", 1700000003, "group msg", "", nil, nil, nil, nil},
		{"DDD", 4, "120363111111111111@g.us", "200000000000001@lid", 0, "image", 1700000004, "", "", "image/jpeg", "photo.jpg", 12345, nil},
		{"EEE", 5, "120363111111111111@g.us", "", 0, "gp2", 1700000005, "", "", nil, nil, nil, nil},
		{"FFF", 6, "100304817270818@lid", "", 0, "chat", 1700000006, "replying", "AAA", nil, nil, nil, nil},
	}
	for _, r := range rows {
		exec(`INSERT INTO messages VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, r...)
	}
	return path
}

func collect(t *testing.T, s *WindowsStore, f MessageFilter) []MessageRecord {
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

func TestWindowsStore(t *testing.T) {
	path := buildWinExport(t)
	s, err := OpenWindows(WindowsOptions{ExportPath: path})
	if err != nil {
		t.Fatalf("OpenWindows: %v", err)
	}
	defer s.Close()

	if s.OwnerJID() != "16502833196@s.whatsapp.net" {
		t.Errorf("OwnerJID = %q, want 16502833196@s.whatsapp.net", s.OwnerJID())
	}

	// Push names keyed by canonical JID.
	names, err := s.PushNames()
	if err != nil {
		t.Fatalf("PushNames: %v", err)
	}
	if names["17348348224@s.whatsapp.net"] != "Yuyan" {
		t.Errorf("push name for Yuyan = %q", names["17348348224@s.whatsapp.net"])
	}

	// Chats: lid chat resolves to phone + contact name; group keeps its subject.
	chats := map[string]ChatRecord{}
	for _, c := range s.Chats() {
		chats[c.JID] = c
	}
	if c, ok := chats["17348348224@s.whatsapp.net"]; !ok || c.Name != "Yuyan" || c.IsGroup {
		t.Errorf("1:1 chat = %+v, ok=%v", c, ok)
	}
	if c, ok := chats["120363111111111111@g.us"]; !ok || c.Name != "Test Group" || !c.IsGroup {
		t.Errorf("group chat = %+v, ok=%v", c, ok)
	}

	// Default filter skips the gp2 system message (5 of 6 messages).
	msgs := collect(t, s, MessageFilter{})
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5 (gp2 skipped)", len(msgs))
	}
	by := map[string]MessageRecord{}
	for _, m := range msgs {
		by[m.ID] = m
	}

	// Incoming 1:1 (lid): sender is the resolved chat partner.
	if m := by["AAA"]; m.SenderJID != "17348348224@s.whatsapp.net" || m.IsFromMe || m.ChatJID != "17348348224@s.whatsapp.net" || m.Type != "text" {
		t.Errorf("AAA = %+v", m)
	}
	// Outgoing 1:1: sender is the owner.
	if m := by["BBB"]; m.SenderJID != "16502833196@s.whatsapp.net" || !m.IsFromMe {
		t.Errorf("BBB = %+v", m)
	}
	// Incoming group: sender = author lid resolved to phone.
	if m := by["CCC"]; m.SenderJID != "15551112222@s.whatsapp.net" || m.ChatJID != "120363111111111111@g.us" {
		t.Errorf("CCC = %+v", m)
	}
	// Media message: placeholder text + media record.
	if m := by["DDD"]; m.Type != "image" || m.Text != "[Image]" || m.Media == nil ||
		m.Media.MimeType != "image/jpeg" || m.Media.FileName != "photo.jpg" || m.Media.FileSize != 12345 {
		t.Errorf("DDD = %+v media=%+v", m, m.Media)
	}
	// Reply reference preserved.
	if m := by["FFF"]; m.ReplyToID != "AAA" {
		t.Errorf("FFF reply = %q, want AAA", m.ReplyToID)
	}

	// IncludeSystem keeps the gp2 message.
	if all := collect(t, s, MessageFilter{IncludeSystem: true}); len(all) != 6 {
		t.Errorf("with IncludeSystem got %d, want 6", len(all))
	}

	// Chat filter restricts to the 1:1 conversation (AAA, BBB, FFF).
	only := collect(t, s, MessageFilter{ChatJID: "17348348224@s.whatsapp.net"})
	if len(only) != 3 {
		t.Errorf("chat-filtered got %d, want 3", len(only))
	}

	// OwnerJID override is honored.
	s2, err := OpenWindows(WindowsOptions{ExportPath: path, OwnerJID: "19998887777"})
	if err != nil {
		t.Fatalf("OpenWindows override: %v", err)
	}
	defer s2.Close()
	if s2.OwnerJID() != "19998887777@s.whatsapp.net" {
		t.Errorf("override OwnerJID = %q", s2.OwnerJID())
	}
}
