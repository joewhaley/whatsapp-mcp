package localapp

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// defaultMacPath returns the standard macOS ChatStorage location, or "" if the
// home directory cannot be determined.
func defaultMacPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Group Containers",
		"group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite")
}

// TestOpenServerLive exercises the read-only server projection against a real
// local WhatsApp installation. It is skipped when no native database is found.
func TestOpenServerLive(t *testing.T) {
	path := defaultMacPath(t)
	if path == "" {
		t.Skip("no home directory")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no local WhatsApp database at %s", path)
	}

	store, err := OpenServer(Options{ChatStoragePath: path})
	if err != nil {
		t.Fatalf("OpenServer: %v", err)
	}
	defer store.Close()

	// list_chats projection
	chats, err := store.Messages.ListChats(20)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) == 0 {
		t.Fatal("ListChats returned no chats")
	}
	for _, c := range chats {
		if c.JID == "" {
			t.Error("chat with empty JID")
		}
		if c.IsGroup && c.PushName == "" {
			t.Errorf("group %s has no name", c.JID)
		}
	}
	first := chats[0].ContactName
	if first == "" {
		first = chats[0].PushName
	}
	t.Logf("listed %d chats; first: %q (%s)", len(chats), first, chats[0].JID)

	// get_chat_messages projection for the most recent chat
	msgs, err := store.Messages.GetChatMessagesWithNames(chats[0].JID, 10, 0)
	if err != nil {
		t.Fatalf("GetChatMessagesWithNames: %v", err)
	}
	for _, m := range msgs {
		if m.Timestamp.IsZero() {
			t.Errorf("message %s has zero timestamp", m.ID)
		}
		if m.MessageType == "" {
			t.Errorf("message %s has empty type", m.ID)
		}
	}
	t.Logf("most-recent chat has %d of last messages projected", len(msgs))

	// search_messages projection (a common word likely present in any history)
	results, err := store.Messages.SearchMessagesWithNamesFiltered("the", false, "", 5)
	if err != nil {
		t.Fatalf("SearchMessagesWithNamesFiltered: %v", err)
	}
	t.Logf("search 'the' -> %d results", len(results))

	// find_chat projection
	found, err := store.Messages.SearchChatsFiltered("a", false, 5)
	if err != nil {
		t.Fatalf("SearchChatsFiltered: %v", err)
	}
	t.Logf("find_chat 'a' -> %d results", len(found))

	if store.OwnerJID() == "" {
		t.Log("owner JID not auto-detected (no self-chat?)")
	} else {
		t.Logf("owner JID: %s", store.OwnerJID())
	}
}

// TestReadOnlyRejectsWrites verifies the hard guarantee that local mode never
// writes to the native database: a write through the read-only connection must
// be rejected by SQLite.
func TestReadOnlyRejectsWrites(t *testing.T) {
	path := defaultMacPath(t)
	if path == "" {
		t.Skip("no home directory")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no local WhatsApp database at %s", path)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// A no-op UPDATE that touches no rows must still be refused up front.
	_, err = db.Exec("UPDATE ZWACHATSESSION SET ZPARTNERNAME = ZPARTNERNAME WHERE 1=0")
	if err == nil {
		t.Fatal("expected write to be rejected on a read-only connection, but it succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") &&
		!strings.Contains(strings.ToLower(err.Error()), "read only") {
		t.Fatalf("expected a read-only error, got: %v", err)
	}
	t.Logf("write correctly rejected: %v", err)
}
