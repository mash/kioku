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
)

func jobTokenFixture(t *testing.T) (*fixture, string, model.Job, model.AgentToken, string) {
	t.Helper()
	f := setupWithWorker(t, false)
	human := f.login("job-owner@example.com")
	p := securityProject(t, f, human)
	var job model.Job
	var token model.AgentToken
	var raw string
	if err := f.db.Update(func(st *model.State) error {
		job = model.Job{ID: "job", ProjectID: p.ID, UserID: st.Sessions[auth.Hash(human)].UserID, Kind: "generate", Status: "running", Created: time.Now().UTC()}
		st.Jobs[job.ID] = job
		raw, token = issueJobToken(st, job, time.Now().Add(time.Hour))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, raw, job, token, human
}
func TestJobTokenOnlyPermitsJobRoutes(t *testing.T) {
	f, raw, job, token, human := jobTokenFixture(t)
	if token.TokenHash != auth.Hash(raw) || token.TokenHash == raw || token.Name != "job:"+job.ID {
		t.Fatal("bad token metadata")
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
		_, p := f.s.session(req)
		if err := f.s.authorizeAgent(req, p); err == nil {
			t.Fatal("accepted", role)
		}
	}
	for _, expired := range []bool{true, false} {
		_ = f.db.Update(func(st *model.State) error {
			x := st.AgentTokens[token.ID]
			if expired {
				x.Expires = time.Now().Add(-time.Second)
			} else {
				x.Expires = time.Now().Add(time.Hour)
				st.AgentTokens[token.ID] = x
				revokeJobToken(st, job.ID)
				return nil
			}
			st.AgentTokens[token.ID] = x
			return nil
		})
		req := httptest.NewRequest("GET", "/", nil)
		req.Pattern = agentInputRoute
		req.SetPathValue("project", job.ProjectID)
		req.SetPathValue("job", job.ID)
		req.Header.Set("Authorization", "Bearer "+raw)
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
		"DELETE /api/account/connections/{provider}",
		"DELETE /api/account/identities/{provider}",
		"POST /api/projects/{project}/owner",
		"POST /api/orgs/{org}/owner",
		"GET /api/projects/{project}/agents",
		"DELETE /api/projects/{project}/agents/{agent}",
		"POST /api/orgs",
		"GET /api/orgs",
		"GET /{org}/{slug}",
		"GET /api/projects/{project}/usage",
		"GET /api/projects/{project}/audit",
		"POST /api/projects/{project}/maintenance",
		"DELETE /api/projects/{project}/sources/{source}",
		"DELETE /api/projects/{project}",
		"POST /api/auth/loopback",
		"POST /api/auth/loopback/token",
		"GET /auth/{provider}/start",
		"GET /auth/{provider}/callback",
		"POST /api/logout",
		"GET /api/me",
		"POST /api/connect/{provider}",
		"GET /api/projects",
		"POST /api/projects",
		"POST /api/join",
		"GET /api/projects/{project}/members",
		"POST /api/projects/{project}/invites",
		"DELETE /api/projects/{project}/members/{member}",
		"POST /api/projects/{project}/put",
		"POST /api/projects/{project}/generate",
		"GET /api/projects/{project}/jobs/{job}",
		"POST /api/projects/{project}/jobs/{job}/retry",
		"GET /api/projects/{project}/sources",
		"GET /api/projects/{project}/sources/{source}/{format}",
		"GET /api/projects/{project}/snapshot",
		"GET /",
		"GET /projects/{project}",
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
	replace := strings.NewReplacer("{project}", job.ProjectID, "{job}", job.ID, "{org}", "org", "{slug}", "slug", "{session}", "session", "{provider}", "google", "{agent}", "agent", "{source}", "source", "{format}", "text", "{member}", "member", "{invite}", "invite")
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			parts := strings.SplitN(route, " ", 2)
			if c := f.request(parts[0], replace.Replace(parts[1]), raw, nil, nil); c != 403 {
				t.Fatalf("job credential escaped route: %s returned %d", route, c)
			}
		})
	}
}
