package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func jobTokenFixture(t *testing.T) (*fixture, string, fixtureJob, fixtureAgentToken, string) {
	t.Helper()
	f := setupWithWorker(t, false)
	human := f.login("job-owner@example.com")
	p := securityProject(t, f, human)
	var job fixtureJob
	token := fixtureAgentToken{}
	now := time.Now().UTC()
	if err := f.stateStore.Write(t.Context(), func(q *store.Queries) error {
		uid := fixtureSessionsGet(t, q, auth.Hash(human)).UserID
		job = fixtureJob{ID: "job", ProjectID: p.ID, UserID: uid, Kind: "generate", Status: "running", Created: now, Run: &fixtureAgentRun{Phase: model.PhasePreparing, PreparedAt: now, Deadline: now.Add(time.Hour), Attempt: fixtureAttempt{ID: "job"}}}
		fixtureJobsPut(t, q, job.ID, job)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.Cipher.SealJobCredential(auth.JobCredential{JobID: job.ID, Attempt: job.ID, Expires: now.Add(time.Hour + 10*time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}

	return f, raw, job, token, human
}
func TestJobTokenOnlyPermitsJobRoutes(t *testing.T) {
	f, raw, job, _, human := jobTokenFixture(t)
	// E24: job credentials are sealed values, never sessions rows.
	if n := sessionKindCount(t, f, "agent"); n != 0 {
		t.Fatal(n)
	}
	for _, pattern := range []string{agentInputRoute, agentOutputRoute} {
		for _, tc := range []struct {
			name, pid, jid, bearer string
			want                   int
		}{
			{"valid", job.ProjectID, job.ID, raw, 200}, {"other-project", "other", job.ID, raw, 403}, {"other-job", job.ProjectID, "other", raw, 403}, {"human", job.ProjectID, job.ID, human, 403}, {"anonymous", job.ProjectID, job.ID, "", 403},
		} {
			t.Run(pattern+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(strings.Split(pattern, " ")[0], "/", nil)
				req.Pattern = pattern
				req.SetPathValue("project", tc.pid)
				req.SetPathValue("job", tc.jid)
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
				req.Header.Set("Kioku-Client-Version", "dev")
				w := httptest.NewRecorder()
				f.s.wrap(func(w http.ResponseWriter, r *http.Request, uid string) error { w.WriteHeader(200); return nil }, true)(w, req)
				if w.Code != tc.want {
					t.Fatal(w.Code, w.Body.String())
				}
			})
		}
	}
	for _, role := range []string{"read", "write"} {
		_, other := testAgent(t, f, human, job.ProjectID, role)
		req := httptest.NewRequest("GET", "/", nil)
		req.Pattern = agentInputRoute
		req.SetPathValue("project", job.ProjectID)
		req.SetPathValue("job", job.ID)
		req.Header.Set("Authorization", "Bearer "+other)
		req.Header.Set("Kioku-Client-Version", "dev")
		_, p, err := f.s.session(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.authorizeAgent(req, p); err == nil {
			t.Fatal("accepted", role)
		}
	}
	for _, expired := range []bool{true, false} {
		denied := raw
		if expired {
			var err error
			denied, err = f.s.Cipher.SealJobCredential(auth.JobCredential{JobID: job.ID, Attempt: job.ID, Expires: time.Now().Add(-time.Second).UnixMilli()})
			if err != nil {
				t.Fatal(err)
			}
		} else {
			fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
				j, e := q.Job(job.ID)
				if e != nil {
					return e
				}
				j.Data.Run.AbortReason = store.NullableString("revoked")
				return q.PutJob(j)
			}))
		}

		req := httptest.NewRequest("GET", "/", nil)
		req.Pattern = agentInputRoute
		req.SetPathValue("project", job.ProjectID)
		req.SetPathValue("job", job.ID)
		req.Header.Set("Authorization", "Bearer "+denied)
		req.Header.Set("Kioku-Client-Version", "dev")
		w := httptest.NewRecorder()
		f.s.wrap(func(http.ResponseWriter, *http.Request, string) error {
			t.Error("invalid token reached handler")
			return nil
		}, true)(w, req)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}

// This table is deliberately explicit: adding a route must update the audit.
func TestJobTokenRouteAudit(t *testing.T) {
	f, raw, job, _, _ := jobTokenFixture(t)
	routes := []string{
		"POST /api/agent/login",
		"GET /.well-known/oauth-protected-resource",
		"GET /.well-known/oauth-protected-resource/mcp",
		"GET /.well-known/oauth-authorization-server",
		"POST /oauth/register",
		"GET /oauth/authorize",
		"POST /oauth/authorize",
		"POST /oauth/token",
		"GET /api/account",
		"DELETE /api/account",
		"DELETE /api/account/sessions/{session}",
		"POST /api/account/revoke-all",
		"DELETE /api/account/identities/{provider}",
		"POST /api/projects/{org}/{slug}/owner",
		"POST /api/orgs/{org}/owner",
		"GET /api/projects/{org}/{slug}/agents",
		"DELETE /api/projects/{org}/{slug}/agents/{agent}",
		"POST /api/orgs",
		"GET /api/orgs",
		"GET /{org}/{slug}",
		"GET /{org}/{slug}/views/{view}",
		"GET /api/projects/{org}/{slug}/usage",
		"GET /api/projects/{org}/{slug}/audit",
		"POST /api/projects/{org}/{slug}/maintenance",
		"DELETE /api/projects/{org}/{slug}",
		"POST /api/auth/loopback",
		"POST /api/auth/loopback/token",
		"GET /auth/{provider}/start",
		"GET /auth/{provider}/callback",
		"POST /api/logout",
		"GET /api/me",
		"GET /api/projects",
		"POST /api/projects",
		"POST /api/join",
		"GET /api/projects/{org}/{slug}/members",
		"POST /api/projects/{org}/{slug}/invites",
		"DELETE /api/projects/{org}/{slug}/members/{member}",
		"POST /api/projects/{org}/{slug}/put",
		"POST /api/projects/{org}/{slug}/generate",
		"POST /api/projects/{org}/{slug}/views",
		"GET /api/projects/{org}/{slug}/views/{view}",
		"PATCH /api/projects/{org}/{slug}/views/{view}",
		"DELETE /api/projects/{org}/{slug}/views/{view}",
		"GET /api/projects/{org}/{slug}/jobs/{job}",
		"GET /api/projects/{org}/{slug}/snapshot",
		"GET /",
		"GET /invite/{invite}",
	}
	exceptions := map[string]string{
		"GET /mcp":                 "Stateless MCP returns 405; not wrapped",
		"POST /mcp":                "Separate OAuth-only authentication; not wrapped",
		"POST /webhooks/anthropic": "Anthropic signature authentication; not wrapped",
		"GET /metrics":             "Dedicated metrics credential; not wrapped",
		"GET /healthz":             "Public liveness only; not wrapped",
	}
	allowed := []string{agentInputRoute, agentOutputRoute}
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(source), "s.mux.HandleFunc("); n != len(routes)+len(exceptions)+len(allowed) {
		t.Fatalf("route audit table stale: routes %d, audited %d", n, len(routes)+len(exceptions)+len(allowed))
	}
	replace := strings.NewReplacer("{project}", job.ProjectID, "{job}", job.ID, "{org}", "org", "{slug}", "slug", "{session}", "session", "{provider}", "google", "{agent}", "agent", "{source}", "source", "{view}", "test-view", "{format}", "text", "{member}", "member", "{invite}", "invite")
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			parts := strings.SplitN(route, " ", 2)
			if c := f.request(parts[0], replace.Replace(parts[1]), raw, nil, nil); c != 403 {
				t.Fatalf("job credential escaped route: %s returned %d", route, c)
			}
		})
	}
}
