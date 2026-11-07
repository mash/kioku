package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"github.com/mash/kioku/internal/wiki"
)

// storeSources persists immutable source snapshots and source receipts.
func (s *Server) storeSources(ctx context.Context, docs []source.Document, job model.Job) (model.Job, error) {
	pid, uid := job.ProjectID, job.UserID
	for _, doc := range docs {
		if err := source.ValidateSourceURL(doc.URL); err != nil {
			return job, fail(400, err.Error())
		}
		if len(doc.Title) > 1024 || len(doc.URL) > 4096 {
			return job, fail(400, "source title or URL is too long")
		}
	}
	unlock, err := s.lockProject(ctx, pid)
	if err != nil {
		return job, err
	}
	defer unlock()
	// Reservations left by a failed request must be reclaimed before accepting
	// more work in this project. A failed cleanup remains charged to its quota.
	if err = s.recoverProjectUploads(ctx, pid); err != nil {
		return job, err
	}
	if job.ID == "" {
		job.ID = auth.Token()[:20]
	}
	if job.Created.IsZero() {
		job.Created = time.Now().UTC()
	}
	if job.Kind == "put" {
		job.Status = "completed"
	} else {
		job.Status = "queued"
	}
	job.SourceIDs = []string{}
	upload := model.Upload{ID: auth.Token()[:20], ProjectID: pid, Job: job, Heads: map[string]string{}}
	documents := map[string]source.Document{}
	err = s.DB.Update(func(st *model.State) error {
		if _, err := member(*st, pid, uid); err != nil {
			return err
		}
		if job.Kind == "put" && job.InputURL != "" {
			existing, ok := st.Jobs[job.ID]
			if !ok || existing.Status != "running" {
				return fail(409, "source intake job was canceled")
			}
		}
		project := st.Projects[pid]
		if upload.Job.PutSequence == 0 {
			project.NextPutSequence++
			upload.Job.PutSequence = project.NextPutSequence
			st.Projects[pid] = project
		}
		known := map[string]model.Source{}
		for id, src := range st.Sources {
			if src.ProjectID == pid {
				known[id] = src
			}
		}
		for _, doc := range docs {
			hash := auth.Hash(string(doc.Original))
			id := ""
			for k, old := range known {
				if old.ContentHash == hash && old.URL == doc.URL && old.Title == doc.Title && old.Kind == doc.Kind {
					id = k
					break
				}
			}
			if id == "" {
				id = auth.Token()[:20]
				prefix := blob.Key("projects/" + pid + "/sources/" + id + "/")
				src := model.Source{ID: id, ProjectID: pid, Title: doc.Title, URL: doc.URL, Kind: doc.Kind, Version: doc.Version, ContentHash: hash, TextKey: prefix + "extracted.md", MediaType: "text/markdown", Bytes: int64(len(doc.Text) + len(doc.Title) + len(doc.URL)), StoredBytes: int64(len(doc.Text)), CreatedBy: uid, Created: time.Now().UTC()}
				upload.Sources = append(upload.Sources, src)
				known[id] = src
				documents[id] = doc
			}
			upload.Job.SourceIDs = append(upload.Job.SourceIDs, id)
			if doc.URL != "" && doc.Kind != "file" {
				upload.Heads[doc.URL] = id
			}
		}
		count, queued := 0, 0
		var bytes int64
		for _, src := range st.Sources {
			if src.ProjectID == pid {
				count++
				bytes += src.Bytes
			}
		}
		for _, u := range st.Uploads {
			if u.ProjectID == pid {
				queued++
				for _, src := range u.Sources {
					count++
					bytes += src.Bytes
				}
			}
		}
		for _, j := range st.Jobs {
			if j.ProjectID == pid && j.Kind != "put" && (j.Status == "queued" || j.Status == "running") {
				queued++
			}
		}
		for _, src := range upload.Sources {
			count++
			bytes += src.Bytes
		}
		if count > s.Config.MaxSources || bytes > s.Config.MaxBytes {
			return fail(413, "project source quota exceeded")
		}
		if queued >= s.Config.MaxQueued && job.Kind != "put" {
			return fail(429, "project job queue is full")
		}
		project = st.Projects[pid]
		if st.Uploads == nil {
			st.Uploads = map[string]model.Upload{}
		}
		st.Uploads[upload.ID] = upload
		return nil
	})
	if err != nil {
		return job, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.discardUpload(cleanupCtx, upload.ID); err != nil {
				slog.Error("upload cleanup deferred", "upload", upload.ID, "error", err)
			}
		}
	}()
	for _, src := range upload.Sources {
		doc := documents[src.ID]
		if err = s.Blobs.Put(ctx, src.TextKey, []byte(doc.Text)); err != nil {
			return job, err
		}
	}
	err = s.DB.Update(func(st *model.State) error {
		p, err := member(*st, pid, uid)
		if err != nil {
			return err
		}
		if err := checkJobAgent(*st, upload.Job); err != nil {
			return err
		}
		if _, ok := st.Uploads[upload.ID]; !ok {
			return fmt.Errorf("upload reservation disappeared")
		}
		if job.Kind == "put" && job.InputURL != "" {
			existing, ok := st.Jobs[job.ID]
			if !ok || existing.Status != "running" {
				return fail(409, "source intake job was canceled")
			}
		}
		now := time.Now().UTC()
		changed := false
		for _, src := range upload.Sources {
			src.UpdatedAt = now
			if src.URL == "" || src.Kind == "file" {
				changed = true
			}
			st.Sources[src.ID] = src
		}
		if p.SourceHeads == nil {
			p.SourceHeads = map[string]string{}
		}
		if p.SourceHeadSequences == nil {
			p.SourceHeadSequences = map[string]int64{}
		}
		for url, id := range upload.Heads {
			if upload.Job.PutSequence >= p.SourceHeadSequences[url] {
				if p.SourceHeads[url] != id {
					changed = true
					src := st.Sources[id]
					src.UpdatedAt = now
					st.Sources[id] = src
				}
				p.SourceHeads[url] = id
				p.SourceHeadSequences[url] = upload.Job.PutSequence
			}
		}
		if changed {
			p.SourceVersion++
			p.SourcesUpdatedAt = now
		}
		upload.Job.SourceVersion = p.SourceVersion
		st.Projects[pid] = p
		st.Jobs[upload.Job.ID] = upload.Job
		evidence := make([]model.Source, 0, len(upload.Job.SourceIDs))
		for _, id := range upload.Job.SourceIDs {
			evidence = append(evidence, st.Sources[id])
		}
		recordIntake(st, upload.Job, "stored", evidence)
		delete(st.Uploads, upload.ID)
		return nil
	})
	if err != nil {
		return job, err
	}
	committed = true
	job = upload.Job

	return job, nil
}

