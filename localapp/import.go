package localapp

import (
	"fmt"

	"whatsapp-mcp/storage"
)

// defaultBatchSize is the number of messages saved per transaction.
const defaultBatchSize = 1000

// ImportOptions controls an import run.
type ImportOptions struct {
	// Filter restricts which messages are imported.
	Filter MessageFilter
	// BatchSize is the number of messages per write transaction (default 1000).
	BatchSize int
	// DryRun reads and counts everything but writes nothing.
	DryRun bool
	// NoOverwrite, when true, never overwrites a message the destination already
	// has — it only inserts missing messages, except that existing rows whose
	// text is a live-sync placeholder (e.g. "[Protocol]") are upgraded to the
	// real text. The default (false) upserts every row, replacing existing ones.
	NoOverwrite bool
	// Progress, when set, is called periodically with the running message count.
	Progress func(messages int)
}

// ImportStats summarizes the result of an import run.
type ImportStats struct {
	Chats         int // chats inserted/updated
	PushNames     int // push names inserted/updated
	Messages      int // messages imported
	Media         int // media metadata records imported
	Replies       int // messages carrying a reply/quote reference
	MissingSender int // group messages with an unresolved sender
}

// Source is a read-only provider of a local WhatsApp installation's history in
// canonical form. Both the macOS Core Data reader (*Store) and the Windows
// IndexedDB-export reader (*WindowsStore) implement it, so they share the same
// Import pipeline.
type Source interface {
	// Chats returns all importable chat sessions.
	Chats() []ChatRecord
	// PushNames returns the JID -> display-name map.
	PushNames() (map[string]string, error)
	// IterateMessages streams messages (oldest first) matching filter.
	IterateMessages(filter MessageFilter, fn func(MessageRecord) error) error
}

// Import copies chats, push names and messages from the local WhatsApp store
// into the destination message/media stores. It is idempotent: re-running it
// upserts rows by their WhatsApp IDs, so it can run alongside the live sync.
func Import(src Source, dest *storage.MessageStore, media *storage.MediaStore, opts ImportOptions) (ImportStats, error) {
	var stats ImportStats

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	// 1. Chats first, so message foreign keys resolve.
	for _, chat := range src.Chats() {
		c := storage.Chat{
			JID:             chat.JID,
			LastMessageTime: chat.LastMessage,
			UnreadCount:     chat.UnreadCount,
			IsGroup:         chat.IsGroup,
		}
		// Group subjects live in PushName; DM display names in ContactName,
		// matching how the live whatsmeow sync populates these fields.
		if chat.IsGroup {
			c.PushName = chat.Name
		} else {
			c.ContactName = chat.Name
		}
		if !opts.DryRun {
			if err := dest.SaveChat(c); err != nil {
				return stats, fmt.Errorf("save chat %s: %w", chat.JID, err)
			}
		}
		stats.Chats++
	}

	// 2. Push names (display names for senders).
	pushNames, err := src.PushNames()
	if err != nil {
		return stats, fmt.Errorf("read push names: %w", err)
	}
	if !opts.DryRun && len(pushNames) > 0 {
		if err := dest.SavePushNames(pushNames); err != nil {
			return stats, fmt.Errorf("save push names: %w", err)
		}
	}
	stats.PushNames = len(pushNames)

	// 3. Messages (+ media) streamed in batches.
	msgBatch := make([]storage.Message, 0, batchSize)
	mediaBatch := make([]storage.MediaMetadata, 0, batchSize)

	flush := func() error {
		if len(msgBatch) == 0 {
			return nil
		}
		if !opts.DryRun {
			if opts.NoOverwrite {
				if err := dest.SaveBulkFillGaps(msgBatch); err != nil {
					return fmt.Errorf("save messages: %w", err)
				}
				if err := media.SaveMediaMetadataBulkFillGaps(mediaBatch); err != nil {
					return fmt.Errorf("save media: %w", err)
				}
			} else {
				if err := dest.SaveBulk(msgBatch); err != nil {
					return fmt.Errorf("save messages: %w", err)
				}
				if err := media.SaveMediaMetadataBulk(mediaBatch); err != nil {
					return fmt.Errorf("save media: %w", err)
				}
			}
		}
		msgBatch = msgBatch[:0]
		mediaBatch = mediaBatch[:0]
		if opts.Progress != nil {
			opts.Progress(stats.Messages)
		}
		return nil
	}

	err = src.IterateMessages(opts.Filter, func(rec MessageRecord) error {
		msgBatch = append(msgBatch, storage.Message{
			ID:          rec.ID,
			ChatJID:     rec.ChatJID,
			SenderJID:   rec.SenderJID,
			Text:        rec.Text,
			Timestamp:   rec.Timestamp,
			IsFromMe:    rec.IsFromMe,
			MessageType: rec.Type,
			ReplyToID:   rec.ReplyToID,
		})
		stats.Messages++
		if rec.ReplyToID != "" {
			stats.Replies++
		}
		if rec.SenderJID == "" && !rec.IsFromMe {
			stats.MissingSender++
		}

		// Record media metadata for true attachments (skip vcard/location,
		// which the live sync also omits from media_metadata).
		if rec.Media != nil && rec.Type != "vcard" && rec.Type != "location" {
			mediaBatch = append(mediaBatch, storage.MediaMetadata{
				MessageID:      rec.ID,
				FileName:       rec.Media.FileName,
				FileSize:       rec.Media.FileSize,
				MimeType:       rec.Media.MimeType,
				Duration:       rec.Media.Duration,
				DownloadStatus: "external", // file lives in the WhatsApp app's own store
			})
			stats.Media++
		}

		if len(msgBatch) >= batchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return stats, err
	}

	if err := flush(); err != nil {
		return stats, err
	}

	return stats, nil
}
