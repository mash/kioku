package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/store"
)

type beforeWriteCatalog struct {
	catalog
	before func()
}

func (d *beforeWriteCatalog) Write(ctx context.Context, fn func(*store.Queries) error) error {
	if before := d.before; before != nil {
		d.before = nil
		before()
	}
	return d.catalog.Write(ctx, fn)
}

func TestJoinRechecksCallerAtCommit(t *testing.T) {
	t.Parallel()
	for _, deleted := range []bool{false, true} {
		t.Run(strconv.FormatBool(deleted), func(t *testing.T) {
			f, owner, p := orgProject(t)
			caller := f.login("invitee@example.com")
			var invite struct{ URL string }
			if code := f.request("POST", "/api/projects/"+p.ID+"/invites", owner, map[string]string{}, &invite); code != 201 {
				t.Fatal(code)
			}
			var uid string
			f.s.Store = &beforeWriteCatalog{catalog: f.stateStore, before: func() {
				fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
					session, err := q.Session(auth.Hash(caller))
					if err != nil {
						return err
					}
					uid = session.UserID
					if deleted {
						return q.DeleteAccountRows(uid)
					}
					return q.DeleteSession(session.ID)
				}))
			}}
			if code := f.request("POST", "/api/join", caller, map[string]string{"token": invite.URL}, nil); code != 401 {
				t.Fatalf("revoked caller joined: %d", code)
			}
			fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
				if _, err := q.ProjectRole(p.ID, uid); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("revoked caller gained membership: %v", err)
				}
				u, err := url.Parse(invite.URL)
				if err != nil {
					return err
				}
				row, err := q.InviteByToken(auth.Hash(strings.TrimPrefix(u.Path, "/invite/")))
				if row.UsedBy != nil {
					t.Fatal("rejected join consumed the invite")
				}
				return err
			}))
		})
	}
}

func TestConnectorProjectSQLFailureIsNotNotFound(t *testing.T) {
	t.Parallel()
	f, human, p := orgProject(t)
	access := connectorAccessToken(t, f, human, p.ID)
	fixtureSQL(t, f, "ALTER TABLE projects RENAME TO unavailable_projects")
	for _, name := range []string{"wiki_read", "cli_login_code"} {
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
			"name": name, "arguments": map[string]string{"project": p.Namespace},
		}}
		req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(fixtureJSON(t, body)))
		req.Header.Set("Authorization", "Bearer "+access)
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, req)
		if w.Code != 500 {
			t.Fatalf("%s hid SQL failure: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestOAuthAuthorizationPropagatesSQLFailures(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"registration", "selection"} {
		t.Run(stage, func(t *testing.T) {
			f, human, p := orgProject(t)
			params := connectorAuthorizeParams(f, connectorClient(t, f), auth.Token())
			if stage == "registration" {
				fixtureSQL(t, f, "ALTER TABLE oauth_clients RENAME TO unavailable_clients")
				w := connectorForm(f, "GET", "/oauth/authorize", human, params)
				if w.Code != 500 {
					t.Fatalf("registration SQL failure became client error: %d %s", w.Code, w.Body.String())
				}
				return
			}
			action, fields, _ := consentControls(t, connectorForm(f, "GET", "/oauth/authorize", human, params))
			fields.Set("namespace", p.Namespace)
			fixtureSQL(t, f, "ALTER TABLE projects RENAME TO unavailable_projects")
			w := connectorForm(f, "POST", action, human, fields)
			if w.Code != 500 {
				t.Fatalf("selection SQL failure became client error: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestClaudeFileFailureRejectsPreviouslyReceivedOutput(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	sendOutput(t, f, ma, j, "Output before provider failure")
	// Recover the same persisted output as a File run after receipt, before
	// publication. The provider now reports an unsuccessful execution.
	fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
		row, err := q.Job(j.ID)
		if err != nil {
			return err
		}
		row.Data.Run.UsesProviderFiles = true
		return q.PutJob(row)
	}))
	f.runnerConfig.ManagedAgents = claudemanaged.NewAPI(ma.URL(), "synthetic", ma.HTTP.Client())
	f.setRunners()
	ma.SetStatus(j.Run.Ref.ID, "idle", "retries_exhausted", 40)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || j.Revision != "" || !j.Run.CleanupComplete || j.Run.CostCents == nil || *j.Run.CostCents != 40 {
		t.Fatalf("File failure lost to received output: %+v run=%+v", j, j.Run)
	}
}

func TestScheduledProjectFailureDoesNotBlockOtherProjects(t *testing.T) {
	t.Parallel()
	f, human, bad := orgProject(t)
	good := consentProject(t, f, human, "other")
	for _, p := range []fixtureProject{bad, good} {
		putFile(t, f, human, p.ID, "source.md", "source")
		fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
			row, err := q.Project(p.ID)
			if err != nil {
				return err
			}
			at := int64(1)
			row.NextGenerateAt = &at
			return q.PutProject(row)
		}))
	}
	restore := fixtureSQLFault(t, f, "jobs", "INSERT", "NEW.project_id='"+bad.ID+"'")
	if err := f.s.scheduleGenerations(time.Now()); err == nil {
		t.Fatal("scheduling failure ignored")
	}
	fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		jobs, err := q.ProjectJobs(good.ID)
		if err != nil {
			return err
		}
		if len(jobs) != 1 || jobs[0].Status != "queued" || !jobs[0].Data.Automatic {
			t.Fatal("unrelated project's automatic generation was blocked", jobs)
		}
		p, err := q.Project(bad.ID)
		if p.NextGenerateAt == nil {
			t.Fatal("failed project lost its retry deadline")
		}
		return err
	}))
	restore()
	fixtureCheck(t, f.s.scheduleGenerations(time.Now()))
	fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		for _, p := range []fixtureProject{bad, good} {
			jobs, err := q.ProjectJobs(p.ID)
			if err != nil {
				return err
			}
			if len(jobs) != 1 {
				t.Fatalf("retry lost or duplicated generation: %s %+v", p.ID, jobs)
			}
		}
		return nil
	}))
}
