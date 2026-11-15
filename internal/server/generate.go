package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
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
	_, principal, err := s.session(r)
	if err != nil {
		return err
	}
	var job model.Job
	err = s.mutation(r, uid, func(q *store.Queries) error {
		p, err := sqlMember(q, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		if req.BudgetCents != 0 && (req.BudgetCents < 50 || req.BudgetCents > s.Config.AgentMaxSessionCents) {
			return fail(400, "budget is outside the allowed range")
		}
		row, err := s.queueGeneration(q, p, uid, principal.AgentID, false, time.Now())
		if err != nil {
			return err
		}
		row.Data.AgentBudgetCents = req.BudgetCents
		if err = q.PutJob(row); err != nil {
			return err
		}
		job, err = jobProjection(q, row)
		return err
	})
	if err != nil {
		return err
	}
	s.notify()
	if form {
		return render(w, "生成開始", struct{ Namespace, JobID string }{r.PathValue("org") + "/" + r.PathValue("slug"), job.ID}, `{{define "body"}}<h1>生成ジョブを開始しました</h1><p>サーバーに設定された生成処理を使用します。</p><p><a href="/api/projects/{{.Namespace}}/jobs/{{.JobID}}">ジョブの状態を確認</a></p><p><a href="/">プロジェクト一覧へ</a></p>{{end}}`, headerFor(r, uid))
	}
	jsonOut(w, http.StatusAccepted, job)
	return nil
}

func (s *Server) queueGeneration(q *store.Queries, p store.ProjectRow, uid, agentID string, automatic bool, now time.Time) (store.JobRow, error) {
	job := store.JobRow{ID: auth.Token()[:20], Kind: "generate", ProjectID: p.ID, Status: "queued", Created: now.UnixMilli(), Data: store.JobData{UserID: uid, AgentID: nullableString(agentID), Automatic: automatic, WikiStatus: nullableString("pending"), Views: []store.ViewResultData{}}}
	rows, err := q.ProjectJobs(p.ID)
	if err != nil {
		return job, err
	}
	pending := 0
	for _, j := range rows {
		if j.ParentID == nil && (j.Status == "queued" || j.Status == "running") {
			pending++
		}
	}
	if pending >= s.Config.MaxQueued {
		return job, fail(429, "project generation queue is full")
	}
	return job, q.PutJob(job)
}
func (s *Server) scheduleGenerations(now time.Time) error {
	queued := false
	err := s.Store.Write(context.Background(), func(q *store.Queries) error {
		projects, err := q.DueProjects(now.UnixMilli())
		if err != nil {
			return err
		}
		for _, p := range projects {
			jobs, err := q.ProjectJobs(p.ID)
			if err != nil {
				return err
			}
			busy := false
			for _, j := range jobs {
				if j.Kind == "generate" && (j.Status == "queued" || j.Status == "running") {
					busy = true
				}
			}
			if busy {
				continue
			}
			files, err := q.ProjectWiki(p.ID)
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
			items, err := q.AvailableInbox(p.ID, size > s.Config.MaxWikiBytes)
			if err != nil {
				return err
			}
			members, err := q.ProjectMembers(p.ID)
			if err != nil {
				return err
			}
			owner := ""
			for uid, role := range members {
				if role == "owner" {
					owner = uid
					break
				}
			}
			p.NextGenerateAt = nil
			if err = q.PutProject(p); err != nil {
				return err
			}
			if len(items) == 0 || owner == "" {
				continue
			}
			if _, err = q.User(owner); errors.Is(err, sql.ErrNoRows) {
				continue
			} else if err != nil {
				return err
			}
			if _, err = s.queueGeneration(q, p, owner, "", true, now); err != nil {
				return err
			}
			queued = true
		}
		return nil
	})
	if err == nil && queued {
		s.notify()
	}
	return err
}
