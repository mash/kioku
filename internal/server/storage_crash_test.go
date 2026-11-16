package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

// Crash points use a separate test process and os.Exit: no transaction rollback,
// database close, gate release, or test cleanup can mask the interrupted state.
type crashCatalog struct {
	catalog
	stage string
}

func (d crashCatalog) Write(ctx context.Context, fn func(*store.Queries) error) error {
	hit := false
	err := d.catalog.Write(ctx, func(q *store.Queries) error {
		if err := fn(q); err != nil {
			return err
		}
		part := strings.Split(d.stage, "/")[0]
		j, je := q.Job("j")
		p, pe := q.Project("p")
		switch part {
		case "inbox":
			_, e := q.Inbox("new")
			hit = e == nil
		case "input":
			hit = je == nil && j.Status == "running"
		case "output":
			hit = je == nil && j.OutputHash != nil
		case "wiki":
			hit = pe == nil && je == nil && j.Status == "completed"
		case "view":
			hit = je == nil && j.Status == "completed"
		case "cleanup":
			hit = je == nil && j.CleanupComplete
		case "delete":
			if strings.Contains(d.stage, "references") {
				rows, e := q.ProjectWiki("p")
				hit = pe == nil && p.Deleting && e == nil && len(rows) == 0
			} else {
				hit = errors.Is(pe, sql.ErrNoRows)
			}
		}
		if hit && strings.HasSuffix(d.stage, "-before") {
			os.Exit(91)
		}
		return nil
	})
	if err == nil && hit && strings.HasSuffix(d.stage, "-after") {
		os.Exit(91)
	}
	return err
}

type crashObjects struct {
	blob.Store
	stage string
}

