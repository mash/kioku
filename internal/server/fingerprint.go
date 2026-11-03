package server

import (
	"encoding/json"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/wiki"
)

func generationFingerprint(inputs []wiki.Input) string {
	type item struct{ ID, Hash string }
	data := struct {
		Version string
		Sources []item
	}{Version: "wiki-v1"}
	for _, in := range inputs {
		data.Sources = append(data.Sources, item{in.Source.ID, in.Source.ContentHash})
	}
	b, _ := json.Marshal(data)
	return auth.Hash(string(b))
}
