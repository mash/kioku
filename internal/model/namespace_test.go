package model

import (
	"strings"
	"testing"
)

func TestValidSlug(t *testing.T) {
	for _, s := range []string{"a", "0", "acme-123", strings.Repeat("a", 63)} {
		if !ValidSlug(s) {
			t.Errorf("rejected %q", s)
		}
	}
	for _, s := range []string{"", strings.Repeat("a", 64), "日本語", "é", "Ａ", "UPPER", "a b", "a/b", "a\\b", ".", "..", "a.b", "a_b", "a%2fb", "-a", "a-", "a--b", "a\n", "a\x00"} {
		if ValidSlug(s) {
			t.Errorf("accepted %q", s)
		}
	}
}
