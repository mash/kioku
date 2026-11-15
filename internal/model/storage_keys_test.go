package model

import (
	"encoding/json"
	"testing"

	"github.com/mash/kioku/internal/blob"
)

func TestRevisionKeysAreJSONStringValues(t *testing.T) {
	key := WikiContentKey("p", HashContent([]byte("body")))
	revision := Revision{Files: map[string]blob.Key{"wiki/index.md": key}}
	raw, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Revision
	if err = json.Unmarshal(raw, &roundtrip); err != nil || roundtrip.Files["wiki/index.md"] != key {
		t.Fatal(roundtrip, err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if got := fields["files"].(map[string]any)["wiki/index.md"]; got != string(key) {
		t.Fatal(got)
	}
}
