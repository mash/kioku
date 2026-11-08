package server

import "github.com/mash/kioku/internal/auth"

func testSourceURL(name, body string) string {
	return "https://example.com/" + auth.Hash(name+"\n"+body)
}

const testUpdatedAt = "2026-11-07T18:00:00+09:00"

func testSourceContent(url, body string) string {
	return "---\nsource_url: " + url + "\nupdated_at: " + testUpdatedAt + "\n---\n" + body
}
