package store

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

func (p *migrationPlan) readProject(ctx context.Context, old legacyV2Project, objects blob.Store, now time.Time) error {
	prefix := "projects/" + old.ID + "/"
	keys, err := objects.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !blob.ValidKey(key) || !strings.HasPrefix(string(key), prefix) {
			return errors.New("invalid legacy project listing")
		}
	}
	raw, err := objects.Get(ctx, blob.Key(prefix+".kioku/state.json"))
	if err != nil {
		return err
	}
	var content legacyV2Content
	if err = decodeLegacyJSON(raw, &content); err != nil {
		return err
	}
	if content.Schema != 2 || content.ProjectID != old.ID {
		return errors.New("invalid legacy project content identity or schema")
	}
	if content.Inbox == nil || content.Jobs == nil || content.Audit == nil {
		return errors.New("missing legacy project content collection")
	}
	project := ProjectRow{ID: old.ID, OrgID: old.OrgID, Slug: old.Slug, Created: migrationCreated(old.Created), WikiIncomplete: content.Incomplete, NextGenerateAt: migrationTime(content.NextGenerateAt), ViewEpoch: NullableString(content.ViewEpoch), Data: ProjectData{Name: old.Name, StorageBucket: NullableString(old.StorageBucket), WikiCompleteRevision: NullableString(content.WikiCompleteRevision), WikiUpdatedAt: migrationTime(content.WikiUpdatedAt), ViewsInitialized: true}}
	if content.NextGenerateAt.After(time.Time{}) && !content.NextGenerateAt.IsZero() {
		return errors.New("complete scheduled generation before migration")
	}
	for user, role := range old.Members {
		p.projectMembers = append(p.projectMembers, [3]string{old.ID, user, role})
	}
	for id, item := range content.Inbox {
		if !validID(id) || item.ID != id || item.ProjectID != old.ID {
			return errors.New("legacy inbox identity mismatch")
		}
		if item.Status != "" && item.Status != "removed" {
			return errors.New("unsupported legacy inbox status")
		}
		item.Remove = item.Remove || item.Status == "removed"
		if !item.Remove {
			if _, err = objects.Get(ctx, blob.Key(prefix+".kioku/inbox/"+id+".md")); err != nil {
				return err
			}
		}
		p.inbox = append(p.inbox, InboxRow{ID: id, ProjectID: old.ID, SourceURL: item.SourceURL, Remove: item.Remove, Created: migrationCreated(item.Created), RunID: NullableString(item.RunID), Data: InboxData{Submitter: item.Submitter}})
	}
	if w := content.Wiki; w != nil {
		if w.Files == nil {
			return errors.New("missing legacy Wiki files")
		}
		if w.ProjectID != old.ID || !validID(w.ID) {
			return errors.New("legacy Wiki identity mismatch")
		}
		project.WikiRevision = &w.ID
		// v2 used the current revision when a completed Wiki had no explicit
		// complete revision. Resolve that once, before dropping the old state.
		if !content.Incomplete && project.Data.WikiCompleteRevision == nil {
			project.Data.WikiCompleteRevision = &w.ID
		}
		project.Data.WikiCreated = migrationTime(w.Created)
		project.Data.WikiCompleteBytes = w.CompleteBytes
		var total int64
		for name, key := range w.Files {
			if !wiki.ValidFile(name) || !blob.ValidKey(key) || !strings.HasPrefix(string(key), prefix) {
				return errors.New("invalid legacy Wiki reference")
			}
			body, e := objects.Get(ctx, key)
			if e != nil {
				return e
			}
			hash := string(model.HashContent(body))
			size := int64(len(body))
			if prior, ok := w.FileHashes[name]; ok && prior != hash {
				return errors.New("legacy Wiki hash mismatch")
			}
			if prior, ok := w.FileBytes[name]; ok && prior != size {
				return errors.New("legacy Wiki size mismatch")
			}
			sum := model.SumMeasurements(&total, &size)
			if sum == nil {
				return errors.New("legacy Wiki size overflow")
			}
			total = *sum
			p.wiki = append(p.wiki, WikiFileRow{ProjectID: old.ID, Path: name, Hash: hash, Data: WikiFileData{Bytes: size}})
			p.copies[model.WikiContentKey(old.ID, model.ContentHash(hash))] = body
		}
		if w.StoredBytes != 0 && w.StoredBytes != total {
			return errors.New("legacy Wiki total size mismatch")
		}
		if w.CompleteBytes != nil && *w.CompleteBytes < 0 {
			return errors.New("invalid completed Wiki size")
		}
	}
	for id, job := range content.Jobs {
		if !validID(id) || job.ID != id || job.ProjectID != old.ID {
			return errors.New("legacy job identity mismatch")
		}
		if job.Status == "queued" || job.Status == "running" {
			return errors.New("finish queued and running jobs before migration")
		}
		row := JobRow{ID: id, ProjectID: old.ID, Status: job.Status, Created: migrationCreated(job.Created), Kind: job.Kind, Data: JobData{UserID: job.UserID, AgentID: NullableString(job.AgentID), AgentBudgetCents: job.AgentBudgetCents, Automatic: job.Automatic, Error: NullableString(job.Error), Revision: NullableString(job.Revision), WikiStatus: NullableString(job.WikiStatus), Unchanged: job.Unchanged, Views: []ViewResultData{}}}
		for _, v := range job.Views {
			row.Data.Views = append(row.Data.Views, ViewResultData{ViewID: v.ViewID, Slug: v.Slug, Status: v.Status, Error: NullableString(v.Error)})
		}
		if g := job.ViewGeneration; g != nil {
			row.GenerationSnapshot = &g.Snapshot
			row.ChildID = NullableString(g.ChildID)
			row.Data.ViewGeneration = &GenerationData{Pending: g.Pending, Remaining: g.Remaining, Next: g.Next}
		}
		if task := job.ViewTask; task != nil {
			row.ParentID = &task.ParentID
			row.Data.ViewTask = &ViewTaskData{ViewID: task.ViewID, Snapshot: task.Snapshot}
		}
		if run := job.Run; run != nil {
			if !run.CleanupComplete {
				return errors.New("complete all runner cleanup before migration")
			}
			complete := true
			row.CleanupComplete = &complete
			row.Phase = &run.Phase
			row.SessionID = NullableString(run.Ref.ID)
			row.OutputHash = NullableString(run.OutputHash)
			row.Data.Run = &RunData{UsesProviderFiles: run.Ref.UsesProviderFiles, Provider: NullableString(run.Ref.Resources["provider"]), UploadStarted: run.Ref.Resources["upload_started"] == "true", InputFileID: NullableString(run.Ref.Resources["input_file_id"]), AbortReason: NullableString(run.AbortReason), PreparedAt: migrationCreated(run.PreparedAt), Deadline: migrationCreated(run.Deadline), OutputWaitSince: migrationOptionalTime(run.OutputWaitSince), OutputRejection: NullableString(run.OutputRejection), BudgetCents: run.BudgetCents, InboxIDs: run.InboxIDs}
			cost := run.CostCents
			if cost != nil && *cost < 0 {
				cost = nil
			}
			p.usage = append(p.usage, UsageRow{AttemptID: id, ProjectID: old.ID, JobID: id, RecordedAt: now.UnixMilli(), Data: model.UsageValues{OrgID: old.OrgID, UserID: job.UserID, UsageMeasurements: model.UsageMeasurements{CostCents: cost}}})
		}
		p.jobs = append(p.jobs, row)
	}
	for id, a := range content.Audit {
		if !validID(id) || a.ID != id || a.ProjectID != old.ID {
			return errors.New("legacy audit identity mismatch")
		}
		p.audit = append(p.audit, AuditRow{ID: id, ProjectID: old.ID, Created: migrationCreated(a.Created), Data: AuditData{UserID: a.UserID, Action: a.Action, Detail: a.Detail}})
	}
	if err = p.readViews(ctx, project, objects, keys, now); err != nil {
		return err
	}
	p.projects = append(p.projects, project)
	for _, key := range keys {
		if obsoleteV2Object(strings.TrimPrefix(string(key), prefix)) {
			p.obsolete = append(p.obsolete, key)
		}
	}
	return nil
}
func obsoleteV2Object(name string) bool {
	if name == ".kioku/state.json" {
		return true
	}
	if strings.HasPrefix(name, "wiki/") || strings.HasPrefix(name, "maps/") {
		return true
	}
	if strings.HasPrefix(name, ".kioku/transactions/") && strings.HasSuffix(name, ".json") {
		return true
	}
	for _, prefix := range []string{".kioku/jobs/", ".kioku/audit/"} {
		if suffix, ok := strings.CutPrefix(name, prefix); ok && !strings.Contains(suffix, "/") && strings.HasSuffix(suffix, ".json") {
			return true
		}
	}
	if strings.HasPrefix(name, ".kioku/views/") {
		base := path.Base(name)
		return base == "index.json" || base == "view.json" || base == "edit.json" || base == "publish.json" || base == "bundle.zip" || base == "candidate.zip" || base == "previous.zip"
	}
	return false
}
