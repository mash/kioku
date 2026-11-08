package source

import (
	"strings"
	"testing"
	"time"
)

func TestFileProvenance(t *testing.T) {
	const updated = "2026-11-07T18:00:00+09:00"
	valid := "---\nsource_url: https://example.com/doc\nupdated_at: " + updated + "\n---\nEvidence"
	for _, tc := range []struct {
		name, content, wantTime, errorKey string
	}{
		{"frontmatter", valid, updated, ""},
		{"quoted", strings.ReplaceAll(valid, updated, `"`+updated+`"`), updated, ""},
		{"crlf", strings.ReplaceAll(valid, "\n", "\r\n"), updated, ""},
		{"utc", strings.ReplaceAll(valid, updated, "2026-11-07T09:00:00Z"), "2026-11-07T09:00:00Z", ""},
		{"negative offset", strings.ReplaceAll(valid, updated, "2026-11-07T03:30:00-05:30"), "2026-11-07T03:30:00-05:30", ""},
		{"fraction", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00.123+09:00"), "2026-11-07T18:00:00.123+09:00", ""},
		{"zero time", strings.ReplaceAll(valid, updated, "0001-01-01T00:00:00Z"), "0001-01-01T00:00:00Z", ""},
		{"closing at eof", strings.TrimSuffix(valid, "\nEvidence"), updated, ""},
		{"missing frontmatter", "Evidence", "", "source_url"},
		{"not at top", "Evidence\n" + valid, "", "source_url"},
		{"unclosed", strings.TrimSuffix(valid, "---\nEvidence"), "", "source_url"},
		{"missing source_url", strings.ReplaceAll(valid, "source_url: https://example.com/doc\n", ""), "", "source_url"},
		{"empty source_url", strings.ReplaceAll(valid, "https://example.com/doc", `""`), "", "source_url"},
		{"relative", strings.ReplaceAll(valid, "https://example.com/doc", "/doc"), "", "source_url"},
		{"credentials", strings.ReplaceAll(valid, "https://example.com/doc", "https://user:pass@example.com/doc"), "", "source_url"},
		{"http", strings.ReplaceAll(valid, "https:", "http:"), "", "source_url"},
		{"private", strings.ReplaceAll(valid, "example.com", "127.0.0.1"), "", "source_url"},
		{"missing updated_at", strings.ReplaceAll(valid, "updated_at: "+updated+"\n", ""), "", "updated_at"},
		{"empty updated_at", strings.ReplaceAll(valid, updated, `""`), "", "updated_at"},
		{"null updated_at", strings.ReplaceAll(valid, updated, "null"), "", "updated_at"},
		{"invalid updated_at", strings.ReplaceAll(valid, updated, "yesterday"), "", "updated_at"},
		{"offset required", strings.ReplaceAll(valid, updated, "2026-11-07T18:00:00"), "", "updated_at"},
		{"date only", strings.ReplaceAll(valid, updated, "2026-11-07"), "", "updated_at"},
		{"invalid calendar date", strings.ReplaceAll(valid, updated, "2026-02-30T18:00:00+09:00"), "", "updated_at"},
		{"numeric updated_at", strings.ReplaceAll(valid, updated, "123"), "", "updated_at"},
		{"list updated_at", strings.ReplaceAll(valid, updated, "["+updated+"]"), "", "updated_at"},
		{"duplicate updated_at", strings.ReplaceAll(valid, "updated_at: ", "updated_at: "+updated+"\nupdated_at: "), "", "updated_at"},
		{"duplicate source_url", strings.ReplaceAll(valid, "source_url: ", "source_url: https://example.com/other\nsource_url: "), "", "source_url"},
		{"invalid yaml", "---\nsource_url: [\n---\nEvidence", "", "source_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := FileWithProvenance("note.md", tc.content)
			if tc.errorKey != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorKey) {
					t.Fatalf("error = %v, want key %s", err, tc.errorKey)
				}
				return
			}
			if err != nil || doc.URL != "https://example.com/doc" || doc.UpdatedAt.Format(time.RFC3339Nano) != tc.wantTime {
				t.Fatalf("document = %+v, error = %v", doc, err)
			}
		})
	}
}
