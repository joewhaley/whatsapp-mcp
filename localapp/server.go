package localapp

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	sqlite "modernc.org/sqlite"

	"whatsapp-mcp/storage"
)

// ServerStore exposes the native WhatsApp desktop database (ChatStorage.sqlite)
// as a read-only source for the MCP server. It opens the database read-only and
// projects the Core Data tables into the project's canonical schema using
// connection-local TEMP views, so the existing storage.MessageStore /
// storage.MediaStore (and therefore all read-only MCP tools) work unchanged
// without ever writing to — or copying — the native database.
type ServerStore struct {
	db        *sql.DB
	Messages  *storage.MessageStore
	Media     *storage.MediaStore
	ownerJID  string
	ownerName string
}

// serverConfig is the active configuration consulted by the globally-registered
// SQL functions and connection hook. Local mode runs a single ServerStore, so a
// single atomically-swapped config is sufficient.
type serverConfig struct {
	path   string
	owner  string
	lidMap map[string]string
}

var (
	activeConfig atomic.Pointer[serverConfig]
	regOnce      sync.Once
)

// OpenServer opens the native database read-only and prepares it for serving.
func OpenServer(opts Options) (*ServerStore, error) {
	// Reuse the reader to load the LID map, chat sessions and owner detection.
	base, err := Open(Options{
		ChatStoragePath: opts.ChatStoragePath,
		LIDPath:         opts.LIDPath,
		OwnerJID:        opts.OwnerJID,
		ReadOnly:        true,
	})
	if err != nil {
		return nil, err
	}
	lidMap := base.lidMap
	owner := base.ownerJID
	ownerName := ""
	if owner != "" {
		if names, err := base.PushNames(); err == nil {
			ownerName = names[owner]
		}
	}
	base.Close()

	// Publish config, then register the SQL functions + connection hook that
	// build the canonical views. Registration must precede opening the pool.
	activeConfig.Store(&serverConfig{path: opts.ChatStoragePath, owner: owner, lidMap: lidMap})
	registerSQLExtensions()

	dsn := "file:" + opts.ChatStoragePath + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open native DB read-only: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to native DB: %w", err)
	}

	// Sanity check: the projected view must be queryable on every pooled conn.
	if _, err := db.Exec("SELECT 1 FROM messages_with_names LIMIT 1"); err != nil {
		db.Close()
		return nil, fmt.Errorf("canonical views unavailable (schema mismatch?): %w", err)
	}

	return &ServerStore{
		db:        db,
		Messages:  storage.NewMessageStore(db),
		Media:     storage.NewMediaStore(db),
		ownerJID:  owner,
		ownerName: ownerName,
	}, nil
}

// Close releases the database handle.
func (s *ServerStore) Close() error { return s.db.Close() }

// OwnerJID returns the account owner's canonical JID (may be empty if unknown).
func (s *ServerStore) OwnerJID() string { return s.ownerJID }

// OwnerName returns the account owner's display name, if known.
func (s *ServerStore) OwnerName() string { return s.ownerName }

// registerSQLExtensions registers the custom SQL functions and the connection
// hook exactly once. They are global to the modernc driver but no-op for any
// connection whose DSN does not match the active native database path.
func registerSQLExtensions() {
	regOnce.Do(func() {
		sqlite.MustRegisterDeterministicScalarFunction("wa_unix", 1,
			func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
				if a[0] == nil {
					return int64(0), nil
				}
				f := asFloat(a[0])
				if f == 0 {
					return int64(0), nil
				}
				return int64(f) + coreDataEpochOffset, nil
			})

		sqlite.MustRegisterDeterministicScalarFunction("wa_msgtype", 2,
			func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
				return messageTypeLabel(asInt64(a[0]), asString(a[1])), nil
			})

		sqlite.MustRegisterDeterministicScalarFunction("wa_normjid", 1,
			func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
				return normalizeUserJID(asString(a[0]), currentLIDMap()), nil
			})

		sqlite.MustRegisterDeterministicScalarFunction("wa_placeholder", 2,
			func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
				return placeholderText(asString(a[0]), asString(a[1])), nil
			})

		sqlite.MustRegisterDeterministicScalarFunction("wa_basename", 1,
			func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
				p := asString(a[0])
				if p == "" {
					return "", nil
				}
				return filepath.Base(p), nil
			})

		sqlite.MustRegisterDeterministicScalarFunction("wa_mime", 1,
			func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
				return mimeFromExt(asString(a[0])), nil
			})

		sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
			cfg := activeConfig.Load()
			if cfg == nil || cfg.path == "" || !strings.Contains(dsn, cfg.path) {
				return nil // not our read-only native connection
			}
			for _, ddl := range canonicalViewDDL(cfg.owner) {
				if _, err := conn.ExecContext(context.Background(), ddl, nil); err != nil {
					return fmt.Errorf("create canonical view: %w", err)
				}
			}
			return nil
		})
	})
}

