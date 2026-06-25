// localimport imports chats and messages from a locally installed WhatsApp
// desktop (macOS) application directly into this project's messages database.
//
// The macOS WhatsApp app stores its history in a Core Data SQLite database
// (ChatStorage.sqlite) inside its shared group container. That store is often
// far more complete than what the whatsmeow history sync can retrieve, so this
// tool lets the local app act as a source of messages.
//
// It reads ChatStorage.sqlite (and LID.sqlite, which maps @lid identities back
// to phone numbers), converts everything into the canonical schema, and upserts
// it into messages.db. It is idempotent and safe to run repeatedly alongside the
// live sync — messages are keyed by their WhatsApp IDs.
//
// Usage:
//
//	go run ./cmd/localimport [flags]
//
// Examples:
//
//	# Dry run against the default macOS location (no writes)
//	go run ./cmd/localimport --dry-run
//
//	# Import everything into the default data/db/messages.db
//	go run ./cmd/localimport
//
//	# Import a single chat since a date
//	go run ./cmd/localimport --chat 16502833196@s.whatsapp.net --since 2025-01-01
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"whatsapp-mcp/localapp"
	"whatsapp-mcp/paths"
	"whatsapp-mcp/storage"

	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		srcPath       = flag.String("src", defaultChatStoragePath(), "path to the WhatsApp app's ChatStorage.sqlite")
		lidPath       = flag.String("lid", "", "path to LID.sqlite (default: sibling of --src, if present)")
		dbPath        = flag.String("db", paths.MessagesDBPath, "path to the destination messages.db")
		meJID         = flag.String("me", "", "your own JID or phone number (default: auto-detect)")
		sinceStr      = flag.String("since", "", "only import messages on/after this date (YYYY-MM-DD or RFC3339)")
		chatJID       = flag.String("chat", "", "only import a single chat (canonical JID, e.g. 1555...@s.whatsapp.net)")
		limit         = flag.Int("limit", 0, "maximum number of messages to import (0 = no limit)")
		includeSystem = flag.Bool("include-system", false, "include group/system event messages")
		dryRun        = flag.Bool("dry-run", false, "read and report counts without writing")
		noCopy        = flag.Bool("no-copy", false, "read the source databases in place instead of copying them to a temp dir first")
		noOverwrite   = flag.Bool("no-overwrite", false, "only insert messages the DB lacks (and upgrade [Protocol]-style placeholders); never overwrite existing rows")
		platform      = flag.String("platform", "auto", "source platform: auto|macos|windows (auto = this OS)")
		exportPath    = flag.String("export", "", "windows: path to the extract_whatsapp_windows.py intermediate SQLite")
	)
	flag.Parse()

	var since time.Time
	if *sinceStr != "" {
		t, err := parseDate(*sinceStr)
		if err != nil {
			return err
		}
		since = t
	}
	filter := localapp.MessageFilter{
		Since:         since,
		ChatJID:       *chatJID,
		Limit:         *limit,
		IncludeSystem: *includeSystem,
	}

	// Windows stores its history very differently (encrypted SQLCipher + WebView2
	// IndexedDB) and is imported from a pre-extracted intermediate database.
	if resolvePlatform(*platform) == "windows" {
		return runWindows(*exportPath, *dbPath, *meJID, filter, *dryRun, *noOverwrite)
	}

	if *srcPath == "" {
		return fmt.Errorf("--src is required (could not determine a default path)")
	}
	if _, err := os.Stat(*srcPath); err != nil {
		return fmt.Errorf("ChatStorage not found at %q: %w", *srcPath, err)
	}

	// By default, copy the (possibly live) source databases to a temp dir so we
	// never read a store the WhatsApp app is actively writing to.
	chatStorage := *srcPath
	lidStorage := *lidPath
	if !*noCopy {
		tmpDir, err := os.MkdirTemp("", "wa-localimport-*")
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		chatStorage, err = copyDBFamily(*srcPath, tmpDir)
		if err != nil {
			return fmt.Errorf("copy ChatStorage: %w", err)
		}
		srcLID := *lidPath
		if srcLID == "" {
			sibling := filepath.Join(filepath.Dir(*srcPath), "LID.sqlite")
			if _, err := os.Stat(sibling); err == nil {
				srcLID = sibling
			}
		}
		if srcLID != "" {
			lidStorage, err = copyDBFamily(srcLID, tmpDir)
			if err != nil {
				return fmt.Errorf("copy LID database: %w", err)
			}
		}
	}

	// Open the destination DB and ensure the schema is present.
	dest, err := openDestDB(*dbPath)
	if err != nil {
		return err
	}
	defer dest.Close()

	// Determine the owner JID: explicit flag, else infer from already-synced
	// messages in the destination, else let the store detect the self-chat.
	owner := *meJID
	if owner == "" {
		owner = detectOwnerFromDest(dest)
	}

	src, err := localapp.Open(localapp.Options{
		ChatStoragePath: chatStorage,
		LIDPath:         lidStorage,
		OwnerJID:        owner,
	})
	if err != nil {
		return err
	}
	defer src.Close()

	return doImport(src, src.OwnerJID(), *srcPath, *dbPath, dest, filter, *dryRun, *noOverwrite)
}

