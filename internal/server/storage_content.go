package server

import (
	"context"
	"database/sql"
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

func valueBool(b *bool) bool { return b != nil && *b }
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
		files, err := q.ProjectWiki(item.ProjectID)
		if err != nil {
			return err
		}
		size, err := wikiBytes(files)
		if err != nil {
			return err
		}
		if p.Data.WikiCompleteBytes != nil {
			size = *p.Data.WikiCompleteBytes
		}
		if !item.Remove && size > o.config.MaxWikiBytes {
			return fail(413, "published wiki exceeds max-wiki-bytes; remove sources first")
		}
		items, err := q.ProjectInbox(item.ProjectID)
		if err != nil {
			return err
		}
		count := 0
		for _, v := range items {
			if v.RunID != nil || v.SourceURL != item.SourceURL {
				count++
			}
		}
		if count >= maxInboxItems {
			return fail(429, "project inbox is full")
		}
		return nil
	}
	if err = o.db.Read(ctx, check); err != nil {
		return err
	}
	if !item.Remove {
		if err = o.objects.Put(ctx, sqlInboxKey(item), body); err != nil {
			return err
		}
	}
	err = o.db.Write(ctx, func(q *store.Queries) error {
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
	var unknown *store.UnknownCommit
	if !errors.As(err, &unknown) {
		return err
	}
	applied := false
	var declined error
	if e := o.db.ResolveCommit(ctx, func(q *store.Queries) error {
		_, e := q.Inbox(item.ID)
		if e == nil {
			applied = true
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		e = check(q)
		var api apiError
		if errors.As(e, &api) {
			declined = e
			return nil
		}
		return e
	}); e != nil {
		return e
	}
	if applied {
		return nil
	}
	if declined != nil {
		return declined
	}
	return fail(503, "inbox commit was not applied; retry the request")
}
func sqlJobAuthorization(q *store.Queries, j store.JobRow, now int64) error {
	if _, err := sqlMember(q, j.ProjectID, j.Data.UserID); err != nil {
		return err
	}
	if j.Data.AgentID == nil {
		return nil
	}
	agents, err := q.ProjectSessions(j.ProjectID, "agent")
	if err != nil {
		return err
	}
	for _, v := range agents {
		a, err := store.SessionData[store.AgentSessionData](v)
		if err != nil {
			return err
		}
		if a.AgentID != nil && *a.AgentID == *j.Data.AgentID && v.UserID == j.Data.UserID && v.Active(now) && a.RevokedAt == nil && store.ValueString(a.Role) == "write" {
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
	if j.ParentID != nil {
		p, err := q.Project(j.ProjectID)
		if err != nil {
			return j, err
		}
		parent, err := q.Job(*j.ParentID)
		if err != nil {
			return j, err
		}
		if j.Data.ViewTask == nil || parent.Status != "running" || parent.ChildID == nil || *parent.ChildID != id || p.WikiIncomplete || store.ValueString(p.WikiRevision) != j.Data.ViewTask.Snapshot {
			return j, fail(409, "Wiki または生成ジョブが変更されました")
		}
	}
	return j, nil
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
