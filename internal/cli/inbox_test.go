package cli

import (
	"bytes"
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
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	var out bytes.Buffer
	a := &App{Dir: t.TempDir(), Config: Config{Server: ts.URL, Namespace: "org/p"}, Client: ts.Client(), Out: &out, Err: io.Discard, ctx: context.Background()}
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(a.Dir, "inbox", name), []byte(SourceContentForTest("https://example.com/"+name, "Body")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.put(nil); err == nil {
		t.Fatal("second failure hidden")
	}
	if got := out.String(); got != "accepted a.md\n" {
		t.Fatal("failed file reported as accepted", got)
	}
	out.Reset()
	r, err := a.readReceipts()
	if err != nil || len(r.Hashes) != 1 {
		t.Fatal(r, err)
	}
	if err := a.put(nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "accepted b.md\n" {
		t.Fatal("retry output", got)
	}
	if writes != 3 {
		t.Fatal("accepted file resubmitted", writes)
	}
	r, err = a.readReceipts()
	if err != nil || len(r.Hashes) != 2 {
		t.Fatal(r, err)
	}
}
