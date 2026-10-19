package feed

import (
	"strings"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	tests := []struct {
		in      string
		display string
		key     string
		ok      bool
	}{
		{"elliott", "elliott", "elliott", true},
		{"  Elliott  ", "Elliott", "elliott", true},
		{"ELLIOTT", "ELLIOTT", "elliott", true},
		{"a.b_c-d", "a.b_c-d", "a.b_c-d", true},
		{"khėnsu", "khėnsu", "khėnsu", true},
		{"ñü", "ñü", "ñü", true},
		{"ÑÜ", "ÑÜ", "ñü", true},
		{"Straße", "Straße", "strasse", true},
		{"ﬁ", "fi", "fi", true},                 // NFKC ligature
		{"ｅｌｌｉｏｔｔ", "elliott", "elliott", true}, // fullwidth
		{"e\u0301", "é", "é", true},             // combining mark composes
		{"東京", "東京", "東京", true},
		{"٣٣", "٣٣", "٣٣", true}, // Arabic-Indic digits
		{"x", "x", "x", true},
		{strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), "", "", false},
		{"", "", "", false},
		{"   ", "", "", false},
		{"has space", "", "", false},
		{"slash/no", "", "", false},
		{"at@sign", "", "", false},
		{"emoji😀", "", "", false},
		{"zero\u200bwidth", "", "", false},
		{"tab\t", "tab", "tab", true},
		{"bad\xff", "", "", false},
		{"Ⅻ", "XII", "xii", true}, // compatibility character, NFKC makes letters
	}
	for _, tt := range tests {
		d, k, err := NormalizeHandle(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("%q: ok=%v, err=%v", tt.in, tt.ok, err)
			continue
		}
		if tt.ok && (d != tt.display || k != tt.key) {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", tt.in, d, k, tt.display, tt.key)
		}
	}
}

func TestAllowedOrigin(t *testing.T) {
	tests := map[string]bool{
		"https://khensu.nu":            true,
		"https://waves.khensu.nu":      true,
		"https://indra.og.khensu.nu":   true,
		"http://127.0.0.1:4321":        true,
		"http://localhost:4321":        true,
		"http://khensu.nu":             false,
		"https://evilkhensu.nu":        false,
		"https://khensu.nu.evil.com":   false,
		"https://waves.khensu.nu:8443": false,
		"https://.khensu.nu":           false,
		"https://-a.khensu.nu":         false,
		"http://localhost:3000":        false,
		"null":                         false,
		"":                             false,
	}
	for o, want := range tests {
		if got := AllowedOrigin(o); got != want {
			t.Errorf("AllowedOrigin(%q) = %v, want %v", o, got, want)
		}
	}
}
