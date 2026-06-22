package localapp

import (
	"testing"
	"time"
)

func TestNSDateToTime(t *testing.T) {
	// 803797224 (NSDate) + 978307200 = 1782104424 (Unix).
	got := nsDateToTime(803797224)
	if want := time.Unix(1782104424, 0); !got.Equal(want) {
		t.Fatalf("nsDateToTime = %v, want %v", got, want)
	}

	if z := nsDateToTime(0); !z.IsZero() {
		t.Fatalf("nsDateToTime(0) = %v, want zero time", z)
	}

	// Fractional seconds should be preserved to ~ms precision.
	frac := nsDateToTime(803797224.5)
	if frac.Nanosecond() < 4e8 || frac.Nanosecond() > 6e8 {
		t.Fatalf("nsDateToTime fractional nanos = %d, want ~5e8", frac.Nanosecond())
	}
}

func TestMessageTypeLabel(t *testing.T) {
	cases := []struct {
		zType     int64
		mediaPath string
		want      string
	}{
		{macTypeText, "", "text"},
		{macTypeURL, "", "text"},
		{macTypeImage, "Media/x/a/b.jpg", "image"},
		{macTypeVideo, "Media/x/a/b.mp4", "video"},
		{macTypeAudio, "Media/x/a/b.opus", "audio"},
		{macTypeDocument, "Media/x/a/b.pdf", "document"},
		{macTypeSticker, "Media/x/a/b.webp", "sticker"},
		{macTypeGIF, "Media/x/a/b.mp4", "gif"},
		{macTypeContact, "", "vcard"},
		{macTypeLocation, "", "location"},
		{macTypeGroupEvt, "", "system"},
		{macTypeSystem, "", "system"},
		{99, "", "unknown"},
		// Extension overrides an ambiguous/unknown numeric code.
		{99, "x/y/z.JPG", "image"},
		{0, "x/y/z.webp", "sticker"},
	}
	for _, c := range cases {
		if got := messageTypeLabel(c.zType, c.mediaPath); got != c.want {
			t.Errorf("messageTypeLabel(%d, %q) = %q, want %q", c.zType, c.mediaPath, got, c.want)
		}
	}
}

func TestNormalizeUserJID(t *testing.T) {
	lidMap := map[string]string{"208288885035139": "15103788422"}
	cases := []struct {
		raw  string
		want string
	}{
		{"208288885035139@lid", "15103788422@s.whatsapp.net"}, // mapped LID -> phone
		{"999999999999999@lid", "999999999999999@lid"},        // unmapped LID kept
		{"16508687737@s.whatsapp.net", "16508687737@s.whatsapp.net"},
		{"120363419394970746@g.us", "120363419394970746@g.us"},          // group unchanged
		{"16502833196:12@s.whatsapp.net", "16502833196@s.whatsapp.net"}, // device suffix stripped
		{"208288885035139.0@lid", "15103788422@s.whatsapp.net"},         // agent suffix stripped then mapped
		{"", ""},
		{"status@broadcast", "status@broadcast"},
	}
	for _, c := range cases {
		if got := normalizeUserJID(c.raw, lidMap); got != c.want {
			t.Errorf("normalizeUserJID(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestPlaceholderText(t *testing.T) {
	if got := placeholderText("image", ""); got != "[Image]" {
		t.Errorf("image placeholder = %q", got)
	}
	if got := placeholderText("vcard", "Alice"); got != "[Contact: Alice]" {
		t.Errorf("vcard placeholder = %q", got)
	}
	if got := placeholderText("text", ""); got != "" {
		t.Errorf("text placeholder = %q, want empty", got)
	}
}

func TestIsSelfName(t *testing.T) {
	// Real value carries a leading U+200E left-to-right mark.
	if !isSelfName("‎You") {
		t.Error("expected \\u200eYou to be detected as self")
	}
	if !isSelfName("You") {
		t.Error("expected You to be detected as self")
	}
	if isSelfName("Younger Brother") {
		t.Error("did not expect 'Younger Brother' to be self")
	}
}

func TestImportableJID(t *testing.T) {
	cases := map[string]bool{
		"16502833196@s.whatsapp.net": true,
		"120363419394970746@g.us":    true,
		"999@lid":                    true,
		"0@status":                   false,
		"status@broadcast":           false,
		"123@lid.status":             false,
		"":                           false,
	}
	for jid, want := range cases {
		if got := importableJID(jid); got != want {
			t.Errorf("importableJID(%q) = %v, want %v", jid, got, want)
		}
	}
}

func TestMimeFromExt(t *testing.T) {
	cases := map[string]string{
		"a/b/c.jpg":  "image/jpeg",
		"a/b/c.opus": "audio/ogg",
		"a/b/c.pdf":  "application/pdf",
		"a/b/c.xyz":  "application/octet-stream",
		"a/b/c":      "",
	}
	for path, want := range cases {
		if got := mimeFromExt(path); got != want {
			t.Errorf("mimeFromExt(%q) = %q, want %q", path, got, want)
		}
	}
}