func (b crashObjects) Put(ctx context.Context, key blob.Key, body []byte) error {
	if err := b.Store.Put(ctx, key, body); err != nil {
		return err
	}
	if strings.HasSuffix(b.stage, "/put") {
		os.Exit(91)
	}
	return nil
}
func (b crashObjects) Delete(ctx context.Context, key blob.Key) error {
	if err := b.Store.Delete(ctx, key); err != nil {
		return err
	}
	if strings.HasSuffix(b.stage, "/delete") {
		os.Exit(91)
	}
	return nil
}
func openCrashStorage(t *testing.T, root string) (*store.Database, *storageOperations) {
	t.Helper()
	objects := local.Store{Dir: filepath.Join(root, "objects")}
	db, _, err := store.OpenStorage(t.Context(), filepath.Join(root, "catalog"), objects, func(bool) (*auth.Cipher, error) {
		return auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	})
	if err != nil {
		t.Fatal(err)
	}
	o := newStorageOperations(db, objects, &Config{MaxWikiBytes: 64 << 20, GenerateDebounce: 10 * time.Second, AgentJobTimeout: time.Hour, AgentMaxSessions: 4, AgentMaxSessionCents: 1000})
	o.now = func() time.Time { return time.UnixMilli(1000) }
	return db, o
}
func seedCrashStorage(t *testing.T, root, stage string) {
	db, o := openCrashStorage(t, root)
	defer db.Close()
	body := []byte("old wiki")
	inbox := store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/old", Data: store.InboxData{Submitter: "u"}}
	for k, b := range map[blob.Key][]byte{model.WikiContentKey("p", model.HashContent(body)): body, sqlInboxKey(inbox): []byte("source"), sqlJobKey(store.JobRow{ProjectID: "p", ID: "j"}, "input.tar.gz"): []byte("input")} {
		if err := o.objects.Put(t.Context(), k, b); err != nil {
			t.Fatal(err)
		}
	}
	sqlWrite(t, db, func(q *store.Queries) error {
		for _, fn := range []func() error{
			func() error { return q.PutUser(store.UserRow{ID: "u", Email: "u@example.com"}) },
			func() error { return q.PutOrganization(store.OrganizationRow{ID: "o", Slug: "org"}) },
			func() error {
				return q.PutProject(store.ProjectRow{ID: "p", OrgID: "o", Slug: "project", WikiRevision: store.NullableString("old")})
			},
			func() error { return q.PutProjectMember("p", "u", "owner") },
			func() error {
				return q.PutWikiFile(store.WikiFileRow{ProjectID: "p", Path: "wiki/index.md", Hash: string(model.HashContent(body)), Data: store.WikiFileData{Bytes: int64(len(body))}})
			},
			func() error { return q.PutInbox(inbox) },
			func() error {
				return q.InsertSession(store.SessionRow{ID: "s", Kind: "user", Status: "active", UserID: "u", Expires: 10000, Data: json.RawMessage(`{}`)})
			},
		} {
			if err := fn(); err != nil {
				return err
			}
		}
		j := store.JobRow{ID: "j", ProjectID: "p", Status: "running", Kind: "generate", Phase: store.NullableString("agent_running"), CleanupComplete: false, Data: store.JobData{UserID: "u", Run: &store.RunData{PreparedAt: 1000, Deadline: 3601000, BudgetCents: 100}}}
		switch strings.Split(stage, "/")[0] {
		case "input":
			j.Status = "queued"
			j.Phase = nil
			j.CleanupComplete = false
			j.Data.Run = nil
		case "view":
			j.Kind = "view"
			j.ParentID = store.NullableString("parent")
			j.Data.ViewTask = &store.ViewTaskData{ViewID: "v"}
			if e := q.PutView(store.ViewRow{ID: "v", ProjectID: "p", Slug: "map", Data: sqlTestViewData()}); e != nil {
				return e
			}
			if e := q.PutJob(store.JobRow{ID: "parent", ProjectID: "p", Status: "running", Kind: "generate", GenerationSnapshot: store.NullableString("old"), Data: store.JobData{UserID: "u", ViewGeneration: &store.GenerationData{Remaining: 100}, Views: []store.ViewResultData{{ViewID: "v"}}}}); e != nil {
				return e
			}
		case "cleanup":
			j.GenerationSnapshot = store.NullableString("old")
			j.Data.ViewGeneration = &store.GenerationData{Remaining: 100}
		case "delete":
			j.Status = "completed"
			j.CleanupComplete = true
		}
		return q.PutJob(j)
	})
}
func crashOperation(ctx context.Context, o *storageOperations, stage string) error {
	switch strings.Split(stage, "/")[0] {
	case "inbox":
		return o.putInbox(ctx, store.InboxRow{ID: "new", ProjectID: "p", SourceURL: "https://example.com/new", Data: store.InboxData{Submitter: "u"}}, []byte("new"))
	case "input":
		_, err := o.prepareInput(ctx, "j", false, func(context.Context, storageInput) ([]byte, error) { return []byte("archive"), nil })
		return err
	case "output":
		_, err := o.saveOutput(ctx, "j", []byte("output"), nil)
		return err
	case "wiki":
		_, err := o.publishWiki(ctx, "j", map[string]string{"wiki/a.md": "a", "wiki/index.md": "new wiki"})
		return err
	case "view":
		return o.publishView(ctx, "j", &viewBundle{files: map[string][]byte{"index.html": []byte("<h1>View</h1>")}})
	case "cleanup":
		cost := int64(40)
		if stage == "cleanup/resume" {
			cost = 999
		}
		_, err := o.cleanup(ctx, "j", runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: &cost}}, func(context.Context, runner.Attempt) error {
			if stage == "cleanup/usage" {
				os.Exit(91)
			}
			return nil
		})
		return err
	case "delete":
		return o.deleteProject(ctx, "p", nil)
	}
	return errors.New("unknown crash operation")
}
func TestStorageCrashChild(t *testing.T) {
	root := os.Getenv("KIOKU_STORAGE_CRASH_ROOT")
	if root == "" {
		return
	}
	stage := os.Getenv("KIOKU_STORAGE_CRASH_STAGE")
	db, o := openCrashStorage(t, root)
	o.db = crashCatalog{catalog: db, stage: stage}
	o.objects = crashObjects{Store: o.objects, stage: stage}
	if err := crashOperation(t.Context(), o, stage); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash point was not reached")
}
func assertStoredReferences(t *testing.T, o *storageOperations) map[blob.Key]string {
	t.Helper()
	keys := map[blob.Key]string{}
	if err := o.db.Read(t.Context(), func(q *store.Queries) error {
		wiki, err := q.ProjectWiki("p")
		if err != nil {
			return err
		}
		for _, f := range wiki {
			keys[model.WikiContentKey("p", model.ContentHash(f.Hash))] = f.Hash
		}
		views, err := q.ProjectViews("p")
		if err != nil {
			return err
		}
		for _, v := range views {
			if v.BundleHash != nil {
				keys[model.ViewContentKey("p", v.ID, model.ContentHash(*v.BundleHash))] = *v.BundleHash
			}
		}
		items, err := q.ProjectInbox("p")
		if err != nil {
			return err
		}
		for _, item := range items {
			if !item.Remove {
				keys[sqlInboxKey(item)] = ""
			}
		}
		jobs, err := q.ProjectJobs("p")
		if err != nil {
			return err
		}
		for _, j := range jobs {
			// Job input and output are read only while the job runs and has
			// not started its View phase; cleanup deletes them afterwards.
			if j.Status == "running" && j.GenerationSnapshot == nil {
				keys[sqlJobKey(j, "input.tar.gz")] = ""
				if j.OutputHash != nil {
					keys[sqlJobKey(j, "output-"+*j.OutputHash+".tar.gz")] = *j.OutputHash
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for k, hash := range keys {
		b, err := o.objects.Get(t.Context(), k)
		if err != nil || hash != "" && hash != string(model.HashContent(b)) {
			t.Fatalf("dangling or corrupt reference %s: %v", k, err)
		}
	}
	return keys
}
func TestEveryBlobCommitBoundarySurvivesRestart(t *testing.T) {
	t.Parallel()
	stages := []string{"inbox/put", "inbox/sql-before", "inbox/sql-after", "input/put", "input/sql-before", "input/sql-after", "output/put", "output/sql-before", "output/sql-after", "wiki/put", "wiki/final-before", "wiki/final-after", "view/put", "view/sql-before", "view/sql-after", "cleanup/usage", "cleanup/delete", "cleanup/final-before", "cleanup/final-after", "delete/references-before", "delete/references-after", "delete/delete", "delete/final-before", "delete/final-after"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			seedCrashStorage(t, root, stage)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStorageCrashChild$")
			cmd.Env = append(os.Environ(), "KIOKU_STORAGE_CRASH_ROOT="+root, "KIOKU_STORAGE_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 91 {
				t.Fatalf("child did not crash at %s: %v\n%s", stage, err, out)
			}
			db, o := openCrashStorage(t, root)
			defer db.Close()
			assertStoredReferences(t, o)
			// A second process lock cannot be acquired while this server is open.
			if other, _, e := store.OpenStorage(t.Context(), filepath.Join(root, "catalog"), o.objects, func(bool) (*auth.Cipher, error) { t.Error("key read before lock"); return nil, nil }); e == nil {
				other.Close()
				t.Fatal("second writer started")
			}
			part := strings.Split(stage, "/")[0]
			var j store.JobRow
			if err := db.Read(t.Context(), func(q *store.Queries) error {
				var e error
				j, e = q.Job("j")
				if part == "delete" && errors.Is(e, sql.ErrNoRows) {
					return nil
				}
				return e
			}); err != nil {
				t.Fatal(err)
			}
			assertCrashCheckpoint(t, db, stage, j)
			resume := true
			switch part {
			case "input":
				if j.Data.Run != nil {
					if j.Status != "running" {
						t.Fatal(j)
					}
					resume = false
				}
			case "wiki", "view":
				resume = j.Status != "completed"
			case "delete":
				resume = j.ID != ""
			}
			if resume {
				if err := crashOperation(t.Context(), o, part+"/resume"); err != nil {
					t.Fatal(err)
				}
			}
			assertStoredReferences(t, o)
			if part == "cleanup" {
				if err := crashOperation(t.Context(), o, "cleanup/resume"); err != nil {
					t.Fatal(err)
				}
				got := readSQLJob(t, o, "j")
				if !got.CleanupComplete || got.Data.ViewGeneration.Remaining != 60 {
					t.Fatal(got)
				}
				if err := db.Read(t.Context(), func(q *store.Queries) error {
					rows, err := q.Usage("j")
					if rows.Data.CostCents == nil || *rows.Data.CostCents != 40 {
						t.Fatal(rows)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if part == "delete" {
				if err := db.Read(t.Context(), func(q *store.Queries) error { _, err := q.Session("s"); return err }); err != nil {
					t.Fatal("project deletion removed session", err)
				}
			}
		})
	}
}

func assertCrashCheckpoint(t *testing.T, db *store.Database, stage string, j store.JobRow) {
	t.Helper()
	if err := db.Read(t.Context(), func(q *store.Queries) error {
		part := strings.Split(stage, "/")[0]
		after := strings.HasSuffix(stage, "-after")
		switch part {
		case "inbox":
			_, err := q.Inbox("new")
			if (err == nil) != after {
				t.Fatal("inbox commit boundary", err)
			}
		case "input":
			i, err := q.Inbox("i")
			if err != nil {
				return err
			}
			if (j.Data.Run != nil) != after || (i.RunID != nil) != after {
				t.Fatal("Run and assignment were not atomic", j, i)
			}
		case "output":
			if (j.OutputHash != nil) != after {
				t.Fatal("output reference committed at wrong boundary", j)
			}
		case "wiki":
			p, err := q.Project("p")
			if err != nil {
				return err
			}
			complete := after
			if (j.Status == "completed") != complete {
				t.Fatal("Wiki completion boundary", p, j)
			}
			_, err = q.WikiFile("p", "wiki/a.md")
			if (err == nil) != complete {
				t.Fatal("Wiki rows were not atomic", err)
			}
			if (store.ValueString(p.WikiRevision) != "old") != complete {
				t.Fatal("Wiki revision was not atomic", p)
			}
		case "view":
			v, err := q.View("p", "v")
			if err != nil {
				return err
			}
			if (v.BundleHash != nil) != after || (j.Status == "completed") != after {
				t.Fatal("View and child completion were not atomic", v, j)
			}
		case "cleanup":
			complete := stage == "cleanup/final-after"
			remaining := int64(100)
			if complete {
				remaining = 60
			}
			if j.CleanupComplete != complete || j.Data.ViewGeneration.Remaining != remaining {
				t.Fatal("cleanup checkpoint", j)
			}
		case "delete":
			p, err := q.Project("p")
			if stage == "delete/final-after" {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("project rows remain", err)
				}
				return nil
			}
			if err != nil {
				return err
			}
			if !p.Deleting {
				t.Fatal("deletion not persisted")
			}
			rows, err := q.ProjectWiki("p")
			if err != nil {
				return err
			}
			if (len(rows) > 0) != (stage == "delete/references-before") {
				t.Fatal("delete reference boundary", rows)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
