package wiki

import (
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestSourceValidationRules(t *testing.T) {
	page := func(u string) string { return "---\nsource_url: " + u + "\n---\nBody" }
	for _, tc := range []struct {
		name            string
		pages, previous map[string]string
		items           []model.InboxItem
		rule            string
	}{
		{"a", map[string]string{"wiki/source-a.md": "body"}, nil, nil, "(a)"},
		{"b", map[string]string{"wiki/source-a.md": page("https://example.com/a"), "wiki/source-b.md": page("https://example.com/a")}, nil, nil, "(b)"},
		{"c", map[string]string{"wiki/source-a.md": page("https://example.com/a")}, nil, []model.InboxItem{{Status: "removed", SourceURL: "https://example.com/a"}}, "(c)"},
		{"d", nil, map[string]string{"wiki/source-old.md": page("https://example.com/a")}, nil, "(d)"},
		{"e", nil, nil, []model.InboxItem{{Status: "fetched", SourceURL: "https://example.com/a"}}, "(e)"},
		{"updated-name", map[string]string{"wiki/source-new.md": page("https://example.com/a")}, map[string]string{"wiki/source-old.md": page("https://example.com/a")}, []model.InboxItem{{Status: "fetched", SourceURL: "https://example.com/a"}}, ""},
		{"slug-collision", map[string]string{"wiki/source-title.md": page("https://example.com/a"), "wiki/source-title-2.md": page("https://example.com/b")}, nil, nil, ""},
		{"removed", nil, map[string]string{"wiki/source-a.md": page("https://example.com/a")}, []model.InboxItem{{Status: "removed", SourceURL: "https://example.com/a"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateSources(tc.pages, tc.previous, tc.items)
			if tc.rule == "" {
				if len(got) != 0 {
					t.Fatal(got)
				}
			} else if len(got) != 1 || !strings.HasPrefix(got[0], tc.rule) {
				t.Fatal(got)
			}
		})
	}
	for _, body := range []string{"---\nsource_url: 123\n---\n", "---\nsource_url: a\nsource_url: b\n---\n", "---\nsource_url: [a]\n---\n"} {
		if _, err := SourceURL(body); err == nil {
			t.Fatal(body)
		}
	}
}

func TestSourceURLFrontmatterBoundaries(t *testing.T) {
	const metadata = "source_url: https://example.com/a\n"
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"closing at eof", "---\n" + metadata + "---", true},
		{"crlf", strings.ReplaceAll("---\n"+metadata+"---\nBody", "\n", "\r\n"), true},
		{"empty block before metadata", "---\n---\n" + metadata + "---\nBody", false},
		{"duplicate other key", "---\n" + metadata + "title: first\ntitle: second\n---\nBody", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, err := SourceURL(tc.body)
			if (err == nil) != tc.valid || (tc.valid && url != "https://example.com/a") {
				t.Fatalf("SourceURL = %q, %v; valid = %v", url, err, tc.valid)
			}
		})
	}
}
