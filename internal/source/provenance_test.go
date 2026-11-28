package source

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestInboxFileRejectsTrailingYAML(t *testing.T) {
	input := "---\nsource_url: kioku:local\nupdated_at: 2026-11-28T00:00:00Z\n...\ntitle: Keep me\n---\nBody\n"
	if _, content, err := InboxFile("note.md", input, time.Time{}); err == nil {
		t.Fatalf("accepted and discarded frontmatter after YAML end marker: %q", content)
	}
}

func TestFileProvenance(t *testing.T) {
	const updated = "2026-11-07T18:00:00+09:00"
	valid := "---\nsource_url: https://example.com/doc\nupdated_at: " + updated + "\n---\nEvidence"
	for _, tc := range []struct {
		name, content, errorKey string
	}{
		{"frontmatter", valid, ""},
		{"tagged scalar", strings.ReplaceAll(valid, updated, `!custom "`+updated+`"`), ""},
		{"quoted", strings.ReplaceAll(valid, updated, `"`+updated+`"`), ""},
		{"crlf", strings.ReplaceAll(valid, "\n", "\r\n"), ""},
		{"utc", strings.ReplaceAll(valid, updated, "2026-11-07T09:00:00Z"), ""},
		{"lowercase utc", strings.ReplaceAll(valid, updated, "2026-11-07t09:00:00z"), ""},
		{"lowercase offset", strings.ReplaceAll(valid, updated, "2026-11-07t18:00:00+09:00"), ""},
		{"maximum offset", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00+23:59"), ""},
		{"negative offset", strings.ReplaceAll(valid, updated, "2026-11-07T03:30:00-05:30"), ""},
		{"fraction", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00.123+09:00"), ""},
		{"zero time", strings.ReplaceAll(valid, updated, "0001-01-01T00:00:00Z"), ""},
		{"closing at eof", strings.TrimSuffix(valid, "\nEvidence"), ""},
		{"crlf closing at eof", strings.ReplaceAll(strings.TrimSuffix(valid, "\nEvidence"), "\n", "\r\n"), ""},
		{"bare cr closing at eof", strings.TrimSuffix(valid, "\nEvidence") + "\r", "frontmatter"},
		{"body is not yaml", valid + "\n[invalid yaml", ""},
		{"missing frontmatter", "Evidence", "frontmatter"},
		{"not at top", "Evidence\n" + valid, "frontmatter"},
		{"leading blank line", "\n" + valid, "frontmatter"},
		{"bom", "\ufeff" + valid, "frontmatter"},
		{"empty frontmatter before provenance", "---\n" + valid, "source_url"},
		{"unclosed", strings.TrimSuffix(valid, "---\nEvidence"), "frontmatter"},
		{"missing source_url", strings.ReplaceAll(valid, "source_url: https://example.com/doc\n", ""), "source_url"},
		{"empty source_url", strings.ReplaceAll(valid, "https://example.com/doc", `""`), "source_url"},
		{"null source_url", strings.ReplaceAll(valid, "https://example.com/doc", "null"), "source_url"},
		{"boolean source_url", strings.ReplaceAll(valid, "https://example.com/doc", "true"), "source_url"},
		{"timestamp source_url", strings.ReplaceAll(valid, "https://example.com/doc", updated), "source_url"},
		{"relative", strings.ReplaceAll(valid, "https://example.com/doc", "/doc"), "source_url"},
		{"credentials", strings.ReplaceAll(valid, "https://example.com/doc", "https://user:pass@example.com/doc"), "source_url"},
		{"http", strings.ReplaceAll(valid, "https:", "http:"), "source_url"},
		{"missing updated_at", strings.ReplaceAll(valid, "updated_at: "+updated+"\n", ""), "updated_at"},
		{"empty updated_at", strings.ReplaceAll(valid, updated, `""`), "updated_at"},
		{"null updated_at", strings.ReplaceAll(valid, updated, "null"), "updated_at"},
		{"invalid updated_at", strings.ReplaceAll(valid, updated, "yesterday"), "updated_at"},
		{"offset required", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00"), "updated_at"},
		{"date only", strings.ReplaceAll(valid, updated, "2026-11-07"), "updated_at"},
		{"invalid calendar date", strings.ReplaceAll(valid, updated, "2026-02-30T18:00:00+09:00"), "updated_at"},
		{"single digit hour", strings.ReplaceAll(valid, updated, "2026-11-07T8:00:00+09:00"), "updated_at"},
		{"comma fraction", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00,123+09:00"), "updated_at"},
		{"offset hour overflow", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00+24:00"), "updated_at"},
		{"offset minute overflow", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00+09:60"), "updated_at"},
		{"boolean updated_at", strings.ReplaceAll(valid, updated, "true"), "updated_at"},
		{"mapping updated_at", strings.ReplaceAll(valid, updated, "{value: now}"), "updated_at"},
		{"injected updated_at", strings.ReplaceAll(valid, updated, `"2026-11-07T18:00:00+09:00\nremove: true"`), "updated_at"},
		{"numeric updated_at", strings.ReplaceAll(valid, updated, "123"), "updated_at"},
		{"list updated_at", strings.ReplaceAll(valid, updated, "["+updated+"]"), "updated_at"},
		{"duplicate updated_at", strings.ReplaceAll(valid, "updated_at: ", "updated_at: "+updated+"\nupdated_at: "), "frontmatter"},
		{"duplicate source_url", strings.ReplaceAll(valid, "source_url: ", "source_url: https://example.com/other\nsource_url: "), "frontmatter"},
		{"sequence frontmatter", "---\n- source_url: https://example.com/doc\n---\nEvidence", "frontmatter"},
		{"duplicate other key", strings.ReplaceAll(valid, "source_url:", "title: first\ntitle: second\nsource_url:"), "frontmatter"},
		{"invalid yaml", "---\nsource_url: [\n---\nEvidence", "frontmatter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := FileWithProvenance("note.md", tc.content)
			if tc.errorKey != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorKey) {
					t.Fatalf("error = %v, want key %s", err, tc.errorKey)
				}
				return
			}
			if err != nil || doc.URL != "https://example.com/doc" {
				t.Fatalf("document = %+v, error = %v", doc, err)
			}
		})
	}
}

func TestLocalInboxBodyKey(t *testing.T) {
	const key = "kioku:local/5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	for _, tc := range []struct{ header, body, want string }{
		{"updated_at: 2026-11-21T00:00:00+09:00\n", "hello\n", key},
		{"updated_at: 2026-11-26T00:00:00Z\ntitle: New title\n", "hello\n", key},
		{"updated_at: 2026-11-26T00:00:00Z\n", "hello\r\n", "kioku:local/cd2eca3535741f27a8ae40c31b0c41d4057a7a7b912b33b9aed86485d1c84676"},
		{"updated_at: 2026-11-26T00:00:00Z\n", " \r\n", ""},
	} {
		content := "---\nsource_url: kioku:local\n" + tc.header + "---\n" + tc.body
		doc, saved, err := InboxFile("notes.md", content, time.Time{})
		if tc.want == "" {
			if err == nil {
				t.Fatal("accepted whitespace-only body")
			}
		} else if err != nil || doc.URL != tc.want || !strings.HasSuffix(saved, "---\n"+tc.body) {
			t.Fatalf("got %q, %q, %v, want %s", doc.URL, saved, err, tc.want)
		}
	}
}

func TestHTTPSIdentityDoesNotCheckNetworkDestination(t *testing.T) {
	const u = "https://127.0.0.1:8443/private"
	doc, err := FileWithProvenance("notes.md", "---\nsource_url: "+u+"\nupdated_at: 2026-11-28T00:00:00Z\n---\nBody")
	if err != nil || doc.URL != u {
		t.Fatal(doc, err)
	}
}

func TestLocalFileFrontmatterOrMarkdownRules(t *testing.T) {
	for _, tc := range []struct{ name, front, body string }{
		{"empty", "---\n---\n", "hello\n"},
		{"empty CRLF", "---\r\n---\r\n", "hello\r\n"},
		{"comments are not a mapping", "", "---\n# Notes\n---\nhello\n"},
		{"scalar is not a mapping", "", "---\nNotes\n---\nhello\n"},
		{"sequence is not a mapping", "", "---\n- Notes\n---\nhello\n"},
		{"invalid YAML", "", "---\ntitle: [Notes\n---\nhello\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := LocalFileContent("notes.md", tc.front+tc.body, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			doc, _, err := InboxFile("notes.md", content, time.Time{})
			want := fmt.Sprintf("%s/%x", LocalSource, sha256.Sum256([]byte(tc.body)))
			if err != nil || doc.URL != want || !strings.HasSuffix(content, "---\n"+tc.body) {
				t.Fatalf("want %s and body %q, got %s and %q: %v", want, tc.body, doc.URL, content, err)
			}
		})
	}
}

func TestProvenanceRewritePreservesReferencesToReplacedValues(t *testing.T) {
	const input = "---\nsource_url: &identity kioku:local\nupdated_at: &old {x: &nested [one, two]}\nsource_copy: *identity\nlist_copy: *nested\nmap_copy: *old\nagain: *old\n---\nBody\n"
	stamp := time.Date(2026, 11, 28, 13, 0, 0, 0, time.UTC)
	local, err := LocalFileContent("notes.md", input, stamp)
	if err != nil {
		t.Fatal(err)
	}
	doc, inbox, err := InboxFile("notes.md", input, stamp)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ content, key string }{{local, LocalSource}, {inbox, doc.URL}} {
		meta, err := Frontmatter(tc.content)
		if err != nil {
			t.Fatal(tc.content, err)
		}
		var values map[string]any
		if err := meta.Decode(&values); err != nil {
			t.Fatal(tc.content, err)
		}
		for key, want := range map[string]string{
			"source_url": tc.key, "source_copy": LocalSource, "updated_at": stamp.Format(time.RFC3339),
			"list_copy": "[one two]", "map_copy": "map[x:[one two]]", "again": "map[x:[one two]]",
		} {
			if got := fmt.Sprint(values[key]); got != want {
				t.Fatalf("%s: %q, want %q\n%s", key, got, want, tc.content)
			}
		}
	}
	for _, invalid := range []string{"source_url: {nested: &identity kioku:local}", "identity: &identity kioku:local\nsource_url: *identity"} {
		if _, err := LocalFileContent("notes.md", "---\n"+invalid+"\n---\nBody", stamp); !errors.Is(err, ErrLocalFileSource) {
			t.Fatalf("local file accepted non-string source_url %q: %v", invalid, err)
		}
	}
}

func TestProvenanceRewriteKeepsUnrelatedAliases(t *testing.T) {
	const input = "---\ntitle: &title Before\noriginal_title: *title\n---\nBody\n"
	content, err := LocalFileContent("notes.md", input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, stored, err := InboxFile("notes.md", content, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := Frontmatter(stored)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := meta.Decode(&values); err != nil || values["title"] != "Before" || values["original_title"] != "Before" {
		t.Fatal(values, err, stored)
	}
}
