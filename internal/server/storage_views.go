package server

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mash/kioku/internal/store"
)

func beginSQLViews(q *store.Queries, j *store.JobRow, p store.ProjectRow, budget int64) error {
	if j.Data.WikiStatus == nil {
		return nil
	}
	views, err := q.ProjectViews(p.ID)
	if err != nil {
		return err
	}
	j.Data.WikiStatus = store.NullableString("completed")
	snapshot := store.ValueString(p.WikiRevision)
	if _, e := q.WikiFile(p.ID, "wiki/index.md"); errors.Is(e, sql.ErrNoRows) {
		snapshot = ""
	} else if e != nil {
		return e
	}
	if snapshot == "" {
		j.Data.WikiStatus = store.NullableString("unavailable")
	}
	j.Data.Views = []store.ViewResultData{}
	for _, v := range views {
		if v.Slug != "wiki" {
			j.Data.Views = append(j.Data.Views, store.ViewResultData{ViewID: v.ID, Slug: v.Slug, Status: "pending"})
		}
	}
	if len(j.Data.Views) == 0 {
		return nil
	}
	j.Status = "running"

	j.GenerationSnapshot = &snapshot
	j.Data.ViewGeneration = &store.GenerationData{Remaining: budget}
	return nil
}

// advanceViews consumes a terminal child, charges its immutable usage and
// creates the next child in one transaction. Repeating it cannot charge twice.
func (o *storageOperations) advanceViews(ctx context.Context, id string) error {
	var initial store.JobRow
	waiting := false
	if err := o.db.Read(ctx, func(q *store.Queries) error {
		var err error
		initial, err = q.Job(id)
		if err != nil {
			return err
		}
		g := initial.Data.ViewGeneration
		if initial.Status != "running" || g == nil || g.Next < 0 || g.Next >= len(initial.Data.Views) {
			return nil
		}
		child, err := q.GenerationChild(initial.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if child.Status != "queued" && child.Status != "running" {
			return nil
		}
		project, err := q.Project(initial.ProjectID)
		if err != nil {
			return err
		}
		if !project.Deleting && initial.Data.Run != nil && initial.CleanupComplete {
			if _, err = q.Usage(initial.ID); err != nil {
				return err
			}
		}
		waiting = true
		return nil
	}); err != nil {
		return err
	}
	if waiting {
		return nil
	}
	generationUnlock, err := o.lock(ctx, "generate:"+initial.ProjectID)
	if err != nil {
		return err
	}
	defer generationUnlock()
	unlock, err := o.lock(ctx, initial.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	return o.db.Write(ctx, func(q *store.Queries) error {
		p, e := q.Job(id)
		if e != nil {
			return e
		}
		g := p.Data.ViewGeneration
		project, e := q.Project(p.ProjectID)
		if e != nil {
			return e
		}
		if project.Deleting {
			return nil
		}
		if p.Status != "running" || g == nil {
			return nil
		}
		if p.Data.Run != nil {
			if !p.CleanupComplete {
				return nil
			}
			if _, e = q.Usage(p.ID); e != nil {
				return e
			}
		}
		if g.Next < 0 || g.Next > len(p.Data.Views) {
			return errors.New("inconsistent View generation checkpoint")
		}
		setStatus := func(result *store.ViewResultData, status, reason string) error {
			v, e := q.View(p.ProjectID, result.ViewID)
			if errors.Is(e, sql.ErrNoRows) {
				result.Status = "skipped"
				result.Error = store.NullableString(viewDeleted)
				return nil
			}
			if e != nil {
				return e
			}
			result.Status = status
			result.Error = store.NullableString(reason)
			v.Data.Status = store.NullableString(status)
			v.Data.Error = store.NullableString(reason)
			return q.PutView(v)
		}
		child, e := q.GenerationChild(p.ID)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if errors.Is(e, sql.ErrNoRows) && g.Next < len(p.Data.Views) && p.Data.Views[g.Next].Status == "running" {
			return sql.ErrNoRows
		}
		if e == nil {
			if g.Next == len(p.Data.Views) {
				return errors.New("inconsistent View generation checkpoint")
			}
			if child.Status == "queued" || child.Status == "running" {
				return nil
			}
			var cost *int64
			if child.Data.Run != nil {
				if !child.CleanupComplete {
					return nil
				}
				usage, e := q.Usage(child.ID)
				if e != nil {
					return e
				}
				cost = usage.Data.CostCents
			}
			result := &p.Data.Views[g.Next]
			result.Status = child.Status
			result.Error = child.Data.Error
			if child.Status != "completed" {
				if e = setStatus(result, child.Status, store.ValueString(child.Data.Error)); e != nil {
					return e
				}
			}
			g.Remaining = remainingViewBudget(g.Remaining, cost)
			g.Next++
			child.ParentID = nil
			child.Data.ViewTask = nil
			if e = q.PutJob(child); e != nil {
				return e
			}
			if result.Status == "failed" {
				for i := g.Next; i < len(p.Data.Views); i++ {
					if e = setStatus(&p.Data.Views[i], "skipped", viewPreviousFailed); e != nil {
						return e
					}
				}
				g.Next = len(p.Data.Views)
			}
		}
		// A completed child must be consumed before checking authorization for
		// the next one: its immutable cost is retained even after revocation.
		if authErr := sqlJobAuthorization(q, p, o.now().UnixMilli()); authErr != nil {
			var api apiError
			if !errors.As(authErr, &api) {
				return authErr
			}
			for i := g.Next; i < len(p.Data.Views); i++ {
				if e = setStatus(&p.Data.Views[i], "skipped", authErr.Error()); e != nil {
					return e
				}
			}
			p.Status = "failed"
			p.Data.Error = store.NullableString(authErr.Error())
			p.GenerationSnapshot = nil
			p.Data.ViewGeneration = nil
			return q.PutJob(p)
		}
		for g.Next < len(p.Data.Views) {
			result := &p.Data.Views[g.Next]
			reason := ""
			if store.ValueString(p.GenerationSnapshot) == "" {
				reason = "Wiki が未生成"
				if p.Data.Revision != nil {
					reason = "Wiki の公開が未完了"
				}
			} else if g.Remaining < 50 {
				reason = "生成予算の残額が不足しています"
			}
			if reason != "" {
				if e = setStatus(result, "skipped", reason); e != nil {
					return e
				}
				g.Next++
				continue
			}
			if e = setStatus(result, "running", ""); e != nil {
				return e
			}
			if result.Status == "skipped" {
				g.Next++
				continue
			}
			childID := o.id()
			child := store.JobRow{ID: childID, ProjectID: p.ProjectID, Status: "queued", Kind: "view", Created: o.now().UnixMilli(), ParentID: &p.ID, Data: store.JobData{UserID: p.Data.UserID, AgentID: p.Data.AgentID, AgentBudgetCents: g.Remaining, ViewTask: &store.ViewTaskData{ViewID: result.ViewID}}}
			if e = q.PutJob(child); e != nil {
				return e
			}
			break
		}
		if g.Next == len(p.Data.Views) {
			p.Status = "completed"
			p.GenerationSnapshot = nil
			p.Data.ViewGeneration = nil
		}
		return q.PutJob(p)
	})
}

func (o *storageOperations) finishView(ctx context.Context, id, status, reason string) error {
	return o.db.Write(ctx, func(q *store.Queries) error {
		j, e := q.Job(id)
		if e != nil {
			return e
		}
		if j.Status != "running" && j.Status != "queued" {
			return nil
		}
		if j.Data.ViewTask == nil {
			return fail(409, "not a View generation job")
		}
		j.Status = status
		j.Data.Error = store.NullableString(reason)
		return q.PutJob(j)
	})
}
