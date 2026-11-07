package server

import "github.com/mash/kioku/internal/auth"

func testSourceURL(name, body string) string {
	return "https://example.com/" + auth.Hash(name+"\n"+body)
}