// resolvePlatform maps the --platform flag (or "auto") to "windows" or "macos".
func resolvePlatform(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "windows", "win":
		return "windows"
	case "macos", "mac", "darwin":
		return "macos"
	default:
		if runtime.GOOS == "windows" {
			return "windows"
		}
		return "macos"
	}
}

// runWindows imports from the intermediate SQLite produced by the Windows
// extract_whatsapp_windows.py pre-step (the native app's data is encrypted and
// split across SQLCipher + WebView2 IndexedDB, so it can't be read in place).
func runWindows(exportPath, dbPath, meJID string, filter localapp.MessageFilter, dryRun, noOverwrite bool) error {
	if exportPath == "" {
		return fmt.Errorf("windows import requires -export <intermediate.db>; " +
			"generate it with localapp/windows/extract_whatsapp_windows.py (see localapp/windows/README.md)")
	}
	if _, err := os.Stat(exportPath); err != nil {
		return fmt.Errorf("windows export not found at %q: %w", exportPath, err)
	}
	dest, err := openDestDB(dbPath)
	if err != nil {
		return err
	}
	defer dest.Close()

	owner := meJID
	if owner == "" {
		owner = detectOwnerFromDest(dest)
	}
	src, err := localapp.OpenWindows(localapp.WindowsOptions{ExportPath: exportPath, OwnerJID: owner})
	if err != nil {
		return err
	}
	defer src.Close()

	return doImport(src, src.OwnerJID(), exportPath, dbPath, dest, filter, dryRun, noOverwrite)
}

// doImport runs the shared import pipeline for any Source and prints a summary.
func doImport(src localapp.Source, ownerJID, srcDesc, dbPath string, dest *sql.DB, filter localapp.MessageFilter, dryRun, noOverwrite bool) error {
	if ownerJID == "" {
		fmt.Fprintln(os.Stderr, "warning: could not determine your own JID; sent messages will have an empty sender. "+
			"Pass --me <your-number> to fix this.")
	} else {
		fmt.Printf("Owner JID: %s\n", ownerJID)
	}

	fmt.Printf("Source:    %s\n", srcDesc)
	fmt.Printf("Dest:      %s\n", dbPath)
	switch {
	case dryRun:
		fmt.Println("Mode:      DRY RUN (no writes)")
	case noOverwrite:
		fmt.Println("Mode:      no-overwrite (fill gaps; upgrade [Protocol]-style placeholders)")
	}
	fmt.Println("Importing...")

	messageStore := storage.NewMessageStore(dest)
	mediaStore := storage.NewMediaStore(dest)

	start := time.Now()
	stats, err := localapp.Import(src, messageStore, mediaStore, localapp.ImportOptions{
		Filter:      filter,
		DryRun:      dryRun,
		NoOverwrite: noOverwrite,
		Progress: func(messages int) {
			fmt.Printf("\r  %d messages processed...", messages)
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}

	fmt.Printf("Done in %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Printf("  Chats:          %d\n", stats.Chats)
	fmt.Printf("  Push names:     %d\n", stats.PushNames)
	fmt.Printf("  Messages:       %d\n", stats.Messages)
	fmt.Printf("  Media records:  %d\n", stats.Media)
	fmt.Printf("  Replies:        %d\n", stats.Replies)
	if stats.MissingSender > 0 {
		fmt.Printf("  Unresolved senders (group): %d\n", stats.MissingSender)
	}
	return nil
}

// defaultChatStoragePath returns the standard macOS location of ChatStorage.sqlite.
func defaultChatStoragePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Group Containers",
		"group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite")
}

// openDestDB opens the destination messages database and runs migrations.
func openDestDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create destination directory: %w", err)
		}
	}

	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open destination DB: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to destination DB: %w", err)
	}
	if err := storage.NewMigrator(db).Migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate destination DB: %w", err)
	}
	return db, nil
}

// detectOwnerFromDest returns the most common sender JID among already-synced
// outgoing messages, or "" when none exist.
func detectOwnerFromDest(db *sql.DB) string {
	var jid string
	err := db.QueryRow(`
		SELECT sender_jid FROM messages
		WHERE is_from_me = 1 AND sender_jid <> ''
		GROUP BY sender_jid ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&jid)
	if err != nil {
		return ""
	}
	return jid
}

// copyDBFamily copies a SQLite database and its -wal/-shm sidecars into dstDir,
// returning the path of the copied main database file.
func copyDBFamily(srcMain, dstDir string) (string, error) {
	base := filepath.Base(srcMain)
	dstMain := filepath.Join(dstDir, base)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := srcMain + suffix
		if _, err := os.Stat(src); err != nil {
			continue // sidecar may not exist
		}
		if err := copyFile(src, dstMain+suffix); err != nil {
			return "", err
		}
	}
	return dstMain, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// parseDate accepts YYYY-MM-DD or RFC3339 timestamps.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid date %q (use YYYY-MM-DD or RFC3339)", s)
}