func (s *Server) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// RunWorker uses a durable queue. One worker serializes publication in this
// initial backend, so concurrent submissions cannot overwrite each other.
func (s *Server) RunWorker(ctx context.Context) error {
	if err := s.Prepare(); err != nil {
		return err
	}
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	var intakeWG sync.WaitGroup
	for i := 0; i < s.Config.IntakeWorkers; i++ {
		intakeWG.Add(1)
		go func() { defer intakeWG.Done(); s.runIntake(maintenanceCtx) }()
	}
	s.notifyIntake()
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); s.runMaintenance(maintenanceCtx) }()
	defer func() { stopMaintenance(); <-maintenanceDone; intakeWG.Wait() }()
	s.notify()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-ticker.C:
		}
		for ctx.Err() == nil {
			var job model.Job
			err := s.DB.Update(func(st *model.State) error {
				pending := []model.Job{}
				running := map[string]bool{}
				sessions := 0
				for _, j := range st.Jobs {
					if (j.Status == "running" || j.Managed != nil && j.Managed.StopPending) && j.Kind != "put" {
						running[j.ProjectID] = true
						if j.Managed != nil {
							sessions++
						}
					}
				}
				for _, j := range st.Jobs {
					if j.Status == "queued" && j.Kind != "put" {
						if running[j.ProjectID] || (j.AnthropicConnectionID != "" || s.Config.managedEnabled()) && sessions >= s.Config.AgentMaxSessions {
							continue
						}
						ready := true
						for _, id := range j.Dependencies {
							d, ok := st.Jobs[id]
							if !ok || d.Status == "failed" {
								j.Status = "failed"
								j.Error = "source intake dependency failed; retry after fixing the source"
								st.Jobs[j.ID] = j
								ready = false
								break
							}
							if d.Status != "completed" {
								ready = false
							}
						}
						if ready {
							pending = append(pending, j)
						}
					}
				}
				if len(pending) == 0 {
					return nil
				}
				sort.Slice(pending, func(i, j int) bool {
					if pending[i].Created.Equal(pending[j].Created) {
						return pending[i].ID < pending[j].ID
					}
					return pending[i].Created.Before(pending[j].Created)
				})
				job = pending[0]
				job.Status = "running"
				st.Jobs[job.ID] = job
				return nil
			})
			if err != nil {
				return err
			}
			if job.ID == "" {
				break
			}
			jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			if job.AnthropicConnectionID != "" || s.Config.managedEnabled() {
				err := s.startAgentJob(jobCtx, job)
				cancel()
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					return err
				}
				continue
			}
			rev, runErr := s.process(jobCtx, job)
			cancel()
			if ctx.Err() != nil {
				return nil
			} // running job will be recovered at next boot
			err = s.DB.Update(func(st *model.State) error {
				j, exists := st.Jobs[job.ID]
				if !exists {
					return nil
				}
				if runErr != nil {
					j.Status = "failed"
					j.Error = runErr.Error()
				} else {
					j.Status = "completed"
					j.Error = ""
					j.Revision = rev
				}
				st.Jobs[j.ID] = j
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
}

// Maintenance must keep running while the generation queue is busy.
func (s *Server) runMaintenance(ctx context.Context) {
	var lastReconcile time.Time
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Since(lastReconcile) >= 30*time.Second {
			if err := s.reconcileAgentJobs(ctx); err != nil {
				slog.Error("agent reconciliation deferred", "error", err)
			}
			lastReconcile = time.Now()
		}
		if err := s.scheduleGenerations(time.Now().UTC()); err != nil {
			slog.Error("generation scheduling deferred", "error", err)
		}
		if err := s.recoverUploads(ctx); err != nil {
			slog.Error("upload recovery deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) process(ctx context.Context, job model.Job) (string, error) {
	if err := s.check(job.ProjectID, job.UserID); err != nil {
		return "", err
	}
	unlock, err := s.lockProject(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return "", err
	}
	defer unlock()
	var project model.Project
	var published model.Revision
	var hasPublished bool

	var all []model.Source
	err = s.DB.View(func(st model.State) error {
		var err error
		if err = checkJobAgent(st, job); err != nil {
			return err
		}
		project, err = member(st, job.ProjectID, job.UserID)
		if err != nil {
			return err
		}
		published, hasPublished = st.Revisions[project.Revision]
		hasPublished = hasPublished && published.ProjectID == project.ID
		for _, src := range st.Sources {
			if src.ProjectID == job.ProjectID {
				all = append(all, src)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Created.Equal(all[j].Created) {
			return all[i].ID < all[j].ID
		}
		return all[i].Created.Before(all[j].Created)
	})
	// A refreshed URL keeps its previous snapshots as evidence, but only the
	// latest version feeds the current wiki.
	latest := map[string]string{}
	for _, src := range all {
		if src.URL != "" && src.Kind != "file" {
			latest[src.URL] = src.ID
		}
	}
	for url, id := range project.SourceHeads {
		latest[url] = id
	}
	active := all[:0]
	for _, src := range all {
		if src.Kind == "file" || src.URL == "" || latest[src.URL] == src.ID {
			active = append(active, src)
		}
	}
	all = active
	validate := func(st model.State) error {
		if err := checkJobAgent(st, job); err != nil {
			return err
		}
		if _, ok := st.Jobs[job.ID]; !ok {
			return fail(409, "generation job was canceled")
		}
		for _, src := range all {
			current, ok := st.Sources[src.ID]
			if !ok || current.ProjectID != job.ProjectID {
				return fail(409, "generation inputs were removed")
			}
		}
		return nil
	}

	// Source snapshots are immutable. Matching active IDs means the wiki already
	// covers these inputs, even if model settings changed or a URL reverted to a
	// previously published version. No blob reads, model call or revision write.
	if !job.Full && !project.WikiIncomplete && (hasPublished && sameSourceIDs(all, published.SourceIDs) || project.Revision == "" && len(all) == 0 && project.SourceVersion == project.WikiSourceVersion) {
		publishUnlock, err := s.lockProject(ctx, job.ProjectID)
		if err != nil {
			return "", err
		}
		defer publishUnlock()
		err = s.DB.Update(func(st *model.State) error {
			if err := validate(*st); err != nil {
				return err
			}
			p, err := member(*st, job.ProjectID, job.UserID)
			if err != nil {
				return err
			}
			if p.Revision != project.Revision {
				return fail(409, "published revision changed; retry generation")
			}
			p.WikiSourceVersion = project.SourceVersion
			p.ProcessedIntakeSequence = project.NextIntakeSequence
			st.Projects[p.ID] = p
			j := st.Jobs[job.ID]
			j.Status = "completed"
			j.Error = ""
			j.Unchanged = true
			j.Revision = project.Revision
			j.Fingerprint = published.Fingerprint
			j.SourceVersion = project.SourceVersion
			st.Jobs[j.ID] = j
			return nil
		})
		return project.Revision, err
	}
	inputs := []wiki.Input{}
	for _, src := range all {
		b, err := s.Blobs.Get(ctx, src.TextKey)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, wiki.Input{Source: src, Text: string(b)})
	}
	fingerprint := generationFingerprint(inputs)
	if err = s.DB.View(validate); err != nil {
		return "", err
	}
	files, err := wiki.Assemble(inputs, nil)
	if err != nil {
		return "", err
	}
	return s.publishWiki(ctx, publishInput{Job: job, Project: project, Sources: all, Files: files, Fingerprint: fingerprint, Validate: validate})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request, uid string) error {
	pid, id := r.PathValue("project"), r.PathValue("job")
	var j model.Job
	var found bool
	var validAfter time.Time
	if err := s.DB.View(func(st model.State) error {
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		j, found = st.Jobs[id]
		validAfter = p.JobsValidAfter
		return nil
	}); err != nil {
		return err
	}
	if !found {
		var err error
		j, err = s.DB.ArchivedJob(r.Context(), pid, id)
		if err != nil {
			return fail(404, "job not found")
		}
	}
	if j.ProjectID != pid || j.Created.Before(validAfter) {
		return fail(404, "job not found")
	}
	jsonOut(w, 200, j)
	return nil
}
func (s *Server) retry(w http.ResponseWriter, r *http.Request, uid string) error {
	pid, id := r.PathValue("project"), r.PathValue("job")
	if err := s.check(pid, uid); err != nil {
		return err
	}
	var archived model.Job
	var found bool
	var cutoff time.Time
	if err := s.DB.View(func(st model.State) error {
		_, found = st.Jobs[id]
		cutoff = st.Projects[pid].JobsValidAfter
		return nil
	}); err != nil {
		return err
	}
	if !found {
		var err error
		archived, err = s.DB.ArchivedJob(r.Context(), pid, id)
		if err != nil || archived.Created.Before(cutoff) {
			return fail(404, "job not found")
		}
	}
	_, principal := s.session(r)
	var job model.Job
	err := s.DB.Update(func(st *model.State) error {
		pid := r.PathValue("project")
		if _, err := member(*st, pid, uid); err != nil {
			return err
		}
		j, ok := st.Jobs[r.PathValue("job")]
		if !ok && archived.ID != "" {
			j = archived
			ok = true
		}
		if !ok || j.ProjectID != pid || j.Created.Before(st.Projects[pid].JobsValidAfter) {
			return fail(404, "job not found")
		}
		if principal.AgentID != "" && j.Kind == "put" && j.InputURL != "" {
			return fail(403, "agents cannot retry URL intake")
		}
		if j.AnthropicConnectionID != "" && (j.UserID != uid || principal.AgentID != "") {
			return fail(403, "only the user whose Anthropic connection runs this BYOK job can retry it")
		}
		if j.Status != "failed" {
			return fail(409, "only failed jobs can be retried")
		}
		if j.Managed != nil && !j.Managed.CleanupComplete {
			return fail(503, "agent cleanup pending; retry after reconciliation")
		}

		queued := 0
		for _, v := range st.Jobs {
			if v.ProjectID == pid && (v.Kind == "put") == (j.Kind == "put") && (v.Status == "queued" || v.Status == "running") {
				queued++
			}
		}
		for _, u := range st.Uploads {
			if u.ProjectID == pid {
				queued++
			}
		}
		limit := s.Config.MaxQueued
		if j.Kind == "put" {
			limit = s.Config.MaxIntakeJobs
		}
		if queued >= limit {
			return fail(429, "project job queue is full")
		}
		j.Status = "queued"
		j.Managed = nil
		j.Unchanged = false
		j.Dependencies = nil
		if j.Kind != "put" {
			for _, d := range st.Jobs {
				if d.ProjectID == pid && d.Kind == "put" && (d.Status == "queued" || d.Status == "running") {
					j.Dependencies = append(j.Dependencies, d.ID)
				}
			}
		}
		j.Error = ""
		j.UserID = uid
		j.AgentID = principal.AgentID
		st.Jobs[j.ID] = j
		job = j
		return nil
	})
	if err != nil {
		return err
	}
	s.notify()
	s.notifyIntake()
	jsonOut(w, 202, job)
	return nil
}
func (s *Server) sources(w http.ResponseWriter, r *http.Request, uid string) error {
	return s.DB.View(func(st model.State) error {
		pid := r.PathValue("project")
		if _, err := member(st, pid, uid); err != nil {
			return err
		}
		out := []model.Source{}
		for _, src := range st.Sources {
			if src.ProjectID == pid {
				out = append(out, src)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
		jsonOut(w, 200, out)
		return nil
	})
}
func (s *Server) sourceContent(w http.ResponseWriter, r *http.Request, uid string) error {
	var src model.Source
	err := s.DB.View(func(st model.State) error {
		pid := r.PathValue("project")
		if _, err := member(st, pid, uid); err != nil {
			return err
		}
		var ok bool
		src, ok = st.Sources[r.PathValue("source")]
		if !ok || src.ProjectID != pid {
			return fail(404, "source not found")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.PathValue("format") != "text" {
		return fail(404, "unknown source format")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	b, err := s.Blobs.Get(r.Context(), src.TextKey)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, uid string) error {
	if r.URL.Query().Get("manifest") == "1" || r.URL.Query().Get("file") != "" {
		return s.syncSnapshot(w, r, uid)
	}
	var rev model.Revision
	err := s.DB.View(func(st model.State) error {
		pid := r.PathValue("project")
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		rid := p.Revision

		if rid == "" {
			for _, e := range st.Intake {
				if e.ProjectID == pid {
					return nil
				}
			}
			return fail(409, "project has no wiki or intake yet")
		}
		var ok bool
		rev, ok = st.Revisions[rid]
		if !ok || rev.ProjectID != pid {
			return fail(404, "revision not found")
		}
		return nil
	})
	if err != nil {
		return err
	}
	files := map[string]string{}
	total := 0
	for name, key := range rev.Files {
		b, err := s.Blobs.Get(r.Context(), key)
		if err != nil {
			return err
		}
		total += len(b)
		if total > 64<<20 {
			return fail(413, "snapshot exceeds 64 MiB")
		}
		files[name] = string(b)
	}
	if err = s.addIntakeView(r.Context(), uid, r.PathValue("project"), files); err != nil {
		return err
	}
	if requested := r.URL.Query().Get("revision"); requested != "" && requested != snapshotToken(files) {
		return fail(404, "wiki history is not retained")
	}
	jsonOut(w, 200, model.Snapshot{Revision: snapshotToken(files), Files: files})
	return nil
}

// sameSourceIDs compares immutable source snapshots. Observation timestamps and
// the last generated revision may change without changing source identity.
func sameSourceIDs(sources []model.Source, ids []string) bool {
	if len(sources) != len(ids) {
		return false
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	for _, src := range sources {
		if !set[src.ID] {
			return false
		}
		delete(set, src.ID)
	}
	return len(set) == 0
}
