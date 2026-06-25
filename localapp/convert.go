// Package localapp parses the local WhatsApp desktop (macOS) application's
// Core Data store (ChatStorage.sqlite) and exposes its chats and messages in
// the canonical form used by the rest of this project. It lets the locally
// installed WhatsApp app act as a source of messages, independent of (and
// richer than) the whatsmeow history sync.
package localapp

import (
	"path/filepath"
	"strings"
	"time"
)

// coreDataEpochOffset is the number of seconds between the Unix epoch
// (1970-01-01) and the Core Data / NSDate reference date (2001-01-01 UTC).
// ChatStorage timestamps are stored as seconds since the NSDate epoch.
const coreDataEpochOffset = 978307200

// WhatsApp JID server suffixes.
const (
	serverUser       = "s.whatsapp.net"
	serverCUS        = "c.us" // legacy individual server (WhatsApp Web / Windows app)
	serverLID        = "lid"
	serverGroup      = "g.us"
	serverBroadcast  = "broadcast"
	serverNewsletter = "newsletter"
	serverStatus     = "status"
)

// nsDateToTime converts a Core Data NSDate value (seconds since 2001-01-01)
// into a time.Time. A zero/empty value maps to the zero time.
func nsDateToTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	unix := seconds + coreDataEpochOffset
	sec := int64(unix)
	nsec := int64((unix - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

// ZMESSAGETYPE codes used by the macOS WhatsApp Core Data store. These were
// derived empirically from a real store (cross-checked against attached media
// file extensions). Unknown codes fall back to "unknown".
const (
	macTypeText     = 0  // plain text
	macTypeImage    = 1  // image (.jpg)
	macTypeVideo    = 2  // video (.mp4)
	macTypeAudio    = 3  // voice note / audio (.opus)
	macTypeContact  = 4  // shared contact (vCard)
	macTypeLocation = 5  // location
	macTypeGroupEvt = 6  // group/system event (member added, etc.)
	macTypeURL      = 7  // text with link preview
	macTypeDocument = 8  // document (.pdf, ...)
	macTypeGIF      = 11 // GIF (stored as .mp4)
	macTypeSystem   = 14 // protocol / e2e notification
	macTypeSticker  = 15 // sticker (.webp)
)

// messageTypeLabel maps a macOS ZMESSAGETYPE (refined by the media file
// extension when available) to the canonical label vocabulary used elsewhere
// in this project (see whatsapp.getMediaTypeFromMessage).
func messageTypeLabel(zType int64, mediaPath string) string {
	// Prefer the file extension when a media item is attached: it is the most
	// reliable signal and survives version-specific numeric code drift.
	if ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(mediaPath), ".")); ext != "" {
		switch ext {
		case "jpg", "jpeg", "png", "gif", "heic", "webp":
			if ext == "webp" {
				return "sticker"
			}
			if zType == macTypeGIF {
				return "gif"
			}
			return "image"
		case "mp4", "mov", "m4v", "3gp":
			if zType == macTypeGIF {
				return "gif"
			}
			return "video"
		case "opus", "ogg", "mp3", "m4a", "aac", "wav":
			return "audio"
		case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "zip", "csv":
			return "document"
		}
	}

	switch zType {
	case macTypeText:
		return "text"
	case macTypeImage:
		return "image"
	case macTypeVideo:
		return "video"
	case macTypeAudio:
		return "audio"
	case macTypeContact:
		return "vcard"
	case macTypeLocation:
		return "location"
	case macTypeURL:
		return "text"
	case macTypeDocument:
		return "document"
	case macTypeGIF:
		return "gif"
	case macTypeSticker:
		return "sticker"
	case macTypeGroupEvt, macTypeSystem:
		return "system"
	default:
		return "unknown"
	}
}

// isSystemType reports whether a message type represents a non-content
// system/group event that is skipped by default during import.
func isSystemType(zType int64) bool {
	return zType == macTypeGroupEvt || zType == macTypeSystem
}

// placeholderText returns a human-readable placeholder for a media/non-text
// message that has no caption, mirroring the convention used by the live
// whatsmeow sync (e.g. "[Image]"). It returns "" when no placeholder applies.
func placeholderText(label, vcardName string) string {
	switch label {
	case "image":
		return "[Image]"
	case "video":
		return "[Video]"
	case "audio":
		return "[Audio]"
	case "document":
		return "[Document]"
	case "sticker":
		return "[Sticker]"
	case "gif":
		return "[GIF]"
	case "location":
		return "[Location]"
	case "vcard":
		if vcardName != "" {
			return "[Contact: " + vcardName + "]"
		}
		return "[Contact]"
	case "system":
		return "[System message]"
	default:
		return ""
	}
}

// normalizeUserJID converts a raw ChatStorage JID into the canonical form used
// by the rest of the project. It mirrors whatsmeow's normalizeJID:
//   - device/agent suffixes are stripped (non-AD form),
//   - @lid JIDs are resolved to phone-number (@s.whatsapp.net) form when a
//     mapping exists, preventing duplicate contacts,
//   - groups, broadcasts and newsletters are returned unchanged.
//
// lidMap maps a bare LID user ("208288885035139") to a bare phone number
// ("15103788422").
func normalizeUserJID(raw string, lidMap map[string]string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	user, server := raw[:at], raw[at+1:]

	// strip device (":12") and agent (".0") suffixes -> non-AD form
	if i := strings.IndexAny(user, ":."); i >= 0 {
		user = user[:i]
	}

	switch server {
	case serverGroup, serverBroadcast, serverNewsletter, serverStatus:
		return user + "@" + server
	case serverCUS:
		// Legacy individual server: canonicalize to the modern user server.
		return user + "@" + serverUser
	case serverLID:
		if pn, ok := lidMap[user]; ok && pn != "" {
			return pn + "@" + serverUser
		}
		return user + "@" + serverLID
	default:
		return user + "@" + server
	}
}

// isGroupJID reports whether a canonical JID refers to a group chat.
func isGroupJID(jid string) bool {
	return strings.HasSuffix(jid, "@"+serverGroup)
}

// mimeFromExt returns a best-effort MIME type for a media file path based on
// its extension. It returns "application/octet-stream" for unknown extensions
// and "" for paths without an extension.
func mimeFromExt(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return ""
	}
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "heic":
		return "image/heic"
	case "mp4", "m4v":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "3gp":
		return "video/3gpp"
	case "opus", "ogg":
		return "audio/ogg"
	case "mp3":
		return "audio/mpeg"
	case "m4a", "aac":
		return "audio/aac"
	case "wav":
		return "audio/wav"
	case "pdf":
		return "application/pdf"
	case "doc":
		return "application/msword"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xls":
		return "application/vnd.ms-excel"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "ppt":
		return "application/vnd.ms-powerpoint"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case "txt":
		return "text/plain"
	case "csv":
		return "text/csv"
	case "zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}
