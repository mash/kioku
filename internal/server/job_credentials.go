package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func checkSQLJobCredential(q *store.Queries, cipher *auth.Cipher, token, pid, id string, now time.Time) (store.JobRow, error) {
	denied := fail(403, "invalid job credential")
	payload, err := cipher.OpenJobCredential(token, now.UnixMilli())
	if err != nil || payload.JobID != id {
		return store.JobRow{}, denied
	}
	job, err := q.Job(id)
	if errors.Is(err, sql.ErrNoRows) {
		return job, denied
	}
	if err != nil {
		return job, err
	}
	if job.ProjectID != pid || job.Status != "running" || job.Data.Run == nil || payload.Attempt != job.ID || job.Data.Run.AbortReason != nil {
		return store.JobRow{}, denied
	}
	if _, err = sqlMember(q, pid, job.Data.UserID); err != nil {
		var api apiError
		if errors.As(err, &api) {
			return store.JobRow{}, denied
		}
		return store.JobRow{}, err
	}
	if job.Data.AgentID != nil {
		agents, err := q.ProjectSessions(pid, "agent")
		if err != nil {
			return store.JobRow{}, err
		}
		allowed := false
		for _, v := range agents {
			var a store.AgentSessionData
			if err = json.Unmarshal(v.Data, &a); err != nil {
				return store.JobRow{}, err
			}
			if a.AgentID != nil && *a.AgentID == *job.Data.AgentID && v.UserID == job.Data.UserID && v.Status == "active" && v.Expires > now.UnixMilli() && a.RevokedAt == nil && a.Role != nil && *a.Role == "write" {
				allowed = true
				break
			}
		}
		if !allowed {
			return store.JobRow{}, denied
		}
	}
	if !job.InputReady || job.Data.Run.UsesProviderFiles || job.GenerationSnapshot != nil || job.Phase == nil || *job.Phase == "finalizing" {
		return store.JobRow{}, fail(409, "job input and output are not available")
	}
	return job, nil
}
