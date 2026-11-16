package server

import (
	"database/sql"
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
	if err = sqlJobAuthorization(q, job, now.UnixMilli()); err != nil {
		var api apiError
		if errors.As(err, &api) {
			return store.JobRow{}, denied
		}
		return store.JobRow{}, err
	}
	if !job.InputReady || job.Data.Run.UsesProviderFiles || job.GenerationSnapshot != nil || job.Phase == nil || *job.Phase == "finalizing" {
		return store.JobRow{}, fail(409, "job input and output are not available")
	}
	return job, nil
}
