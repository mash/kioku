package server

import (
	"net/http"
	"net/url"
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
	raw, err := f.s.Cipher.SealJobCredential(auth.JobCredential{JobID: job.ID, Expires: now.Add(time.Hour + 10*time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}

	return f, raw, job, token, human
}

// This table is deliberately explicit: adding a route must update the audit.
func jobRestrictedRoutes() []string {
	return []string{
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
		"GET /api/projects/{org}/{slug}/views",
		"POST /api/projects/{org}/{slug}/views",
		"GET /api/projects/{org}/{slug}/views/{view}",
		"POST /api/projects/{org}/{slug}/views/{view}",
		"PATCH /api/projects/{org}/{slug}/views/{view}",
		"DELETE /api/projects/{org}/{slug}/views/{view}",
		"GET /api/projects/{org}/{slug}/jobs/{job}",
		"GET /api/projects/{org}/{slug}/snapshot",
		"GET /",
		"GET /invite/{invite}",
	}
}

func TestJobTokenRouteAudit(t *testing.T) {
	t.Parallel()
	routes := jobRestrictedRoutes()
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
}

// Called by the real exec child with the job credential received on stdin.
func checkExecJobScope(t *testing.T, inputURL, token string) {
	t.Helper()
	endpoint, err := url.Parse(inputURL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(endpoint.Path, "/")
	if len(parts) != 9 {
		t.Fatal("unexpected job input path")
	}
	replace := strings.NewReplacer("{job}", parts[6], "{org}", parts[3], "{slug}", parts[4], "{session}", "session", "{provider}", "google", "{agent}", "agent", "{view}", "test-view", "{member}", "member", "{invite}", "invite")
	routes := jobRestrictedRoutes()
	for _, suffix := range []string{"input", "output"} {
		method := "GET"
		if suffix == "output" {
			method = "PUT"
		}
		routes = append(routes, method+" /api/projects/{org}/{slug}/jobs/other/agent/"+suffix, method+" /api/projects/{org}/other/jobs/{job}/agent/"+suffix)
	}
	for _, route := range routes {
		method, path, _ := strings.Cut(route, " ")
		endpoint.Path = replace.Replace(path)
		req, err := http.NewRequest(method, endpoint.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Kioku-Client-Version", "dev")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("job credential escaped route: %s returned %d", route, res.StatusCode)
		}
	}
}
