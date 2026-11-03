package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestGenerateFullRebuildsUnchanged(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("alice")
	var p model.Project
	if c := f.request("POST", "/api/projects", token, map[string]string{"name": "full"}, &p); c != 201 {
		t.Fatal(c)
	}
	putFile(t, f, token, p.ID, "note.md", "Evidence")
	var previous string
	for _, full := range []bool{false, true} {
		var job model.Job
		if c := f.request("POST", "/api/projects/"+p.ID+"/generate", token, map[string]bool{"full": full}, &job); c != 202 {
			t.Fatal(c)
		}
		if job.Full != full {
			t.Fatalf("full not persisted: %+v", job)
		}
		rev, err := f.s.process(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		if rev == "" || rev == previous {
			t.Fatalf("full did not rebuild: %q", rev)
		}
		_ = f.db.View(func(st model.State) error {
			if st.Jobs[job.ID].Unchanged {
				t.Error("unexpected no-op")
			}
			return nil
		})
		previous = rev
	}
}
