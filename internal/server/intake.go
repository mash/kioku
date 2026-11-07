package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"github.com/mash/kioku/internal/usd"
)

// put acknowledges durable intake independently of wiki generation. URL bodies
// are fetched by bounded background workers, never in the submission request.
func (s *Server) put(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	if err := s.check(pid, uid); err != nil {
		return err
	}
	var req model.PutRequest
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if len(req.Files) == 0 && req.URL == "" || len(req.Files) > 30 || req.URL != "" && len(req.Files) > 0 {
		return fail(400, "provide 1–30 files or one URL")
	}
	_, principal := s.session(r)
	job := model.Job{ID: auth.Token()[:20], Kind: "put", ProjectID: pid, UserID: uid, AgentID: principal.AgentID, Created: time.Now().UTC()}
	if req.URL != "" {
		if principal.AgentID != "" {
			return fail(403, "agents put files only; provider connections are not delegated")
		}
		u, err := url.Parse(req.URL)
		if err != nil || len(req.URL) > 4096 {
			return fail(400, "invalid source URL")
		}
		if err = source.ValidateURL(u); err != nil {
			return fail(400, err.Error())
		}
		job.InputURL = req.URL
		job.Status = "queued"
		err = s.DB.Update(func(st *model.State) error {
			p, err := member(*st, pid, uid)
			if err != nil {
				return err
			}
			pending := 0
			for _, j := range st.Jobs {
				if j.ProjectID == pid && j.Kind == "put" && (j.Status == "queued" || j.Status == "running") {
					pending++
				}
			}
			if pending >= s.Config.MaxIntakeJobs {
				return fail(429, "project intake queue is full")
			}
			p.NextPutSequence++
			job.PutSequence = p.NextPutSequence
			st.Projects[pid] = p
			st.Jobs[job.ID] = job
			recordIntake(st, job, "submitted", nil)
			return nil
		})
		if err != nil {
			return err
		}
		s.notifyIntake()
	} else {
		docs := make([]source.Document, 0, len(req.Files))
		for _, f := range req.Files {
			doc, err := source.FileWithURL(f.Name, f.Content, f.SourceURL)
			if err != nil {
				return fail(400, fmt.Sprintf("%s: %v", f.Name, err))
			}
			docs = append(docs, doc)
		}
		var err error
		job, err = s.storeSources(r.Context(), docs, job)
		if err != nil {
			return err
		}
	}
	jsonOut(w, http.StatusAccepted, job)
	return nil
}

func (s *Server) generate(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Full        bool  `json:"full"`
		BudgetCents int64 `json:"budget_cents,omitempty"`
	}
	form := strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	if form {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			return fail(400, "invalid generation form")
		}
		micros, err := usd.Parse(r.FormValue("budget_usd"))
		if err != nil || micros%10000 != 0 || micros < 500000 {
			return fail(400, "invalid generation budget")
		}
		req.BudgetCents = micros / 10000
		req.Full = r.FormValue("full") == "true"
	}
	// A missing body retains the original incremental request semantics.
	if !form && r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil && err != io.EOF {
			return fail(400, "invalid generation request")
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return fail(400, "unexpected trailing JSON")
		}
	}
	_, principal := s.session(r)
	var job model.Job
	err := s.DB.Update(func(st *model.State) error {
		p, err := member(*st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		if req.BudgetCents != 0 && (req.BudgetCents < 50 || req.BudgetCents > s.Config.AgentMaxSessionCents) {
			return fail(400, "budget is outside the allowed range")
		}
		job, err = s.queueGeneration(st, p, uid, principal.AgentID, false, req.Full, time.Now().UTC())
		if err == nil && req.BudgetCents != 0 {
			job.AgentBudgetCents = req.BudgetCents
			st.Jobs[job.ID] = job
		}
		return err
	})
	if err != nil {
		return err
	}
	s.notify()
	if form {
		return render(w, "生成開始", struct{ ProjectID, JobID string }{job.ProjectID, job.ID}, `{{define "body"}}<h1>生成ジョブを開始しました</h1><p>サーバーに設定された生成処理を使用します。</p><p><a href="/api/projects/{{.ProjectID}}/jobs/{{.JobID}}">ジョブの状態を確認</a></p><p><a href="/">プロジェクト一覧へ</a></p>{{end}}`)
	}
	jsonOut(w, http.StatusAccepted, job)
	return nil
}

func (s *Server) queueGeneration(st *model.State, p model.Project, uid, agentID string, automatic, full bool, now time.Time) (model.Job, error) {
	job := model.Job{ID: auth.Token()[:20], Kind: "generate", ProjectID: p.ID, UserID: uid, AgentID: agentID, Status: "queued", Created: now, Automatic: automatic, Full: full}
	pending := 0
	for _, j := range st.Jobs {
		if j.ProjectID != p.ID || (j.Status != "queued" && j.Status != "running") {
			continue
		}
		if j.Kind == "put" {
			job.Dependencies = append(job.Dependencies, j.ID)
		} else {
			pending++
		}
	}
	if pending >= s.Config.MaxQueued {
		return job, fail(429, "project generation queue is full")
	}
	sort.Strings(job.Dependencies)
	st.Jobs[job.ID] = job
	return job, nil
}

