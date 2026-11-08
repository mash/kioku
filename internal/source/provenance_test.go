package source

import (
	"strings"
	"testing"
)

func TestFileProvenance(t *testing.T) {
	const updated = "2026-11-07T18:00:00+09:00"
	valid := "---\nsource_url: https://example.com/doc\nupdated_at: " + updated + "\n---\nEvidence"
	for _, tc := range []struct {
		name, content, errorKey string
	}{
		{"frontmatter", valid, ""},
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
		{"missing frontmatter", "Evidence", "source_url"},
		{"not at top", "Evidence\n" + valid, "source_url"},
		{"leading blank line", "\n" + valid, "source_url"},
		{"bom", "\ufeff" + valid, "source_url"},
		{"empty frontmatter before provenance", "---\n" + valid, "source_url"},
		{"unclosed", strings.TrimSuffix(valid, "---\nEvidence"), "source_url"},
		{"missing source_url", strings.ReplaceAll(valid, "source_url: https://example.com/doc\n", ""), "source_url"},
		{"empty source_url", strings.ReplaceAll(valid, "https://example.com/doc", `""`), "source_url"},
		{"null source_url", strings.ReplaceAll(valid, "https://example.com/doc", "null"), "source_url"},
		{"boolean source_url", strings.ReplaceAll(valid, "https://example.com/doc", "true"), "source_url"},
		{"timestamp source_url", strings.ReplaceAll(valid, "https://example.com/doc", updated), "source_url"},
		{"relative", strings.ReplaceAll(valid, "https://example.com/doc", "/doc"), "source_url"},
		{"credentials", strings.ReplaceAll(valid, "https://example.com/doc", "https://user:pass@example.com/doc"), "source_url"},
		{"http", strings.ReplaceAll(valid, "https:", "http:"), "source_url"},
		{"private", strings.ReplaceAll(valid, "example.com", "127.0.0.1"), "source_url"},
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
		{"injected updated_at", strings.ReplaceAll(valid, updated, `"2026-11-07T18:00:00+09:00\nstatus: removed"`), "updated_at"},
		{"numeric updated_at", strings.ReplaceAll(valid, updated, "123"), "updated_at"},
		{"list updated_at", strings.ReplaceAll(valid, updated, "["+updated+"]"), "updated_at"},
		{"duplicate updated_at", strings.ReplaceAll(valid, "updated_at: ", "updated_at: "+updated+"\nupdated_at: "), "updated_at"},
		{"duplicate source_url", strings.ReplaceAll(valid, "source_url: ", "source_url: https://example.com/other\nsource_url: "), "source_url"},
		{"invalid yaml", "---\nsource_url: [\n---\nEvidence", "source_url"},
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
