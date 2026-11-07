package source

import "testing"

func TestFileProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, content, url, want string
		valid                    bool
	}{
		{"explicit", "Evidence", "https://example.com/doc", "https://example.com/doc", true},
		{"frontmatter", "---\nsource_url: https://example.com/doc\n---\nEvidence", "", "https://example.com/doc", true},
		{"crlf", "---\r\nsource_url: https://example.com/doc\r\n---\r\nEvidence", "", "https://example.com/doc", true},
		{"override", "---\nsource_url: https://example.com/old\n---\nEvidence", "https://example.com/new", "https://example.com/new", true},
		{"missing", "Evidence", "", "", false},
		{"relative", "Evidence", "/doc", "", false},
		{"credentials", "Evidence", "https://user:pass@example.com/doc", "", false},
		{"invalid yaml", "---\nsource_url: [\n---\nEvidence", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := FileWithURL("note.md", tc.content, tc.url)
			if (err == nil) != tc.valid {
				t.Fatalf("error = %v", err)
			}
			if tc.valid && (doc.URL != tc.want || doc.Text != tc.content) {
				t.Fatalf("document = %+v", doc)
			}
		})
	}
}
