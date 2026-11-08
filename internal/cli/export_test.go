package cli

const testUpdatedAt = "2026-11-07T18:00:00+09:00"

// SourceContentForTest supplies provenance for both CLI unit and external tests.
func SourceContentForTest(url, body string) string {
	return "---\nsource_url: " + url + "\nupdated_at: " + testUpdatedAt + "\n---\n" + body
}
