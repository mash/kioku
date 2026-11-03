package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type receipts struct {
	Hashes  map[string]string `json:"hashes"`
	LastJob string            `json:"last_job,omitempty"`
}

func (a *App) readReceipts() (receipts, error) {
	r := receipts{Hashes: map[string]string{}}
	b, err := os.ReadFile(filepath.Join(a.metadataDir(), "receipts.json"))
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	if r.Hashes == nil {
		r.Hashes = map[string]string{}
	}
	return r, err
}