// Deadlines are fixed intervals, not a debounce: frequent puts never postpone a
// project's next run. Dirty versions survive restarts and failed generations.
func (s *Server) scheduleGenerations(now time.Time) error {
	if s.Config.GenerateEvery <= 0 {
		return s.DB.Update(func(st *model.State) error {
			for id, p := range st.Projects {
				if !p.NextGenerateAt.IsZero() || p.GenerateIntervalSeconds != 0 {
					p.NextGenerateAt = time.Time{}
					p.GenerateIntervalSeconds = 0
					st.Projects[id] = p
				}
			}
			return nil
		})
	}
	interval := s.Config.GenerateEvery
	err := s.DB.Update(func(st *model.State) error {
		for id, p := range st.Projects {
			if p.Deleting {
				continue
			}
			if p.NextGenerateAt.IsZero() || p.GenerateIntervalSeconds != int64(interval/time.Second) {
				p.NextGenerateAt = now.Add(interval)
				p.GenerateIntervalSeconds = int64(interval / time.Second)
				st.Projects[id] = p
				continue
			}
			if now.Before(p.NextGenerateAt) {
				continue
			}
			p.NextGenerateAt = now.Add(interval)
			st.Projects[id] = p
			dirty := p.SourceVersion > p.WikiSourceVersion
			// Pre-versioning projects may have accepted sources but no published wiki.
			if p.SourceVersion == 0 && p.Revision == "" {
				for _, src := range st.Sources {
					if src.ProjectID == id {
						dirty = true
						break
					}
				}
			}
			pendingGeneration := false
			for _, j := range st.Jobs {
				if j.ProjectID == id && (j.Status == "queued" || j.Status == "running") {
					if j.Kind == "put" {
						dirty = true
					} else {
						pendingGeneration = true
					}
				}
			}
			if !dirty || pendingGeneration {
				continue
			}
			uid := ""
			for member, role := range p.Members {
				if role == "owner" {
					uid = member
					break
				}
			}
			if uid == "" {
				continue
			}
			if _, err := s.queueGeneration(st, p, uid, "", true, false, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.notify()
	}
	return err
}

func (s *Server) notifyIntake() {
	for i := 0; i < s.Config.IntakeWorkers; i++ {
		select {
		case s.intakeWake <- struct{}{}:
		default:
		}
	}
}

func (s *Server) runIntake(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.intakeWake:
		case <-ticker.C:
		}
		for ctx.Err() == nil {
			var job model.Job
			err := s.DB.Update(func(st *model.State) error {
				for _, j := range st.Jobs {
					if j.Kind == "put" && j.Status == "queued" && (job.ID == "" || j.Created.Before(job.Created) || j.Created.Equal(job.Created) && j.ID < job.ID) {
						job = j
					}
				}
				if job.ID != "" {
					job.Status = "running"
					st.Jobs[job.ID] = job
				}
				return nil
			})
			if err != nil {
				slog.Error("intake queue deferred", "error", err)
				break
			}
			if job.ID == "" {
				break
			}
			jobCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err = s.processPut(jobCtx, job)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				updateErr := s.DB.Update(func(st *model.State) error {
					j, ok := st.Jobs[job.ID]
					if !ok || j.Status != "running" {
						return nil
					}
					j.Status = "failed"
					j.Error = err.Error()
					st.Jobs[j.ID] = j
					return nil
				})
				if updateErr != nil {
					slog.Error("intake result deferred", "error", updateErr)
				}
			}
			s.notify()
		}
	}
}

func (s *Server) processPut(ctx context.Context, job model.Job) error {
	if err := s.check(job.ProjectID, job.UserID); err != nil {
		return err
	}
	if job.AgentID != "" {
		return fail(403, "agents cannot fetch URLs")
	}
	provider := source.Kind(job.InputURL)
	token := ""
	if provider != "web" {
		var err error
		token, err = s.providerCredential(ctx, job.UserID, provider)
		if err != nil {
			return err
		}
	}
	doc, err := s.Fetcher.Fetch(ctx, job.InputURL, token)
	if err != nil {
		return err
	}
	_, err = s.storeSources(ctx, []source.Document{doc}, job)
	return err
}

func recordIntake(st *model.State, job model.Job, kind string, sources []model.Source) {
	id := job.ID + "-" + kind
	if st.Intake == nil {
		st.Intake = map[string]model.IntakeEntry{}
	}
	if _, ok := st.Intake[id]; ok {
		return
	}
	p := st.Projects[job.ProjectID]
	p.NextIntakeSequence++
	st.Intake[id] = model.IntakeEntry{ID: id, ProjectID: p.ID, JobID: job.ID, UserID: job.UserID, Sequence: p.NextIntakeSequence, Kind: kind, URL: job.InputURL, Sources: sources, SourceVersion: p.SourceVersion, Created: time.Now().UTC()}
	st.Projects[p.ID] = p
}
