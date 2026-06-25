package localapp

import "testing"

func TestWinMessageTypeLabel(t *testing.T) {
	cases := map[string]string{
		"chat":             "text",
		"image":            "image",
		"album":            "image",
		"video":            "video",
		"ptv":              "video",
		"ptt":              "audio",
		"audio":            "audio",
		"document":         "document",
		"sticker":          "sticker",
		"vcard":            "vcard",
		"multi_vcard":      "vcard",
		"location":         "location",
		"poll_creation":    "text",
		"groups_v4_invite": "text",
		"gp2":              "system",
		"e2e_notification": "system",
		"call_log":         "system",
		"revoked":          "system",
		"ciphertext":       "system",
		"unknown":          "unknown",
		"something_new":    "unknown",
		"CHAT":             "text", // case-insensitive
	}
	for in, want := range cases {
		if got := winMessageTypeLabel(in); got != want {
			t.Errorf("winMessageTypeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWinIsSystemType(t *testing.T) {
	for _, sys := range []string{"gp2", "e2e_notification", "revoked", "call_log", "protocol", "CIPHERTEXT"} {
		if !winIsSystemType(sys) {
			t.Errorf("winIsSystemType(%q) = false, want true", sys)
		}
	}
	for _, ok := range []string{"chat", "image", "video", "unknown", ""} {
		if winIsSystemType(ok) {
			t.Errorf("winIsSystemType(%q) = true, want false", ok)
		}
	}
}

func TestNormalizeUserJIDCUS(t *testing.T) {
	lidMap := map[string]string{"100304817270818": "17348348224"}
	cases := map[string]string{
		"16502833196@c.us":        "16502833196@s.whatsapp.net", // legacy individual server
		"16502833196:3@c.us":      "16502833196@s.whatsapp.net", // device suffix stripped
		"100304817270818@lid":     "17348348224@s.whatsapp.net", // lid mapped to phone
		"120363131431675089@g.us": "120363131431675089@g.us",    // group unchanged
	}
	for raw, want := range cases {
		if got := normalizeUserJID(raw, lidMap); got != want {
			t.Errorf("normalizeUserJID(%q) = %q, want %q", raw, got, want)
		}
	}
}
