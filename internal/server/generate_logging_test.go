package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/mash/kioku/internal/store"
)

func TestGenerationSignalsOnlyPersistedChanges(t *testing.T) {
	t.Parallel()
	for _, queue := range []bool{false, true} {
		t.Run(fmt.Sprint(queue), func(t *testing.T) {
			f := setupWithWorker(t, false)
			token := f.login("owner@example.com")
			p := securityProject(t, f, token)
			if queue {
				putFile(t, f, token, p.ID, "a.md", "Body")
			}
			now := time.Now()
			fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
				p, e := q.Project(p.ID)
				if e != nil {
					return e
				}
				n := now.UnixMilli()
				p.NextGenerateAt = &n
				return q.PutProject(p)
			}))
			for len(f.s.wake) > 0 {
				<-f.s.wake
			}
			restore := fixtureSQLFault(t, f, "projects", "UPDATE", "1")
			for range 2 {
				if err := f.s.scheduleGenerations(now); err == nil {
					t.Fatal("expected persistence failure")
				}
			}
			if len(f.s.wake) != 0 {
				t.Fatal("failed commit woke worker")
			}
			restore()
			fixtureCheck(t, f.s.scheduleGenerations(now))
			if (len(f.s.wake) > 0) != queue {
				t.Fatal("wrong wake", len(f.s.wake))
			}
			fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
				jobs, err := q.ProjectJobs(p.ID)
				want := 0
				if queue {
					want = 1
				}
				if len(jobs) != want {
					t.Fatal(jobs)
				}
				return err
			}))
		})
	}
}
