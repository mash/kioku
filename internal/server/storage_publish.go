package server

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
)

// publishWiki holds the generation gate before the project gate. No SQL
// transaction spans a content upload; every page remains readable on failure.
func (o *storageOperations) publishWiki(ctx context.Context, id string, files map[string]string) (string, error) {
	var initial store.JobRow
	if err := o.db.Read(ctx, func(q *store.Queries) error { var err error; initial, err = q.Job(id); return err }); err != nil {
		return "", err
	}
	unlockGeneration, err := o.lock(ctx, "generate:"+initial.ProjectID)
	if err != nil {
		return "", err
	}
	defer unlockGeneration()
	return o.publishWikiLocked(ctx, initial, files)
}
func (o *storageOperations) publishWikiLocked(ctx context.Context, initial store.JobRow, files map[string]string) (string, error) {
	id := initial.ID
	unlock, err := o.lock(ctx, initial.ProjectID)
	if err != nil {
		return "", err
	}
	defer unlock()
	names := make([]string, 0, len(files))
	for name := range files {
		if !wiki.ValidFile(name) {
			return "", errors.New("invalid generated filename")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	rev, started := o.id(), o.now().UnixMilli()
	if err = o.db.Write(ctx, func(q *store.Queries) error {
		j, e := o.runningJob(q, id)
		if e != nil {
			return e
		}
		if j.ParentID != nil || j.GenerationSnapshot != nil {
			return fail(409, "job no longer accepts Wiki publication")
		}
		p, e := q.Project(j.ProjectID)
		if e != nil {
			return e
		}
		current, e := q.ProjectWiki(p.ID)
		if e != nil {
			return e
		}
		if p.Data.WikiCompleteBytes == nil {
			size, e := wikiBytes(current)
			if e != nil {
				return e
			}
			p.Data.WikiCompleteBytes = &size
		}
		if !p.WikiIncomplete && p.Data.WikiCompleteRevision == nil {
			p.Data.WikiCompleteRevision = p.WikiRevision
		}
		p.WikiIncomplete = true
		j.Phase = nullableString("finalizing")
		if e = q.PutProject(p); e != nil {
			return e
		}
		return q.PutJob(j)
	}); err != nil {
		return "", err
	}
	for _, name := range names {
		body := []byte(files[name])
		hash := string(model.HashContent(body))
		changed := true
		if err = o.db.Read(ctx, func(q *store.Queries) error {
			if _, e := o.runningJob(q, id); e != nil {
				return e
			}
			f, e := q.WikiFile(initial.ProjectID, name)
			if errors.Is(e, sql.ErrNoRows) {
				return nil
			}
			if e != nil {
				return e
			}
			changed = f.Hash != hash
			return nil
		}); err != nil {
			return "", err
		}
		if changed {
			if err = o.objects.Put(ctx, model.WikiContentKey(initial.ProjectID, model.ContentHash(hash)), body); err != nil {
				return "", err
			}
		}
		if err = o.db.Write(ctx, func(q *store.Queries) error {
			j, e := o.runningJob(q, id)
			if e != nil {
				return e
			}
			p, e := q.Project(j.ProjectID)
			if e != nil {
				return e
			}
			if e = q.PutWikiFile(store.WikiFileRow{ProjectID: p.ID, Path: name, Hash: hash, Data: store.WikiFileData{Bytes: int64(len(body))}}); e != nil {
				return e
			}
			p.WikiRevision = &rev
			if changed {
				now := o.now().UnixMilli()
				p.Data.WikiUpdatedAt = &now
			}
			return q.PutProject(p)
		}); err != nil {
			return "", err
		}
	}
	err = o.db.Write(ctx, func(q *store.Queries) error {
		j, e := o.runningJob(q, id)
		if e != nil {
			return e
		}
		p, e := q.Project(j.ProjectID)
		if e != nil {
			return e
		}
		current, e := q.ProjectWiki(p.ID)
		if e != nil {
			return e
		}
		for _, f := range current {
			if _, ok := files[f.Path]; !ok {
				if e = q.DeleteWikiFile(p.ID, f.Path); e != nil {
					return e
				}
			}
		}
		items, e := q.ProjectInbox(p.ID)
		if e != nil {
			return e
		}
		removed := false
		for _, item := range items {
			if valueString(item.RunID) == id {
				removed = removed || item.Remove
				if e = q.DeleteInbox(item.ID); e != nil {
					return e
				}
			}
		}
		if removed {
			epoch := o.id()
			p.ViewEpoch = &epoch
			views, e := q.ProjectViews(p.ID)
			if e != nil {
				return e
			}
			for _, v := range views {
				v.BundleHash = nil
				v.ViewEpoch = nil
				v.Data.JobID = nil
				v.Data.WikiSnapshotID = nil
				v.Data.GeneratedAt = nil
				if e = q.PutView(v); e != nil {
					return e
				}
			}
		}
		now := o.now().UnixMilli()
		p.WikiRevision = &rev
		p.Data.WikiCompleteRevision = &rev
		p.Data.WikiCompleteBytes = nil
		p.Data.WikiCreated = &started
		p.Data.WikiUpdatedAt = &now
		p.WikiIncomplete = false
		if e = q.PutProject(p); e != nil {
			return e
		}
		j.Status = "completed"
		j.Data.Revision = &rev
		if e = beginSQLViews(q, &j, p, j.Data.Run.BudgetCents); e != nil {
			return e
		}
		if e = q.PutJob(j); e != nil {
			return e
		}
		return q.CompactHistory(p.ID, nil)
	})
	return rev, err
}
func (o *storageOperations) publishView(ctx context.Context, id string, bundle *viewBundle) error {
	archive, err := bundle.Archive()
	if err != nil {
		return err
	}
	var initial store.JobRow
	if err = o.db.Read(ctx, func(q *store.Queries) error { var e error; initial, e = q.Job(id); return e }); err != nil {
		return err
	}
	unlock, err := o.lock(ctx, initial.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	var j store.JobRow
	check := func(q *store.Queries) error {
		var e error
		j, e = o.runningJob(q, id)
		if e != nil {
			return e
		}
		if j.Data.ViewTask == nil {
			return fail(409, "not a View generation job")
		}
		return nil
	}
	if err = o.db.Read(ctx, check); err != nil {
		return err
	}
	hash := string(model.HashContent(archive))
	if err = o.objects.Put(ctx, model.ViewContentKey(j.ProjectID, j.Data.ViewTask.ViewID, model.ContentHash(hash)), archive); err != nil {
		return err
	}
	return o.db.Write(ctx, func(q *store.Queries) error {
		if e := check(q); e != nil {
			return e
		}
		p, e := q.Project(j.ProjectID)
		if e != nil {
			return e
		}
		v, e := q.View(p.ID, j.Data.ViewTask.ViewID)
		if e != nil {
			return e
		}
		now := o.now().UnixMilli()
		v.BundleHash = &hash
		v.ViewEpoch = p.ViewEpoch
		v.Data.Status = nullableString("completed")
		v.Data.Error = nil
		v.Data.JobID = j.ParentID
		v.Data.WikiSnapshotID = &j.Data.ViewTask.Snapshot
		v.Data.GeneratedAt = &now
		if e = q.PutView(v); e != nil {
			return e
		}
		j.Status = "completed"
		j.Data.Error = nil
		if e = q.PutJob(j); e != nil {
			return e
		}
		return q.CompactHistory(j.ProjectID, nil)
	})
}
