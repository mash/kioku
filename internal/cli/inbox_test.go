package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestPutCheckpointsEachAcceptedFile(t *testing.T) {
	writes := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req model.PutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.File == nil {
			t.Error("missing single file")
		}
		writes++
		if writes == 2 {
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"unavailable"}`))
			return
		}
		w.WriteHeader(202)
		w.Write([]byte(`{"id":"accepted","status":"fetched"}`))
	}))
	defer ts.Close()
	a := &App{Dir: t.TempDir(), Config: Config{Server: ts.URL, Project: "p", Namespace: "org/p"}, Client: ts.Client(), Out: io.Discard, Err: io.Discard, ctx: context.Background()}
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(a.Dir, "inbox", name), []byte("---\nsource_url: https://example.com/"+name+"\n---\nBody"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.put(nil); err == nil {
		t.Fatal("second failure hidden")
	}
	r, err := a.readReceipts()
	if err != nil || len(r.Hashes) != 1 {
		t.Fatal(r, err)
	}
	if err := a.put(nil); err != nil {
		t.Fatal(err)
	}
	if writes != 3 {
		t.Fatal("accepted file resubmitted", writes)
	}
	r, err = a.readReceipts()
	if err != nil || len(r.Hashes) != 2 {
		t.Fatal(r, err)
	}
}
