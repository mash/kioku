package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/store"
)

type storageTestObjects struct {
	blob.Store
	put  func(context.Context, blob.Key, []byte) error
	del  func(context.Context, blob.Key) error
	list func(context.Context, string) ([]blob.Key, error)
}

func (b *storageTestObjects) Put(ctx context.Context, key blob.Key, body []byte) error {
	if b.put != nil {
		if err := b.put(ctx, key, body); err != nil {
			return err
		}
	}
	return b.Store.Put(ctx, key, body)
}
func (b *storageTestObjects) Delete(ctx context.Context, key blob.Key) error {
	if b.del != nil {
		if err := b.del(ctx, key); err != nil {
			return err
		}
	}
	return b.Store.Delete(ctx, key)
}
func (b *storageTestObjects) List(ctx context.Context, prefix string) ([]blob.Key, error) {
	if b.list != nil {
		return b.list(ctx, prefix)
	}
	return b.Store.List(ctx, prefix)
}
func storageFixture(t *testing.T) (*storageOperations, *storageTestObjects) {
	d, _ := sqlAuthFixture(t)
	objects := &storageTestObjects{Store: local.Store{Dir: t.TempDir()}}
	o := newStorageOperations(d, objects, &Config{MaxWikiBytes: 64 << 20, GenerateDebounce: 10 * time.Second, AgentJobTimeout: time.Hour, AgentMaxSessions: 4, AgentMaxSessionCents: 1000})
	o.now = func() time.Time { return time.UnixMilli(1000).UTC() }
	return o, objects
}
func TestInputPutBeforeRunCommit(t *testing.T) {
	t.Parallel()
	o, objects := storageFixture(t)
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if err := q.PutJob(store.JobRow{ID: "j", ProjectID: "p", Status: "queued", Kind: "generate", Data: store.JobData{UserID: "u"}}); err != nil {
			return err
		}
		return q.PutInbox(store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", Data: store.InboxData{Submitter: "u"}})
	})
	objects.put = func(ctx context.Context, key blob.Key, _ []byte) error {
		if !strings.HasSuffix(string(key), "input.tar.gz") {
			return nil
		}
		// A writer can run during PUT; the upload holds no SQL transaction.
		return o.db.Write(ctx, func(q *store.Queries) error {
			j, err := q.Job("j")
			if err != nil {
				return err
			}
			i, err := q.Inbox("i")
			if err != nil {
				return err
			}
			if j.Status != "queued" || j.Data.Run != nil || i.RunID != nil {
				t.Fatal("Run committed before input PUT")
			}
			return nil
		})
	}
	job, err := o.prepareInput(t.Context(), "j", false, func(context.Context, storageInput) ([]byte, error) { return []byte("archive"), nil })
	if err != nil || job.Status != "running" || !job.InputReady || job.Data.Run == nil {
		t.Fatal(job, err)
	}
	if err = o.db.Read(t.Context(), func(q *store.Queries) error {
		i, err := q.Inbox("i")
		if err == nil && (i.RunID == nil || *i.RunID != "j") {
			t.Fatal("assignment not atomic")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestInboxPutCommitsOnlyAfterObjectSuccess(t *testing.T) {
	t.Parallel()
	o, objects := storageFixture(t)
	objects.put = func(context.Context, blob.Key, []byte) error { return errors.New("object store unavailable") }
	item := store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", Data: store.InboxData{Submitter: "u"}}
	if err := o.putInbox(t.Context(), item, []byte("body")); err == nil {
		t.Fatal("PUT failure ignored")
	}
	if err := o.db.Read(t.Context(), func(q *store.Queries) error {
		items, err := q.ProjectInbox("p")
		if len(items) != 0 {
			t.Fatal("failed body referenced")
		}
		p, e := q.Project("p")
		if e != nil {
			return e
		}
		if p.NextGenerateAt != nil {
			t.Fatal("failed PUT changed deadline")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInputPreparationTimeoutBecomesTerminal(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"build", "put"} {
		t.Run(stage, func(t *testing.T) {
			o, objects := storageFixture(t)
			sqlWrite(t, o.db, func(q *store.Queries) error {
				if err := q.PutJob(store.JobRow{ID: "j", ProjectID: "p", Status: "queued", Kind: "generate", Data: store.JobData{UserID: "u", WikiStatus: store.NullableString("pending")}}); err != nil {
					return err
				}
				return q.PutInbox(store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", Data: store.InboxData{Submitter: "u"}})
			})
			if stage == "put" {
				objects.put = func(ctx context.Context, _ blob.Key, _ []byte) error {
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("unbounded upload")
					}
					return context.DeadlineExceeded
				}
			}
			_, err := o.prepareInput(t.Context(), "j", false, func(ctx context.Context, _ storageInput) ([]byte, error) {
				if stage == "build" {
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("unbounded build")
					}
					return nil, context.DeadlineExceeded
				}
				return []byte("archive"), nil
			})
			if !errors.Is(err, errPreparationRecorded) {
				t.Fatalf("preparation error = %v", err)
			}
			j := readSQLJob(t, o, "j")
			if j.Status != "failed" || store.ValueString(j.Data.WikiStatus) != "failed" || store.ValueString(j.Data.Error) != context.DeadlineExceeded.Error() || j.Data.Run != nil || j.InputReady {
				t.Fatalf("timed-out preparation was left retryable: %+v", j)
			}
			fixtureCheck(t, o.db.Read(t.Context(), func(q *store.Queries) error {
				items, err := q.AvailableInbox("p", false)
				if err != nil {
					return err
				}
				usage, err := q.Usage("j")
				if len(items) != 1 || !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("unstarted job assigned input or recorded usage: items=%v usage=%v", items, usage)
				}
				return nil
			}))
		})
	}
}

func TestInputPreparationCancellationKeepsJobQueued(t *testing.T) {
	t.Parallel()
	o, _ := storageFixture(t)
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if err := q.PutJob(store.JobRow{ID: "j", ProjectID: "p", Status: "queued", Kind: "generate", Data: store.JobData{UserID: "u"}}); err != nil {
			return err
		}
		return q.PutInbox(store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", Data: store.InboxData{Submitter: "u"}})
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := o.prepareInput(ctx, "j", false, func(context.Context, storageInput) ([]byte, error) {
		cancel()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("preparation error = %v", err)
	}
	j := readSQLJob(t, o, "j")
	if j.Status != "queued" || j.Data.Run != nil || j.InputReady {
		t.Fatalf("shutdown cancellation changed unstarted job: %+v", j)
	}
}

func readFixtureWikiFile(ctx context.Context, o *storageOperations, path string) (string, error) {
	s := &Server{Store: o.db, Blobs: o.objects, storage: o}
	rev, unlock, err := s.wikiFiles(ctx, "u", "p")
	if err != nil {
		return "", err
	}
	defer unlock()
	return s.wikiFile(ctx, rev, path)
}
