package server

import (
	"context"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

func createTestOrg(t *testing.T, f *fixture, token, slug string) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if c := f.request("POST", "/api/orgs", token, map[string]string{"slug": slug}, &out); c != 201 {
		t.Fatal(c)
	}
	return out.ID
}

// orgProject creates the organization "acme" and the project "acme/platform".
func orgProject(t *testing.T) (*fixture, string, model.Project) {
	t.Helper()
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	createTestOrg(t, f, owner, "acme")
	var p model.Project
	if c := f.request("POST", "/api/projects", owner, map[string]string{"name": "acme/platform"}, &p); c != 201 {
		t.Fatal(c)
	}
	return f, owner, p
}

func TestNamespacesAreUniqueAndOrganizationsDoNotGrantProjectReadAccess(t *testing.T) {
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	other := f.login("other@example.com")
	createTestOrg(t, f, owner, "acme")
	createTestOrg(t, f, other, "other")
	var p model.Project
	if c := f.request("POST", "/api/projects", owner, map[string]string{"name": "acme/platform"}, &p); c != 201 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/projects", owner, map[string]string{"name": "acme/platform"}, nil); c != 409 {
		t.Fatal("duplicate namespace", c)
	}
	if c := f.request("POST", "/api/projects", other, map[string]string{"name": "acme/stolen"}, nil); c != 404 {
		t.Fatal("namespace theft", c)
	}
	if c := f.request("POST", "/api/projects", other, map[string]string{"name": "other/platform"}, nil); c != 201 {
		t.Fatal("same slug in different organization", c)
	}
	for _, slug := range []string{"api", "auth", "billing", "UPPER", "../bad"} {
		if c := f.request("POST", "/api/orgs", owner, map[string]string{"slug": slug}, nil); c != 400 {
			t.Fatal("invalid organization", slug, c)
		}
	}
	if p.Namespace != "acme/platform" {
		t.Fatal(p.Namespace)
	}
}
func TestAgentFingerprintIncludesSources(t *testing.T) {
	inputs := []wiki.Input{{Source: model.Source{ID: "source", ContentHash: "hash"}}}
	a := agentFingerprint(inputs)
	inputs[0].Source.ID = "other"
	b := agentFingerprint(inputs)
	inputs[0].Source.ContentHash = "changed"
	if a == b || b == agentFingerprint(inputs) {
		t.Fatal("fingerprint missed sources or content")
	}
}
func TestDeterministicFallbackPublishes(t *testing.T) {
	f, token, p := orgProject(t)
	putFile(t, f, token, p.ID, "x.md", "Evidence")
	var j model.Job
	f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, &j)
	if _, err := f.s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].Revision == "" {
			t.Error("fallback did not publish")
		}
		return nil
	})
}
func TestManagedBudgetCapsTheSession(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "Evidence")
	j := startManaged(t, f, token, p.ID, false)
	if j.Run.BudgetCents != f.s.Config.AgentMaxSessionCents || j.Run.Attempt.ID == "" {
		t.Fatal("budget or attempt", j.Run.BudgetCents, j.Run.Attempt.ID)
	}
	f, token, p, ma = managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "Evidence")
	f.s.Config.AgentMaxSessionCents = 49
	before := len(ma.Requests())
	j = managedJob(t, f, token, p.ID, false)
	if err := f.s.startAgentJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j = storedJob(t, f, j.ID)
	if j.Status != "failed" || !strings.Contains(j.Error, "below 50 cents") || len(ma.Requests()) != before {
		t.Fatal("small budget started a session", j.Status, j.Error)
	}
}
