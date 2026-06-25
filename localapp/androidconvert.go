package localapp

import (
	"path/filepath"
	"strings"
	"time"
)

// msTimeToTime converts an Android msgstore timestamp (milliseconds since the
// Unix epoch) into a time.Time. A zero/negative value maps to the zero time.
func msTimeToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
}

// message_type codes used by the modern Android WhatsApp msgstore "message"
// table. Only the well-established, stable codes are named here; everything else
// falls back to the media MIME/extension (preferred when present) or "unknown".
// System/notification events are modeled by WhatsApp in a separate
// "message_system" table, so the reader skips those by the join rather than by
// guessing the volatile higher type codes.
const (
	androidTypeText     = 0  // plain text (incl. link previews)
	androidTypeImage    = 1  // image
	androidTypeAudio    = 2  // voice note / audio
	androidTypeVideo    = 3  // video
	androidTypeContact  = 4  // shared contact (vCard)
	androidTypeLocation = 5  // location
	androidTypeSystem   = 7  // system / protocol notification
	androidTypeDocument = 9  // document
	androidTypeGIF      = 13 // GIF (stored as mp4)
	androidTypeLiveLoc  = 16 // live location
	androidTypeSticker  = 20 // sticker (.webp)
)

// androidMessageTypeLabel maps an Android message_type (refined by the media
// MIME type / file extension when available) to the canonical label vocabulary
// used elsewhere in this project (see whatsapp.getMediaTypeFromMessage). The
// MIME/extension is preferred because the numeric codes drift between WhatsApp
// versions, whereas an attached file's type is unambiguous.
func androidMessageTypeLabel(msgType int64, mimeType, filePath string) string {
	if label := labelFromMime(mimeType); label != "" {
		// A GIF is delivered as an mp4; trust the type code to distinguish it.
		if label == "video" && msgType == androidTypeGIF {
			return "gif"
		}
		return label
	}
	if ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filePath), ".")); ext != "" {
		switch ext {
		case "webp":
			return "sticker"
		case "jpg", "jpeg", "png", "heic":
			return "image"
		case "gif":
			return "gif"
		case "mp4", "mov", "m4v", "3gp":
			if msgType == androidTypeGIF {
				return "gif"
			}
			return "video"
		case "opus", "ogg", "mp3", "m4a", "aac", "wav":
			return "audio"
		case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "zip", "csv":
			return "document"
		}
	}

	switch msgType {
	case androidTypeText:
		return "text"
	case androidTypeImage:
		return "image"
	case androidTypeAudio:
		return "audio"
	case androidTypeVideo:
		return "video"
	case androidTypeContact:
		return "vcard"
	case androidTypeLocation, androidTypeLiveLoc:
		return "location"
	case androidTypeDocument:
		return "document"
	case androidTypeGIF:
		return "gif"
	case androidTypeSticker:
		return "sticker"
	case androidTypeSystem:
		return "system"
	default:
		return "unknown"
	}
}

// androidIsSystemCode reports whether a message_type code is a known
// system/notification event skipped by default. The authoritative signal is the
// presence of a "message_system" row (handled by the reader); this is a backstop
// for stores that lack that table.
func androidIsSystemCode(msgType int64) bool {
	return msgType == androidTypeSystem
}

// labelFromMime maps a MIME type to a canonical media label, or "" when the
// MIME type is empty/unrecognized.
func labelFromMime(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType == "" {
		return ""
	}
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	switch mimeType {
	case "image/webp":
		return "sticker"
	case "application/pdf", "application/msword", "text/plain", "text/csv", "application/zip":
		return "document"
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case strings.HasPrefix(mimeType, "application/vnd.") ||
		strings.HasPrefix(mimeType, "application/") && mimeType != "application/octet-stream":
		return "document"
	default:
		return ""
	}
}
