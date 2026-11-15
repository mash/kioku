package server

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func (o *storageOperations) collectGarbage(ctx context.Context, project string) (int, error) {
	unlock, err := o.lock(ctx, project)
	if err != nil {
		return 0, err
	}
	defer unlock()
	live := map[blob.Key]bool{}
	if err = o.db.Read(ctx, func(q *store.Queries) error {
		files, e := q.ProjectWiki(project)
		if e != nil {
			return e
		}
		for _, f := range files {
			live[model.WikiContentKey(project, model.ContentHash(f.Hash))] = true
		}
		views, e := q.ProjectViews(project)
		if e != nil {
			return e
		}
		for _, v := range views {
			if v.BundleHash != nil {
				live[model.ViewContentKey(project, v.ID, model.ContentHash(*v.BundleHash))] = true
			}
		}
		inbox, e := q.ProjectInbox(project)
		if e != nil {
			return e
		}
		for _, i := range inbox {
			if !i.Remove {
				live[sqlInboxKey(i)] = true
			}
		}
		jobs, e := q.ProjectJobs(project)
		if e != nil {
			return e
		}
		for _, j := range jobs {
			if j.InputReady {
				live[sqlJobKey(j, "input.tar.gz")] = true
				if j.OutputHash != nil {
					live[sqlJobKey(j, "output-"+*j.OutputHash+".tar.gz")] = true
				}
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	prefix := "projects/" + project + "/"
	keys, err := o.objects.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	for _, key := range keys {
		if !strings.HasPrefix(string(key), prefix) || !blob.ValidKey(key) {
			return 0, errors.New("invalid project listing")
		}
	}
	removed := 0
	for _, key := range keys {
		if !live[key] {
			if err = o.objects.Delete(ctx, key); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
func (o *storageOperations) collectAllGarbage(ctx context.Context) (int, error) {
	// Start with a complete global list so deleted projects and failed creates
	// are collected as well. A partial list is never used for deletion.
	keys, err := o.objects.List(ctx, "projects/")
	if err != nil {
		return 0, err
	}
	projects := map[string]bool{}
	for _, key := range keys {
		parts := strings.SplitN(string(key), "/", 3)
		if len(parts) != 3 || parts[0] != "projects" || parts[1] == "" || !blob.ValidKey(key) {
			return 0, errors.New("invalid project listing")
		}
		projects[parts[1]] = true
	}
	removed := 0
	for _, project := range slices.Sorted(maps.Keys(projects)) {
		n, e := o.collectGarbage(ctx, project)
		removed += n
		if e != nil {
			return removed, e
		}
	}
	return removed, nil
}
func (o *storageOperations) maintenance(ctx context.Context, project string, commit ...func(*store.Queries) error) (int, error) {
	now := o.now().UnixMilli()
	if err := o.db.Write(ctx, func(q *store.Queries) error {
		if e := q.CompactHistory(project, &now); e != nil {
			return e
		}
		if err := q.PruneInvites(project, now); err != nil {
			return err
		}
		for _, fn := range commit {
			if err := fn(q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return o.collectGarbage(ctx, project)
}
func (o *storageOperations) deleteProject(ctx context.Context, project string, reconcile func(context.Context, store.JobRow) error, authorize ...func(*store.Queries) error) error {
	unlock, err := o.lock(ctx, project)
	if err != nil {
		return err
	}
	err = o.db.Write(ctx, func(q *store.Queries) error {
		p, e := q.Project(project)
		if e != nil {
			return e
		}
		for _, fn := range authorize {
			if err := fn(q); err != nil {
				return err
			}
		}
		p.Deleting = true
		if e = q.PutProject(p); e != nil {
			return e
		}
		jobs, e := q.ProjectJobs(project)
		if e != nil {
			return e
		}
		for _, j := range jobs {
			if j.Status != "running" && j.Status != "queued" {
				continue
			}
			if j.Data.Run == nil || j.GenerationSnapshot != nil {
				j.Status = "failed"
				j.Data.Error = nullableString("project deleted")
				if e = q.ReleaseInbox(project, j.ID); e != nil {
					return e
				}
			} else if j.Data.Run.AbortReason == nil {
				j.Data.Run.AbortReason = nullableString("project deleted")
			}
			if e = q.PutJob(j); e != nil {
				return e
			}
		}
		return nil
	})
	unlock()
	if err != nil {
		return err
	}
	var jobs []store.JobRow
	if err = o.db.Read(ctx, func(q *store.Queries) error { var e error; jobs, e = q.ProjectJobs(project); return e }); err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Data.Run != nil && !valueBool(j.CleanupComplete) {
			if reconcile == nil {
				return fail(503, "project generation cleanup pending")
			}
			if err = reconcile(ctx, j); err != nil {
				return err
			}
		}
	}
	unlock, err = o.lock(ctx, project)
	if err != nil {
		return err
	}
	defer unlock()
	if err = o.db.Write(ctx, func(q *store.Queries) error {
		jobs, e := q.ProjectJobs(project)
		if e != nil {
			return e
		}
		for _, j := range jobs {
			if j.Status == "running" || j.Status == "queued" || j.Data.Run != nil && !valueBool(j.CleanupComplete) {
				return fail(503, "project generation cleanup pending")
			}
		}
		return q.ClearProjectContent(project)
	}); err != nil {
		return err
	}
	prefix := "projects/" + project + "/"
	keys, err := o.objects.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !strings.HasPrefix(string(key), prefix) || !blob.ValidKey(key) {
			return errors.New("invalid project listing")
		}
	}
	for _, key := range keys {
		if err = o.objects.Delete(ctx, key); err != nil {
			return err
		}
	}
	return o.db.Write(ctx, func(q *store.Queries) error { return q.DeleteProjectRows(project) })
}
