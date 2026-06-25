package storage

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newMessagesTestDB creates an in-memory DB with just the messages table needed
// by SaveBulkFillGaps (no FK to chats, so rows can be inserted standalone).
func newMessagesTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		chat_jid TEXT NOT NULL,
		sender_jid TEXT NOT NULL,
		text TEXT,
		timestamp INTEGER NOT NULL,
		is_from_me BOOLEAN NOT NULL,
		message_type TEXT NOT NULL,
		reply_to_id TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func textOf(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var txt sql.NullString
	if err := db.QueryRow("SELECT text FROM messages WHERE id = ?", id).Scan(&txt); err != nil {
		t.Fatalf("select %s: %v", id, err)
	}
	return txt.String
}

func TestSaveBulkFillGaps(t *testing.T) {
	db := newMessagesTestDB(t)
	defer db.Close()
	s := NewMessageStore(db)

	ts := time.Unix(1700000000, 0)
	// Seed the destination as if the live sync had written it.
	seed := []Message{
		{ID: "real", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "real body", Timestamp: ts, MessageType: "text"},
		{ID: "proto", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "[Protocol]", Timestamp: ts, MessageType: "text"},
		{ID: "img", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "[Image]", Timestamp: ts, MessageType: "image"},
	}
	if err := s.SaveBulk(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Import (fill-gaps) overlapping + new rows.
	incoming := []Message{
		{ID: "real", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "DIFFERENT body", Timestamp: ts, MessageType: "text"},          // must NOT overwrite
		{ID: "proto", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "the real protocol text", Timestamp: ts, MessageType: "text"}, // upgrade
		{ID: "img", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "[Image]", Timestamp: ts, MessageType: "image"},                 // placeholder->placeholder: no change
		{ID: "imgcap", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "[Image]", Timestamp: ts, MessageType: "image"},
		{ID: "new", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "brand new", Timestamp: ts, MessageType: "text"}, // gap insert
	}
	if err := s.SaveBulkFillGaps(incoming); err != nil {
		t.Fatalf("fillgaps: %v", err)
	}

	if got := textOf(t, db, "real"); got != "real body" {
		t.Errorf("real overwritten: got %q, want unchanged %q", got, "real body")
	}
	if got := textOf(t, db, "proto"); got != "the real protocol text" {
		t.Errorf("proto not upgraded: got %q", got)
	}
	if got := textOf(t, db, "img"); got != "[Image]" {
		t.Errorf("img placeholder changed by placeholder: got %q", got)
	}
	if got := textOf(t, db, "new"); got != "brand new" {
		t.Errorf("new gap not inserted: got %q", got)
	}

	// Now upgrade the captionless image with a real caption.
	if err := s.SaveBulkFillGaps([]Message{
		{ID: "imgcap", ChatJID: "c@g.us", SenderJID: "a@s.whatsapp.net", Text: "look at this", Timestamp: ts, MessageType: "image"},
	}); err != nil {
		t.Fatalf("fillgaps caption: %v", err)
	}
	if got := textOf(t, db, "imgcap"); got != "look at this" {
		t.Errorf("img caption not upgraded: got %q", got)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Errorf("row count = %d, want 5", count)
	}
}
