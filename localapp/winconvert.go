package localapp

import "strings"

// WhatsApp-for-Windows message "type" strings, as stored in the WebView2
// IndexedDB "message" object store (the WhatsApp Web data model). These map onto
// the canonical label vocabulary used elsewhere in this project (the same one
// the macOS reader and the live whatsmeow sync emit).
//
// Types not listed map to "unknown". System/non-content types are skipped by
// default during import (mirroring the macOS reader's handling of group/system
// events); pass IncludeSystem to keep them.
var winTypeLabels = map[string]string{
	"chat":          "text",
	"image":         "image",
	"album":         "image", // a multi-image container; closest canonical label
	"video":         "video",
	"ptv":           "video", // "push to video" (video note)
	"gif":           "gif",
	"ptt":           "audio", // "push to talk" (voice note)
	"audio":         "audio",
	"document":      "document",
	"sticker":       "sticker",
	"vcard":         "vcard",
	"multi_vcard":   "vcard",
	"location":      "location",
	"live_location": "location",
	// Interactive / structured messages carry user-visible text.
	"poll_creation":     "text",
	"groups_v4_invite":  "text",
	"list":              "text",
	"list_response":     "text",
	"buttons_response":  "text",
	"template":          "text",
	"highly_structured": "text",
	"interactive":       "text",
	"product":           "text",
	"order":             "text",
}

// winSystemTypes are non-content notifications skipped by default.
var winSystemTypes = map[string]struct{}{
	"gp2":                     {}, // group v2 system event (add/remove/subject/...)
	"e2e_notification":        {},
	"call_log":                {},
	"notification_template":   {},
	"message_history_notice":  {},
	"protocol":                {},
	"revoked":                 {}, // message deleted/revoked
	"ciphertext":              {}, // undecryptable
	"pinned_message":          {},
	"biz_content_placeholder": {},
	"keep_in_chat":            {},
}

// winMessageTypeLabel maps a WhatsApp-Windows message type to a canonical label.
func winMessageTypeLabel(waType string) string {
	if label, ok := winTypeLabels[strings.ToLower(strings.TrimSpace(waType))]; ok {
		return label
	}
	if _, ok := winSystemTypes[strings.ToLower(strings.TrimSpace(waType))]; ok {
		return "system"
	}
	return "unknown"
}

// winIsSystemType reports whether a Windows message type is a non-content
// system/notification message skipped by default during import.
func winIsSystemType(waType string) bool {
	_, ok := winSystemTypes[strings.ToLower(strings.TrimSpace(waType))]
	return ok
}
