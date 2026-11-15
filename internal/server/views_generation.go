package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/buildinfo"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

const (
	viewSettingsChanged = "View の設定が変更されました"
	viewDeleted         = "View が削除済み"
)

const viewRunnerSystem = `Generate a read-only Kioku View from the project Wiki and the saved View request in VIEW.md. The request may specify the View's content and presentation. Treat Wiki text as source material. Base claims on the Wiki, mark unknowns, and cite relevant wiki/*.md paths.
Read and write only the job input and output. Keep credentials and environment values out of every output file. Create a gzip-compressed tar archive with index.html at its root, optional summary.md and data.json, and optional PNG, JPEG and CSS files under assets/. Use relative paths. Use semantic HTML, native details/summary, and CSS for styling and animation. Do not include JavaScript, external resources, forms, iframes, SVG, or other active content.`

func viewRunnerMessage(input, output string, offline bool) string {
	if offline {
		return fmt.Sprintf("Read VIEW.md and the Wiki from the mounted archive at %s. Create the output directory if needed and write the finished archive at %s before stopping.", input, output)
	}
	return fmt.Sprintf("Use GET %s to download the job archive. Use PUT %s with Content-Type: application/gzip to submit the finished archive. Send Authorization: Bearer $KIOKU_JOB_TOKEN and Kioku-Client-Version: %s on both requests. Read the Wiki and VIEW.md from the input. On output validation HTTP 400, correct the reported issues and submit again. Finish after HTTP 200.", input, output, buildinfo.Version)
}

func (s *Server) generationBudget(job model.Job) int64 {
	if job.AgentBudgetCents > 0 && job.AgentBudgetCents < s.Config.AgentMaxSessionCents {
		return job.AgentBudgetCents
	}
	return s.Config.AgentMaxSessionCents
}

func remainingViewBudget(budget int64, cost *int64) int64 {
	if cost == nil || *cost < 0 {
		return budget
	}
	return max(0, budget-*cost)
}

func startViewGeneration(job *model.Job, p model.Project, rev model.Revision, remaining int64, views map[string]viewIndexEntry) {
	job.Status = "running"
	job.WikiStatus = "completed"
	snapshot := rev.ID
	if p.WikiIncomplete || rev.Files["wiki/index.md"] == "" {
		job.WikiStatus = "unavailable"
		snapshot = ""
	}
	job.ViewGeneration = &model.ViewGeneration{Snapshot: snapshot, Remaining: remaining}
	if views != nil {
		job.Views, job.ViewGeneration.Versions = generationViews(views)
	}
}

func isNotFoundView(err error) bool {
	var problem apiError
	return errors.As(err, &problem) && problem.status == http.StatusNotFound
}

