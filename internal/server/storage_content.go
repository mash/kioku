package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

// storageOperations owns the boundaries between SQL and immutable content.
// Each method uses short transactions and keeps the project gate across object
// selection, IO, and the conditional reference commit.
type storageOperations struct {
	db      catalog
	objects blob.Store
	config  *Config
	now     func() time.Time
	id      func() string
	gates   sync.Map
}

func newStorageOperations(db catalog, objects blob.Store, c *Config) *storageOperations {
	return &storageOperations{db: db, objects: objects, config: c, now: func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }, id: func() string { return auth.Token()[:20] }}
}
func (o *storageOperations) lock(ctx context.Context, project string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gate, _ := o.gates.LoadOrStore(project, make(chan struct{}, 1))
	ch := gate.(chan struct{})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	}
}

func equalOptional(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func sqlJobKey(j store.JobRow, name string) blob.Key {
	return blob.Key("projects/" + j.ProjectID + "/.kioku/jobs/" + j.ID + "/" + name)
}
func sqlInboxKey(i store.InboxRow) blob.Key {
	return blob.Key("projects/" + i.ProjectID + "/.kioku/inbox/" + i.ID + ".md")
}
func wikiBytes(files []store.WikiFileRow) (int64, error) {
	var total int64
	for _, f := range files {
		sum := model.SumMeasurements(&total, &f.Data.Bytes)
		if sum == nil {
			return 0, errors.New("invalid wiki capacity")
		}
		total = *sum
	}
	return total, nil
}
func wikiCapacity(q *store.Queries, p store.ProjectRow) (int64, error) {
	files, err := q.ProjectWiki(p.ID)
	if err != nil {
		return 0, err
	}
	return wikiBytes(files)
}
func (o *storageOperations) putInbox(ctx context.Context, item store.InboxRow, body []byte, commit ...mutationHook) error {
	unlock, err := o.lock(ctx, item.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	check := func(q *store.Queries) error {
		for _, hook := range commit {
			if hook.check != nil {
				if err := hook.check(q); err != nil {
					return err
				}
			}
		}
		p, err := sqlMember(q, item.ProjectID, item.Data.Submitter)
		if err != nil {
			return err
		}
		size, err := wikiCapacity(q, p)
		if err != nil {
			return err
		}
		if !item.Remove && size > o.config.MaxWikiBytes {
			return fail(413, "published wiki exceeds max-wiki-bytes; remove sources first")
		}
		count, err := q.InboxCountExceptReplacement(item.ProjectID, item.SourceURL)
		if err != nil {
			return err
		}
		if count >= maxInboxItems {
			return fail(429, "project inbox is full")
		}
		return nil
	}
	if !item.Remove {
		if err = o.db.Read(ctx, check); err != nil {
			return err
		}
		if err = o.objects.Put(ctx, sqlInboxKey(item), body); err != nil {
			return err
		}
	}
	return o.db.Write(ctx, func(q *store.Queries) error {
		if err := check(q); err != nil {
			return err
		}
		if err := q.ReplaceInbox(item); err != nil {
			return err
		}
		p, err := q.Project(item.ProjectID)
		if err != nil {
			return err
		}
		due := o.now().Add(o.config.GenerateDebounce).UnixMilli()
		p.NextGenerateAt = &due
		if err := q.PutProject(p); err != nil {
			return err
		}
		for _, hook := range commit {
			if hook.audit != nil {
				if err := hook.audit(q); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func sqlJobAuthorization(q *store.Queries, j store.JobRow, now int64) error {
	if _, err := sqlMember(q, j.ProjectID, j.Data.UserID); err != nil {
		return err
	}
	if j.Data.AgentID == nil {
		return nil
	}
	agents, err := q.ProjectUserSessions(j.ProjectID, j.Data.UserID, "agent")
	if err != nil {
		return err
	}
	for _, v := range agents {
		a, err := store.SessionData[store.AgentSessionData](v)
		if err != nil {
			return err
		}
		if a.AgentID != nil && *a.AgentID == *j.Data.AgentID && v.UserID == j.Data.UserID && v.Active(now) && store.ValueString(a.Role) == "write" {
			return nil
		}
	}
	return fail(403, "generation authorization was revoked")
}
func (o *storageOperations) runningJob(q *store.Queries, id string) (store.JobRow, error) {
	j, err := q.Job(id)
	if err != nil {
		return j, err
	}
	if !j.Running() {
		return j, fail(409, "generation job was canceled")
	}
	if j.Data.Run.AbortReason != nil {
		return j, fail(403, "generation authorization was revoked")
	}
	if err = sqlJobAuthorization(q, j, o.now().UnixMilli()); err != nil {
		return j, err
	}
	return checkJobParent(q, j)
}
func checkJobParent(q *store.Queries, j store.JobRow) (store.JobRow, error) {
	if j.ParentID != nil {
		p, err := q.Project(j.ProjectID)
		if err != nil {
			return j, err
		}
		parent, err := q.Job(*j.ParentID)
		if err != nil {
			return j, err
		}
		if j.Data.ViewTask == nil || parent.Status != "running" || parent.ProjectID != j.ProjectID || parent.GenerationSnapshot == nil || !equalOptional(p.WikiRevision, parent.GenerationSnapshot) {
			return j, fail(409, "Wiki または生成ジョブが変更されました")
		}
	}
	return j, nil
}

// outputJob reuses an authorized row from this transaction. Parent linkage is
// independent of token validation and must still be checked.
func (o *storageOperations) outputJob(q *store.Queries, id string, authorize func(*store.Queries) (store.JobRow, error)) (store.JobRow, error) {
	if authorize == nil {
		return o.runningJob(q, id)
	}
	j, err := authorize(q)
	if err != nil {
		return j, err
	}
	return checkJobParent(q, j)
}

type mutationHook struct{ check, audit func(*store.Queries) error }

func (h mutationHook) apply(q *store.Queries, fn func(*store.Queries) error) error {
	if h.check != nil {
		if err := h.check(q); err != nil {
			return err
		}
	}
	if err := fn(q); err != nil {
		return err
	}
	if h.audit != nil {
		return h.audit(q)
	}
	return nil
}
func (h mutationHook) write(ctx context.Context, db catalog, fn func(*store.Queries) error) error {
	return db.Write(ctx, func(q *store.Queries) error { return h.apply(q, fn) })
}
