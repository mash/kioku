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
	if content.Incomplete {
		return errors.New("complete Wiki publication before migration")
	}
	project := ProjectRow{ID: old.ID, OrgID: old.OrgID, Slug: old.Slug, Created: migrationCreated(old.Created), NextGenerateAt: migrationTime(content.NextGenerateAt), Data: ProjectData{Name: old.Name, WikiUpdatedAt: migrationTime(content.WikiUpdatedAt)}}
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
		row := JobRow{ID: id, ProjectID: old.ID, Status: job.Status, Created: migrationCreated(job.Created), Kind: job.Kind, Data: JobData{UserID: job.UserID, AgentID: NullableString(job.AgentID), AgentBudgetCents: job.AgentBudgetCents, Automatic: job.Automatic, Error: NullableString(job.Error), Revision: NullableString(job.Revision), WikiStatus: NullableString(job.WikiStatus), Unchanged: job.Unchanged, Views: []ViewResultData{}}}
		// The v2 process stopped before migration, so its unfinished jobs
		// cannot continue. Record them as failed; the user can generate again.
		if job.Status == "queued" || job.Status == "running" {
			row.Status = "failed"
			row.Data.Error = NullableString("stopped by the storage migration")
			if row.Data.WikiStatus != nil {
				row.Data.WikiStatus = NullableString("failed")
			}
		}
		for _, v := range job.Views {
			row.Data.Views = append(row.Data.Views, ViewResultData{ViewID: v.ViewID, Slug: v.Slug, Status: v.Status, Error: NullableString(v.Error)})
		}
		if g := job.ViewGeneration; g != nil {
			row.GenerationSnapshot = &g.Snapshot
			row.Data.ViewGeneration = &GenerationData{Remaining: g.Remaining, Next: g.Next}
		}
		// Only unconsumed children retain linkage in v3. Migration accepts
		// terminal jobs only; keep their kind so HTTP history still hides them.
		if job.ViewTask != nil {
			row.Kind = "view"
		}
		if run := job.Run; run != nil {
			// Provider sessions and files of an unfinished cleanup expire on
			// the provider side; v3 does not resume v2 cleanup.
			row.CleanupComplete = true
			row.Phase = &run.Phase
			row.SessionID = NullableString(run.Ref.ID)
			row.OutputHash = NullableString(run.OutputHash)
			row.Data.Run = &RunData{UsesProviderFiles: run.Ref.UsesProviderFiles, UploadStarted: run.Ref.Resources["upload_started"] == "true", InputFileID: NullableString(run.Ref.Resources["input_file_id"]), AbortReason: NullableString(run.AbortReason), PreparedAt: migrationCreated(run.PreparedAt), Deadline: migrationCreated(run.Deadline), OutputWaitSince: migrationOptionalTime(run.OutputWaitSince), OutputRejection: NullableString(run.OutputRejection), BudgetCents: run.BudgetCents}
			cost := run.CostCents
			if cost != nil && *cost < 0 {
				cost = nil
			}
			row.Data.Run.Usage = &model.UsageMeasurements{CostCents: cost}
		}
		p.jobs = append(p.jobs, row)
	}
	for id, a := range content.Audit {
		if !validID(id) || a.ID != id || a.ProjectID != old.ID {
			return errors.New("legacy audit identity mismatch")
		}
		p.audit = append(p.audit, AuditRow{ID: id, ProjectID: old.ID, Created: migrationCreated(a.Created), Data: AuditData{UserID: a.UserID, Action: a.Action, Detail: a.Detail}})
	}
	if err = p.readViews(ctx, project, content.ViewEpoch, objects, keys, now); err != nil {
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
