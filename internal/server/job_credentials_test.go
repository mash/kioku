package server

import (
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func TestJobCipherChecksCurrentRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*store.JobRow)
		status int
	}{
		{"running", func(*store.JobRow) {}, 0},
		{"completed", func(j *store.JobRow) { j.Status = "completed" }, 403},
		{"stopping", func(j *store.JobRow) { j.Data.Run.AbortReason = sqlTestPtr("revoked") }, 403},
		{"finalizing", func(j *store.JobRow) { j.Phase = sqlTestPtr("finalizing") }, 409},
		{"unprepared", func(j *store.JobRow) { j.InputReady = false }, 409},
		{"file", func(j *store.JobRow) { j.Data.Run.UsesProviderFiles = true }, 409},
		{"parent-wait", func(j *store.JobRow) {
			j.GenerationSnapshot = sqlTestPtr("")
			j.Data.ViewGeneration = &store.GenerationData{}
		}, 409},
		{"no-run", func(j *store.JobRow) {
			j.Phase = nil
			j.Data.Run = nil
			j.CleanupComplete = nil
			j.InputReady = false
			j.GenerationSnapshot = sqlTestPtr("")
			j.Data.ViewGeneration = &store.GenerationData{}
		}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c := sqlAuthFixture(t)
			now := time.UnixMilli(1000)
			job := store.JobRow{ID: "j", ProjectID: "p", Status: "running", Kind: "generate", Phase: sqlTestPtr("preparing"), CleanupComplete: sqlTestPtr(false), InputReady: true, Data: store.JobData{UserID: "u", Run: &store.RunData{PreparedAt: 1000, Deadline: 2000}}}
			tc.mutate(&job)
			sqlWrite(t, d, func(q *store.Queries) error { return q.PutJob(job) })
			raw, err := c.SealJobCredential(auth.JobCredential{JobID: "j", Attempt: "j", Expires: 3000})
			if err != nil {
				t.Fatal(err)
			}
			err = d.Read(t.Context(), func(q *store.Queries) error { _, err := checkSQLJobCredential(q, c, raw, "p", "j", now); return err })
			if tc.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				expectSQLStatus(t, err, tc.status)
			}
			for _, token := range []string{raw + "bad", "kioku_agent.j." + c.Seal(`{"job_id":"j","attempt":"other","expires":3000}`, "job:j")} {
				err = d.Read(t.Context(), func(q *store.Queries) error { _, err := checkSQLJobCredential(q, c, token, "p", "j", now); return err })
				expectSQLStatus(t, err, 403)
			}
			err = d.Read(t.Context(), func(q *store.Queries) error {
				_, err := checkSQLJobCredential(q, c, raw, "p", "j", time.UnixMilli(3000))
				return err
			})
			expectSQLStatus(t, err, 403)
		})
	}
}
