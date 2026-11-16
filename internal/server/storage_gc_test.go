package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestGCDoesNothingOnReferenceOrListFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"list", "reference"} {
		t.Run(failure, func(t *testing.T) {
			o, b := storageFixture(t)
			if err := b.Put(t.Context(), "projects/p/.kioku/wiki/orphan.md", []byte("x")); err != nil {
				t.Fatal(err)
			}
			deleted := 0
			b.del = func(context.Context, blob.Key) error { deleted++; return nil }
			if failure == "list" {
				b.list = func(context.Context, string) ([]blob.Key, error) {
					return []blob.Key{"projects/p/.kioku/wiki/orphan.md"}, errors.New("incomplete listing")
				}
			} else {
				if err := o.db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := o.collectGarbage(t.Context(), "p"); err == nil || deleted != 0 {
				t.Fatal(err, deleted)
			}
		})
	}
}
func TestGCProtectsEveryContentReference(t *testing.T) {
	t.Parallel()
	o, b := storageFixture(t)
	j := storageRunning(t, o, "j")
	j.OutputHash = store.NullableString(strings.Repeat("a", 64))
	h := string(model.HashContent([]byte("wiki")))
	vh := string(model.HashContent([]byte("zip")))
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if e := q.PutJob(j); e != nil {
			return e
		}
		if e := q.PutWikiFile(store.WikiFileRow{ProjectID: "p", Path: "wiki/index.md", Hash: h}); e != nil {
			return e
		}
		if e := q.PutView(store.ViewRow{ProjectID: "p", ID: "v", Slug: "map", BundleHash: &vh, Data: sqlTestViewData()}); e != nil {
			return e
		}
		return q.PutInbox(store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "s"})
	})
	live := []blob.Key{model.WikiContentKey("p", model.ContentHash(h)), model.ViewContentKey("p", "v", model.ContentHash(vh)), "projects/p/.kioku/inbox/i.md", sqlJobKey(j, "input.tar.gz"), sqlJobKey(j, "output-"+*j.OutputHash+".tar.gz")}
	for _, key := range live {
		if e := b.Put(t.Context(), key, []byte("data")); e != nil {
			t.Fatal(e)
		}
	}
	orphan := blob.Key("projects/p/.kioku/jobs/j/output-old.tar.gz")
	if e := b.Put(t.Context(), orphan, []byte("old")); e != nil {
		t.Fatal(e)
	}
	n, e := o.collectGarbage(t.Context(), "p")
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	for _, key := range live {
		if _, e := b.Get(t.Context(), key); e != nil {
			t.Fatal(key, e)
		}
	}
}
func TestProjectLockProtectsInputSelectionAndRead(t *testing.T) {
	t.Parallel()
	o, b := storageFixture(t)
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if e := q.PutJob(store.JobRow{ID: "j", ProjectID: "p", Status: "queued", Kind: "generate", Data: store.JobData{UserID: "u"}}); e != nil {
			return e
		}
		return q.PutInbox(store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "s", Data: store.InboxData{Submitter: "u"}})
	})
	selected, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := o.prepareInput(t.Context(), "j", false, func(context.Context, storageInput) ([]byte, error) {
			close(selected)
			<-release
			return []byte("input"), nil
		})
		done <- e
	}()
	<-selected
	// A cancelled lock request proves GC cannot list between selection and PUT.
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	listed := false
	b.list = func(context.Context, string) ([]blob.Key, error) { listed = true; return nil, nil }
	if _, e := o.collectGarbage(ctx, "p"); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if listed {
		t.Fatal("GC entered held gate")
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestProjectDeletionRetainsUsageAndSessions(t *testing.T) {
	t.Parallel()
	o, b := storageFixture(t)
	j := storageRunning(t, o, "j")
	j.Status = "completed"
	j.CleanupComplete = true
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if e := q.PutJob(j); e != nil {
			return e
		}
		if e := q.RecordUsage(store.UsageRow{JobID: "j", ProjectID: "p"}); e != nil {
			return e
		}
		if e := q.InsertSession(store.SessionRow{ID: "agent", UserID: "u", Kind: "agent", Status: "active", ProjectID: store.NullableString("p"), Expires: 999999, Data: json.RawMessage(`{}`)}); e != nil {
			return e
		}
		if e := q.PutWikiFile(store.WikiFileRow{ProjectID: "p", Path: "wiki/index.md", Hash: string(model.HashContent([]byte("x")))}); e != nil {
			return e
		}
		return q.PutView(store.ViewRow{ProjectID: "p", ID: "v", Slug: "map", Data: sqlTestViewData()})
	})
	if e := b.Put(t.Context(), "projects/p/.kioku/wiki/old.md", []byte("x")); e != nil {
		t.Fatal(e)
	}
	b.del = func(ctx context.Context, k blob.Key) error {
		return o.db.Write(ctx, func(q *store.Queries) error {
			p, e := q.Project("p")
			if e != nil {
				return e
			}
			w, e := q.ProjectWiki("p")
			if e != nil {
				return e
			}
			v, e := q.ProjectViews("p")
			if !p.Deleting || len(w) != 0 || len(v) != 0 {
				t.Fatal(p, w, v)
			}
			return e
		})
	}
	if e := o.deleteProject(t.Context(), "p", nil); e != nil {
		t.Fatal(e)
	}
	if e := o.db.Read(t.Context(), func(q *store.Queries) error {
		if _, e := q.Project("p"); !errors.Is(e, sql.ErrNoRows) {
			t.Fatal(e)
		}
		if _, e := q.Job("j"); !errors.Is(e, sql.ErrNoRows) {
			t.Fatal(e)
		}
		if _, e := q.Usage("j"); e != nil {
			return e
		}
		_, e := q.Session("agent")
		return e
	}); e != nil {
		t.Fatal(e)
	}
}
