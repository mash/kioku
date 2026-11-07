package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/usd"
)

func (s *Server) generate(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
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
	}
	// A missing body retains the default budget.
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
		job, err = s.queueGeneration(st, p, uid, principal.AgentID, false, time.Now().UTC())
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

func (s *Server) queueGeneration(st *model.State, p model.Project, uid, agentID string, automatic bool, now time.Time) (model.Job, error) {
	job := model.Job{ID: auth.Token()[:20], Kind: "generate", ProjectID: p.ID, UserID: uid, AgentID: agentID, Status: "queued", Created: now, Automatic: automatic}
	pending := 0
	for _, j := range st.Jobs {
		if j.ProjectID != p.ID || (j.Status != "queued" && j.Status != "running") {
			continue
		}
		pending++
	}
	if pending >= s.Config.MaxQueued {
		return job, fail(429, "project generation queue is full")
	}
	st.Jobs[job.ID] = job
	return job, nil
}

// Deadlines are fixed intervals, not a debounce: frequent puts never postpone a
// project's next run. Waiting items survive restarts and failed generations.
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
			dirty := false
			over := completedWikiBytes(st.Revisions[p.Revision]) > s.Config.MaxWikiBytes
			for _, item := range st.Inbox {
				if item.ProjectID == id && item.RunID == "" && (!over || item.Status == "removed") {
					dirty = true
				}
			}
			pendingGeneration := false
			for _, j := range st.Jobs {
				if j.ProjectID == id && j.Kind == "generate" && (j.Status == "queued" || j.Status == "running") {
					pendingGeneration = true
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
			if _, err := s.queueGeneration(st, p, uid, "", true, now); err != nil {
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

// Partial writes must not change which waiting items can rebuild the wiki.
// The removal-only rule takes effect after a completed publication.
func completedWikiBytes(rev model.Revision) int64 {
	if rev.CompleteBytes != nil {
		return *rev.CompleteBytes
	}
	return rev.StoredBytes
}
