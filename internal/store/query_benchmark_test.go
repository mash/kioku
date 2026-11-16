package store

import (
	"path/filepath"
	"strconv"
	"testing"
)

// This is a local allocation/latency sample, not a production performance claim.
// The operation issues three indexed queries regardless of unrelated projects.
func BenchmarkProjectLookupWithUnrelatedProjects(b *testing.B) {
	for _, unrelated := range []int{0, 1000} {
		b.Run(strconv.Itoa(unrelated), func(b *testing.B) {
			d, err := OpenDatabase(filepath.Join(b.TempDir(), "catalog.sqlite"), true)
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			if err := d.Write(b.Context(), func(q *Queries) error {
				if err := q.PutProject(ProjectRow{ID: "target", OrgID: "o", Slug: "target"}); err != nil {
					return err
				}
				if err := q.PutProjectMember("target", "u", "owner"); err != nil {
					return err
				}
				if err := q.PutInbox(InboxRow{ID: "i", ProjectID: "target", SourceURL: "source"}); err != nil {
					return err
				}
				for i := 0; i < unrelated; i++ {
					id := strconv.Itoa(i)
					if err := q.PutProject(ProjectRow{ID: id, OrgID: "o", Slug: "project-" + id}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := d.Read(b.Context(), func(q *Queries) error {
					if _, err := q.Project("target"); err != nil {
						return err
					}
					if _, err := q.ProjectRole("target", "u"); err != nil {
						return err
					}
					_, err := q.AvailableInbox("target", false)
					return err
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRecentGenerationsWithHistory(b *testing.B) {
	for _, history := range []int{5, 256} {
		b.Run(strconv.Itoa(history), func(b *testing.B) {
			d, err := OpenDatabase(filepath.Join(b.TempDir(), "catalog.sqlite"), true)
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			if err := d.Write(b.Context(), func(q *Queries) error {
				for i := range history {
					if err := q.PutJob(testJob(strconv.Itoa(i), "p", "completed", int64(i))); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := d.Read(b.Context(), func(q *Queries) error {
					_, err := q.RecentGenerationJobs("p", 5)
					return err
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
