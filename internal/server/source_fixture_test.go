package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

// seedSource prepares the old generator's storage while the inbox API is
// introduced separately. PR-3 replaces this fixture with inbox input.
func (f *fixture) seedSource(_, path, token string, input, out any) int {
	req := input.(model.PutRequest)
	file := req.File
	doc, err := source.FileWithURL(file.Name, file.Content, file.SourceURL)
	if err != nil {
		return 400
	}
	r := httptest.NewRequest("POST", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	_, session := f.s.session(r)
	pid := strings.Split(path, "/")[3]
	job, err := f.s.storeSources(context.Background(), []source.Document{doc}, model.Job{ID: auth.Token()[:20], Kind: "put", ProjectID: pid, UserID: session.UserID, AgentID: session.AgentID, Created: time.Now().UTC()})
	if err != nil {
		if e, ok := err.(apiError); ok {
			return e.status
		}
		return 500
	}
	if out != nil {
		b, _ := json.Marshal(job)
		json.Unmarshal(b, out)
	}
	return 202
}

func testSourceURL(name, body string) string {
	return "https://example.com/" + auth.Hash(name+"\n"+body)
}
