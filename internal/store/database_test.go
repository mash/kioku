package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func newCatalog(t *testing.T) *Database {
	t.Helper()
	d, err := OpenDatabase(filepath.Join(t.TempDir(), "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func catalogWrite(t *testing.T, d *Database, fn func(*Queries) error) {
	t.Helper()
	if err := d.Write(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}
func testJob(id, project, status string, created int64) JobRow {
	return JobRow{ID: id, ProjectID: project, Status: status, Created: created, Kind: "generate", Data: JobData{UserID: "u1", Views: []ViewResultData{}}}
}
func pointer[T any](v T) *T { return &v }

func TestCatalogSchemaHasExactlyFifteenTablesAndSeventyEightColumns(t *testing.T) {
	d := newCatalog(t)
	var tables, columns int
	if err := d.reader.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := d.reader.QueryRow("SELECT count(*) FROM sqlite_master AS m JOIN pragma_table_info(m.name) AS p WHERE m.type='table' AND m.name NOT LIKE 'sqlite_%'").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if tables != 15 || columns != 78 {
		t.Fatalf("tables=%d columns=%d", tables, columns)
	}
	for _, tc := range []struct {
		db     *sql.DB
		pragma string
		want   string
	}{{d.writer, "journal_mode", "wal"}, {d.writer, "synchronous", "2"}, {d.writer, "secure_delete", "1"}, {d.writer, "busy_timeout", "5000"}, {d.reader, "query_only", "1"}, {d.reader, "busy_timeout", "5000"}} {
		var got string
		if err := tc.db.QueryRow("PRAGMA " + tc.pragma).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("%s=%s want %s: %v", tc.pragma, got, tc.want, err)
		}
	}
	var equal bool
	if err := d.reader.QueryRow("SELECT kioku_equal_fold('K','k')").Scan(&equal); err != nil || !equal {
		t.Fatal(equal, err)
	}
}
func TestSchemaRejectsInvalidRows(t *testing.T) {
	d := newCatalog(t)
	for _, query := range []string{
		`INSERT INTO users VALUES('u','a','[]')`,
		`INSERT INTO organization_members VALUES('o','u','admin')`,
		`INSERT INTO projects VALUES('p','o','p',0,2,NULL,NULL,'{}')`,
		`INSERT INTO sessions VALUES('s','user','pending','u',NULL,10,0,'{}')`,
		`INSERT INTO sessions VALUES('s','agent','active','',NULL,10,0,'{}')`,
		`INSERT INTO usage VALUES('p',NULL,0,'{}')`,
		`INSERT INTO wiki_files VALUES('p','wiki/a','bad','{"bytes":1}')`,
	} {
		if _, err := d.writer.Exec(query); err == nil {
			t.Errorf("accepted %s", query)
		}
	}
	for _, mutate := range []func(*JobRow){
		func(j *JobRow) { j.Status = "running" },
		func(j *JobRow) { j.Phase = pointer("preparing"); j.CleanupComplete = false },
		func(j *JobRow) { j.Data.Run = &RunData{} },
		func(j *JobRow) { j.InputReady = true },
		func(j *JobRow) { j.SessionID = pointer("s") },
		func(j *JobRow) { j.GenerationSnapshot = pointer("") },
		func(j *JobRow) { j.Data.ViewTask = &ViewTaskData{} },
		func(j *JobRow) { j.ParentID = pointer("p") },
	} {
		j := testJob("j", "p", "queued", 0)
		mutate(&j)
		if err := d.Write(t.Context(), func(q *Queries) error { return q.PutJob(j) }); err == nil {
			t.Fatalf("invalid null invariant accepted: %+v", j)
		}
	}
	// A missing JSON key must not pass a CHECK through SQL NULL.
	_, err := d.writer.Exec(`INSERT INTO jobs(id,project_id,status,created,kind,input_ready,data) VALUES('missing','p','queued',0,'generate',0,'{}')`)
	if err == nil {
		t.Fatal("missing null keys accepted")
	}
}
func TestReadSnapshotDoesNotMixCommits(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		return q.PutUser(UserRow{ID: "u", Email: "old", Data: UserData{Name: "Before"}})
	})
	if err := d.Read(t.Context(), func(q *Queries) error {
		before, err := q.User("u")
		if err != nil {
			return err
		}
		if err := d.Write(t.Context(), func(w *Queries) error {
			return w.PutUser(UserRow{ID: "u", Email: "new", Data: UserData{Name: "After"}})
		}); err != nil {
			return err
		}
		// Another reader must see the new commit without waiting for this snapshot.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := d.Read(ctx, func(other *Queries) error {
			latest, err := other.User("u")
			if err == nil && latest.Email != "new" {
				return fmt.Errorf("new reader saw %q", latest.Email)
			}
			return err
		}); err != nil {
			return err
		}
		after, err := q.User("u")
		if err != nil {
			return err
		}
		if before != after {
			t.Fatalf("mixed snapshot: %+v %+v", before, after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Read(t.Context(), func(q *Queries) error {
		u, err := q.User("u")
		if err == nil && u.Email != "new" {
			t.Fatal(u)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestExchangeChangesOneRow(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		return q.PutSession(SessionRow{ID: "s", Kind: "oauth_code", Status: "pending", UserID: "u", Expires: 100, Data: json.RawMessage(`{}`)})
	})
	winners := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := d.Write(t.Context(), func(q *Queries) error {
				ok, err := q.ConsumeOAuthCode("s", 99)
				if ok {
					winners <- struct{}{}
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatal(len(winners))
	}
}
func TestDatabaseFailureDoesNotPoisonOtherOperations(t *testing.T) {
	d := newCatalog(t)
	err := d.Write(t.Context(), func(q *Queries) error {
		if err := q.PutUser(UserRow{ID: "u", Email: "x"}); err != nil {
			return err
		}
		_, err := q.exec("INSERT INTO missing VALUES(1)")
		return err
	})
	if err == nil {
		t.Fatal("failure not reported")
	}
	if err := d.Read(t.Context(), func(q *Queries) error {
		_, err := q.User("u")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("partial write", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	catalogWrite(t, d, func(q *Queries) error { return q.PutUser(UserRow{ID: "other", Email: "ok"}) })
}
func TestUsageInsertPreservesFirstNull(t *testing.T) {
	d := newCatalog(t)
	u := UsageRow{JobID: "j", ProjectID: "p", RecordedAt: 1, Data: model.UsageValues{OrgID: "o", UserID: "u"}}
	catalogWrite(t, d, func(q *Queries) error { return q.RecordUsage(u) })
	u.RecordedAt = 2
	u.Data.CostCents = pointer(int64(40))
	u.Data.UserID = "changed"
	catalogWrite(t, d, func(q *Queries) error { return q.RecordUsage(u) })
	if err := d.Read(t.Context(), func(q *Queries) error {
		got, err := q.Usage("j")
		if err == nil && (got.Data.CostCents != nil || got.RecordedAt != 1 || got.Data.UserID != "u") {
			t.Fatal(got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestHistoryKeepsPendingCleanupAndUnconsumedChildren(t *testing.T) {
	for _, count := range []int{255, 256, 257} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			d := newCatalog(t)
			catalogWrite(t, d, func(q *Queries) error {
				for i := 1; i <= count; i++ {
					id := fmt.Sprintf("j%03d", i)
					if err := q.PutJob(testJob(id, "p", "completed", int64(i))); err != nil {
						return err
					}
					if err := q.PutAudit(AuditRow{ID: id, ProjectID: "p", Created: int64(i)}); err != nil {
						return err
					}
					if err := q.RecordUsage(UsageRow{JobID: id, ProjectID: "p", RecordedAt: 1}); err != nil {
						return err
					}
				}
				cleanup := testJob("cleanup", "p", "failed", 0)
				cleanup.Phase = pointer("preparing")
				cleanup.CleanupComplete = false
				cleanup.Data.Run = &RunData{}
				if err := q.PutJob(cleanup); err != nil {
					return err
				}
				parent := testJob("parent", "p", "running", 0)
				parent.GenerationSnapshot = pointer("")
				parent.Data.ViewGeneration = &GenerationData{}
				if err := q.PutJob(parent); err != nil {
					return err
				}
				child := testJob("child", "p", "completed", 0)
				child.ParentID = pointer("parent")
				child.Data.ViewTask = &ViewTaskData{ViewID: "v"}
				if err := q.PutJob(child); err != nil {
					return err
				}
				if err := q.PutJob(testJob("other", "p2", "completed", 0)); err != nil {
					return err
				}
				return q.CompactHistory("p", nil)
			})
			if err := d.Read(t.Context(), func(q *Queries) error {
				jobs, err := q.ProjectJobs("p")
				if err != nil {
					return err
				}
				if len(jobs) != min(count, 256)+3 {
					t.Fatalf("jobs=%d", len(jobs))
				}
				audit, err := q.ProjectAudit("p")
				if err != nil {
					return err
				}
				if len(audit) != min(count, 256) {
					t.Fatal(len(audit))
				}
				for i := 1; i <= count; i++ {
					if _, err := q.Usage(fmt.Sprintf("j%03d", i)); err != nil {
						return err
					}
				}
				for _, id := range []string{"cleanup", "parent", "child", "other"} {
					if _, err := q.Job(id); err != nil {
						return err
					}
				}
				_, err = q.Job("j001")
				if count == 257 && !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("oldest survived", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestHistoryMaintenanceAgeBoundaries(t *testing.T) {
	d := newCatalog(t)
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).UnixMilli()
	catalogWrite(t, d, func(q *Queries) error {
		for _, delta := range []int64{-1, 0, 1} {
			id := strconv.FormatInt(delta, 10)
			if err := q.PutJob(testJob(id, "p", "completed", now-int64(30*24*time.Hour/time.Millisecond)+delta)); err != nil {
				return err
			}
			if err := q.PutAudit(AuditRow{ID: id, ProjectID: "p", Created: now - int64(90*24*time.Hour/time.Millisecond) + delta}); err != nil {
				return err
			}
		}
		return q.CompactHistory("p", &now)
	})
	if err := d.Read(t.Context(), func(q *Queries) error {
		jobs, err := q.ProjectJobs("p")
		if err != nil {
			return err
		}
		audit, err := q.ProjectAudit("p")
		if len(jobs) != 2 || len(audit) != 2 {
			t.Fatal(len(jobs), len(audit))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestCatalogQueriesUseDeclaredIndexes(t *testing.T) {
	d := newCatalog(t)
	for _, tc := range []struct{ query, index string }{
		{"SELECT id FROM jobs INDEXED BY jobs_queue WHERE status='queued' ORDER BY created,id LIMIT 1", "jobs_queue"},
		{"SELECT id FROM projects WHERE next_generate_at IS NOT NULL AND deleting=0 AND next_generate_at<=100 ORDER BY next_generate_at,id", "projects_due"},
		{"SELECT id FROM sessions WHERE user_id='u' AND kind='user' ORDER BY expires,id", "sessions_user"},
		{"SELECT id FROM jobs WHERE session_id='s'", "jobs_session"},
	} {
		rows, err := d.reader.Query("EXPLAIN QUERY PLAN " + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			found = found || strings.Contains(detail, tc.index)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !found {
			t.Fatal(tc.index, err)
		}
	}
}

func TestCleanupDefaultAndUnconsumedChildConstraints(t *testing.T) {
	d := newCatalog(t)
	raw, err := marshalData(JobData{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec(`INSERT INTO jobs(id,project_id,status,created,kind,input_ready,data) VALUES('default','p','queued',0,'generate',0,?)`, raw); err != nil {
		t.Fatal(err)
	}
	if err := d.Read(t.Context(), func(q *Queries) error {
		j, err := q.Job("default")
		if j.CleanupComplete {
			t.Fatal("cleanup default is not false")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"NULL", "2", "1"} {
		if _, err := d.writer.Exec("UPDATE jobs SET cleanup_complete=" + value + " WHERE id='default'"); err == nil {
			t.Fatalf("accepted cleanup_complete=%s without Run", value)
		}
	}
	child := testJob("child", "p", "queued", 0)
	child.ParentID = pointer("parent")
	child.Data.ViewTask = &ViewTaskData{ViewID: "v"}
	catalogWrite(t, d, func(q *Queries) error { return q.PutJob(child) })
	other := child
	other.ID = "other"
	if err := d.Write(t.Context(), func(q *Queries) error { return q.PutJob(other) }); err == nil {
		t.Fatal("accepted two unconsumed children")
	}
	catalogWrite(t, d, func(q *Queries) error {
		child.Status = "completed"
		child.ParentID = nil
		child.Data.ViewTask = nil
		if err := q.PutJob(child); err != nil {
			return err
		}
		return q.PutJob(other)
	})
}

func TestReadinessUsesVersionForStartupAndHealth(t *testing.T) {
	for _, version := range []int{0, 2, 3, 4} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			d := newCatalog(t)
			if _, err := d.writer.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			want := version == 3
			if err := d.validateCatalog(); (err == nil) != want {
				t.Fatal("startup", err)
			}
			if err := d.Read(t.Context(), func(q *Queries) error { return q.Healthy() }); (err == nil) != want {
				t.Fatal("health", err)
			}
		})
	}
}