// currentLIDMap returns the LID map for the active config (nil-safe).
func currentLIDMap() map[string]string {
	if cfg := activeConfig.Load(); cfg != nil {
		return cfg.lidMap
	}
	return nil
}

// canonicalViewDDL returns the TEMP view statements that project the Core Data
// tables onto the canonical schema (chats, messages, push_names,
// media_metadata, messages_with_names). owner is the account-owner JID used for
// sent messages; it is injected as a SQL string literal.
func canonicalViewDDL(owner string) []string {
	ownerLit := "'" + strings.ReplaceAll(owner, "'", "''") + "'"

	return []string{
		// chats — one row per resolved JID (sessions sharing a JID are merged).
		`CREATE TEMP VIEW IF NOT EXISTS chats AS
		SELECT
		  jid,
		  CASE WHEN jid LIKE '%@g.us' THEN name ELSE '' END AS push_name,
		  CASE WHEN jid LIKE '%@g.us' THEN ''   ELSE name END AS contact_name,
		  last_message_time,
		  unread_count,
		  (jid LIKE '%@g.us') AS is_group
		FROM (
		  SELECT
		    wa_normjid(ZCONTACTJID)              AS jid,
		    MAX(COALESCE(ZPARTNERNAME, ''))      AS name,
		    wa_unix(MAX(ZLASTMESSAGEDATE))       AS last_message_time,
		    MAX(COALESCE(ZUNREADCOUNT, 0))       AS unread_count
		  FROM ZWACHATSESSION
		  WHERE ZCONTACTJID IS NOT NULL AND ZCONTACTJID <> ''
		    AND ZCONTACTJID NOT LIKE '%@status'
		    AND ZCONTACTJID NOT LIKE '%@broadcast'
		    AND ZCONTACTJID NOT LIKE '%.status'
		  GROUP BY wa_normjid(ZCONTACTJID)
		)`,

		// push_names — display names keyed by resolved JID.
		`CREATE TEMP VIEW IF NOT EXISTS push_names AS
		SELECT jid, push_name, 0 AS updated_at FROM (
		  SELECT wa_normjid(ZJID) AS jid, MAX(ZPUSHNAME) AS push_name
		  FROM ZWAPROFILEPUSHNAME
		  WHERE ZPUSHNAME IS NOT NULL AND ZPUSHNAME <> ''
		  GROUP BY wa_normjid(ZJID)
		)`,

		// media_metadata — one row per on-disk attachment.
		`CREATE TEMP VIEW IF NOT EXISTS media_metadata AS
		SELECT
		  COALESCE(NULLIF(m.ZSTANZAID, ''), 'mac-' || m.Z_PK) AS message_id,
		  NULL                              AS file_path,
		  wa_basename(mi.ZMEDIALOCALPATH)   AS file_name,
		  COALESCE(mi.ZFILESIZE, 0)         AS file_size,
		  wa_mime(mi.ZMEDIALOCALPATH)       AS mime_type,
		  NULL                              AS width,
		  NULL                              AS height,
		  CASE WHEN COALESCE(mi.ZMOVIEDURATION, 0) > 0
		       THEN CAST(mi.ZMOVIEDURATION AS INTEGER) ELSE NULL END AS duration,
		  NULL AS media_key, NULL AS direct_path, NULL AS file_sha256, NULL AS file_enc_sha256,
		  'external' AS download_status, NULL AS download_timestamp, NULL AS download_error,
		  '' AS created_at
		FROM ZWAMEDIAITEM mi
		JOIN ZWAMESSAGE m ON mi.ZMESSAGE = m.Z_PK
		WHERE mi.ZMEDIALOCALPATH IS NOT NULL AND mi.ZMEDIALOCALPATH <> ''`,

		// messages — canonical message rows projected from ZWAMESSAGE.
		`CREATE TEMP VIEW IF NOT EXISTS messages AS
		SELECT
		  COALESCE(NULLIF(m.ZSTANZAID, ''), 'mac-' || m.Z_PK) AS id,
		  wa_normjid(cs.ZCONTACTJID) AS chat_jid,
		  CASE
		    WHEN m.ZISFROMME = 1               THEN ` + ownerLit + `
		    WHEN cs.ZCONTACTJID LIKE '%@g.us'  THEN COALESCE(wa_normjid(gm.ZMEMBERJID), '')
		    ELSE wa_normjid(cs.ZCONTACTJID)
		  END AS sender_jid,
		  COALESCE(NULLIF(TRIM(m.ZTEXT), ''),
		           wa_placeholder(wa_msgtype(m.ZMESSAGETYPE, mi.ZMEDIALOCALPATH), mi.ZVCARDNAME)) AS text,
		  wa_unix(m.ZMESSAGEDATE) AS timestamp,
		  m.ZISFROMME AS is_from_me,
		  wa_msgtype(m.ZMESSAGETYPE, mi.ZMEDIALOCALPATH) AS message_type,
		  0 AS created_at,
		  NULLIF(TRIM(COALESCE(p.ZSTANZAID, '')), '') AS reply_to_id
		FROM ZWAMESSAGE m
		JOIN ZWACHATSESSION cs ON m.ZCHATSESSION = cs.Z_PK
		LEFT JOIN ZWAGROUPMEMBER gm ON m.ZGROUPMEMBER = gm.Z_PK
		LEFT JOIN ZWAMEDIAITEM   mi ON m.ZMEDIAITEM   = mi.Z_PK
		LEFT JOIN ZWAMESSAGE      p ON m.ZPARENTMESSAGE = p.Z_PK
		WHERE cs.ZCONTACTJID IS NOT NULL AND cs.ZCONTACTJID <> ''
		  AND cs.ZCONTACTJID NOT LIKE '%@status'
		  AND cs.ZCONTACTJID NOT LIKE '%@broadcast'
		  AND cs.ZCONTACTJID NOT LIKE '%.status'
		  AND m.ZMESSAGETYPE NOT IN (6, 14)`,

		// messages_with_names — mirrors storage migration 001's view exactly.
		`CREATE TEMP VIEW IF NOT EXISTS messages_with_names AS
		SELECT
		  m.id,
		  m.chat_jid,
		  m.sender_jid,
		  COALESCE(p.push_name, '') AS sender_push_name,
		  COALESCE(c_sender.contact_name, '') AS sender_contact_name,
		  COALESCE(c_chat.contact_name, c_chat.push_name, m.chat_jid) AS chat_name,
		  m.text,
		  m.timestamp,
		  m.is_from_me,
		  m.message_type,
		  m.created_at,
		  media.file_path AS media_file_path,
		  media.file_name AS media_file_name,
		  media.file_size AS media_file_size,
		  media.mime_type AS media_mime_type,
		  media.width AS media_width,
		  media.height AS media_height,
		  media.duration AS media_duration,
		  media.download_status AS media_download_status,
		  media.download_timestamp AS media_download_timestamp,
		  media.download_error AS media_download_error
		FROM messages m
		LEFT JOIN push_names p ON m.sender_jid = p.jid
		LEFT JOIN chats c_sender ON m.sender_jid = c_sender.jid
		LEFT JOIN chats c_chat ON m.chat_jid = c_chat.jid
		LEFT JOIN media_metadata media ON m.id = media.message_id`,
	}
}

// --- driver.Value coercion helpers -----------------------------------------

func asString(v driver.Value) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func asInt64(v driver.Value) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	default:
		return 0
	}
}

func asFloat(v driver.Value) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	default:
		return 0
	}
}