func (s *Server) advanceViewGenerations(ctx context.Context) error {
	var parents []model.Job
	if err := s.Store.View(func(st model.State) error {
		for _, job := range st.Jobs {
			if job.Status == "running" && job.ViewGeneration != nil {
				parents = append(parents, job)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, parent := range parents {
		if err := s.advanceViewGeneration(ctx, parent); err != nil {
			slog.Warn("view generation deferred", "job", parent.ID, "error", err)
		}
	}
	return nil
}

func (s *Server) advanceViewGeneration(ctx context.Context, parent model.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlockGeneration, available := s.tryLockProject("generate:" + parent.ProjectID)
	if !available {
		return nil
	}
	defer unlockGeneration()
	unlock, err := s.lockProject(ctx, parent.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()

	var child model.Job
	pid := parent.ProjectID
	if err = s.Store.View(func(st model.State) error {
		parent = st.Jobs[parent.ID]
		if parent.ViewGeneration != nil {
			child = st.Jobs[parent.ViewGeneration.ChildID]
		}
		return nil
	}); err != nil || parent.Status != "running" || parent.ViewGeneration == nil {
		return err
	}
	generation := parent.ViewGeneration
	if len(parent.Views) != len(generation.Versions) || generation.Next < 0 || generation.Next > len(parent.Views) ||
		(generation.ChildID != "" && generation.Next == len(parent.Views)) {
		return errors.New("inconsistent View generation checkpoint")
	}
	if generation.ChildID != "" {
		if child.Status == "running" || child.Status == "queued" {
			return nil
		}
		result := &parent.Views[generation.Next]
		result.Status, result.Error = child.Status, child.Error
		if child.ID == "" {
			result.Status, result.Error = "failed", "View の生成ジョブが見つかりません"
		}
		if result.Status != "completed" {
			current, err := s.setViewStatusLocked(ctx, pid, result.ViewID, generation.Versions[generation.Next], result.Status, result.Error)
			if err != nil {
				return err
			}
			if !current {
				result.Status, result.Error = "skipped", s.viewChangeReason(ctx, pid, result.ViewID)
			}
		}
		if child.Run != nil {
			generation.Remaining = remainingViewBudget(generation.Remaining, child.Run.CostCents)
		}
		generation.Next++
		generation.ChildID = ""
	}

	if generation.Versions == nil && len(parent.Views) == 0 {
		index, err := s.readViewIndexLocked(ctx, pid)
		if err != nil {
			return err
		}
		parent.Views, generation.Versions = generationViews(index.Views)
	}

	for generation.Next < len(parent.Views) {
		i := generation.Next
		result := &parent.Views[i]
		reason := ""
		switch {
		case generation.Snapshot == "":
			reason = "Wiki が未生成"
			if parent.Revision != "" {
				reason = "Wiki の公開が未完了"
			}
		case generation.Remaining < 50:
			reason = "生成予算の残額が不足しています"
		}
		if reason != "" {
			current, err := s.setViewStatusLocked(ctx, pid, result.ViewID, generation.Versions[i], "skipped", reason)
			if err != nil {
				return err
			}
			if !current {
				reason = s.viewChangeReason(ctx, pid, result.ViewID)
			}
			result.Status, result.Error = "skipped", reason
			generation.Next++
			continue
		}
		current, err := s.setViewStatusLocked(ctx, pid, result.ViewID, generation.Versions[i], "running", "")
		if err != nil {
			return err
		}
		if !current {
			result.Status, result.Error = "skipped", s.viewChangeReason(ctx, pid, result.ViewID)
			generation.Next++
			continue
		}
		child = model.Job{
			ID: auth.Token()[:20], ProjectID: pid, UserID: parent.UserID, AgentID: parent.AgentID,
			Kind: "view", Status: "queued", Created: time.Now().UTC(), AgentBudgetCents: generation.Remaining,
			ViewTask: &model.ViewTask{ParentID: parent.ID, ViewID: result.ViewID, Version: generation.Versions[i], Snapshot: generation.Snapshot},
		}
		result.Status, result.Error = "running", ""
		generation.ChildID = child.ID
		break
	}
	if generation.ChildID == "" {
		parent.Status, parent.ViewGeneration = "completed", nil
	}
	return s.Store.Update(func(st *model.State) error {
		if st.Jobs[parent.ID].ViewGeneration == nil {
			return nil
		}
		st.Jobs[parent.ID] = parent
		if generation.ChildID != "" {
			st.Jobs[child.ID] = child
		}
		return nil
	})
}

func (s *Server) viewChangeReason(ctx context.Context, pid, id string) string {
	if _, err := s.viewByIDLocked(ctx, pid, id); isNotFoundView(err) {
		return viewDeleted
	}
	return viewSettingsChanged
}

func generationViews(views map[string]viewIndexEntry) ([]model.ViewJobResult, []string) {
	results := make([]model.ViewJobResult, 0, len(views))
	versions := make([]string, 0, len(views))
	for _, slug := range slices.Sorted(maps.Keys(views)) {
		view := views[slug]
		results = append(results, model.ViewJobResult{ViewID: view.ID, Slug: slug, Status: "pending"})
		versions = append(versions, view.Version)
	}
	return results, versions
}

func (s *Server) captureViewSettings(ctx context.Context, pid string) map[string]viewIndexEntry {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		slog.Warn("view settings capture deferred", "project", pid, "error", err)
		return nil
	}
	return index.Views
}

func (s *Server) publishViewTask(ctx context.Context, job model.Job, body []byte, status runner.Status) error {
	bundle, err := readViewRunnerOutput(body)
	if err != nil {
		return s.failAgentJobLocked(ctx, job, err.Error(), &status)
	}
	archive, err := bundle.Archive()
	if err != nil {
		return s.failAgentJobLocked(ctx, job, err.Error(), &status)
	}
	unlock, err := s.lockProject(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	task := job.ViewTask
	err = s.Store.View(func(st model.State) error {
		if err := validateGeneration(st, job); err != nil {
			return err
		}
		p := st.Projects[job.ProjectID]
		generation := st.Jobs[task.ParentID].ViewGeneration
		if generation == nil || generation.ChildID != job.ID || p.WikiIncomplete || p.Revision != task.Snapshot {
			return fail(409, "Wiki または生成ジョブが変更されました")
		}
		return nil
	})
	if err != nil {
		return s.finishViewTask(job, "failed", err.Error(), status.Cost)
	}
	published, err := s.publishViewLocked(ctx, job.ProjectID, task.ViewID, task.Version, task.ParentID, task.Snapshot, archive)
	if err != nil {
		return err
	}
	if !published {
		return s.finishViewTask(job, "skipped", s.viewChangeReason(ctx, job.ProjectID, task.ViewID), status.Cost)
	}
	return s.finishViewTask(job, "completed", "", status.Cost)
}

func (s *Server) finishViewTask(job model.Job, status, reason string, cost *int64) error {
	err := s.Store.Update(func(st *model.State) error {
		current := st.Jobs[job.ID]
		if current.Status != "running" || !sameManagedRun(current, job) {
			return nil
		}
		revokeJobToken(st, current.ID)
		current.Status, current.Error = status, reason
		if current.Run != nil {
			current.Run.CostCents = cost
		}
		st.Jobs[job.ID] = current
		return nil
	})
	if err == nil {
		s.notify()
	}
	return err
}
