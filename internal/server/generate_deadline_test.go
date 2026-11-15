package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/mash/kioku/internal/store"
)

// E2: selection uses persisted millisecond deadlines, including exact equality.
func TestDeadlineStage(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 15, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		deadline       time.Time
		deleting, want bool
	}{
		{"none", time.Time{}, false, false}, {"before", now.Add(time.Millisecond), false, false},
		{"exact", now, false, true}, {"after", now.Add(-time.Millisecond), false, true}, {"deleting", now.Add(-time.Second), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, p := orgProject(t)
			fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
				r, e := q.Project(p.ID)
				if e != nil {
					return e
				}
				r.Deleting = tc.deleting
				if !tc.deadline.IsZero() {
					n := tc.deadline.UnixMilli()
					r.NextGenerateAt = &n
				}
				return q.PutProject(r)
			}))
			fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
				rows, e := q.DueProjects(now.UnixMilli())
				if (len(rows) == 1) != tc.want {
					t.Fatalf("due=%v want %v", rows, tc.want)
				}
				return e
			}))
		})
	}
}
func TestDeadlineAction(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, eligible := range []bool{false, true} {
			for _, hasOwner := range []bool{false, true} {
				t.Run(fmt.Sprintf("busy=%v eligible=%v owner=%v", busy, eligible, hasOwner), func(t *testing.T) {
					f, token, p := orgProject(t)
					now := time.Now().UTC()
					if eligible {
						putFile(t, f, token, p.ID, "source.md", "Evidence")
					}
					fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
						r, e := q.Project(p.ID)
						if e != nil {
							return e
						}
						n := now.UnixMilli()
						r.NextGenerateAt = &n
						if e = q.PutProject(r); e != nil {
							return e
						}
						if !hasOwner {
							for id := range p.Members {
								if e = q.DeleteProjectMember(p.ID, id); e != nil {
									return e
								}
							}
						}
						if busy {
							return q.PutJob(store.JobRow{ID: "busy", Kind: "generate", ProjectID: p.ID, Status: "queued"})
						}
						return nil
					}))
					fixtureCheck(t, f.s.scheduleGenerations(now))
					fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
						r, e := q.Project(p.ID)
						if e != nil {
							return e
						}
						if (r.NextGenerateAt != nil) != busy {
							t.Fatal("wrong deadline", r.NextGenerateAt)
						}
						jobs, e := q.ProjectJobs(p.ID)
						want := 0
						if busy || eligible && hasOwner {
							want = 1
						}
						if len(jobs) != want {
							t.Fatalf("jobs=%d want=%d", len(jobs), want)
						}
						return e
					}))
				})
			}
		}
	}
}
