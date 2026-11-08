package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

// testAgent mints an agent credential the way the code exchange does.
func testAgent(t *testing.T, f *fixture, user, pid, role string) (string, string) {
	t.Helper()
	var raw string
	var token model.AgentToken
	if err := f.db.Update(func(st *model.State) error {
		var err error
		raw, token, err = mintAgent(st, st.Sessions[auth.Hash(user)].UserID, pid, "Cloud test", role, time.Hour)
		return err
	}); err != nil {
		t.Fatal("mint agent", err)
	}
	return token.ID, raw
}

func TestCreateAgentRouteRemoved(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	if c := f.request("POST", "/api/projects/"+p.ID+"/agents", human, map[string]any{"name": "Cloud test", "role": "read"}, nil); c != 405 {
		t.Fatal("create agent route still answers", c)
	}
	_ = f.db.View(func(st model.State) error {
		if len(st.AgentTokens) != 0 {
			t.Error("agent credential minted", len(st.AgentTokens))
		}
		return nil
	})
}
func TestAgentCannotCrossProjectsOrAdministerAccount(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	a := securityProject(t, f, human)
	var b model.Project
	if c := f.request("POST", "/api/projects", human, map[string]string{"name": "Other project"}, &b); c != 201 {
		t.Fatal(c)
	}
	var job model.Job
	if c := f.request("POST", "/api/projects/"+a.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "evidence.md", Content: testSourceContent(testSourceURL("evidence.md", "Project A evidence"), "Project A evidence")}}, &job); c != 202 {
		t.Fatal(c)
	}
	runGeneration(t, f, human, a.ID)
	id, token := testAgent(t, f, human, a.ID, "read")
	var projects []model.Project
	if c := f.request("GET", "/api/projects", token, nil, &projects); c != 200 || len(projects) != 1 || projects[0].Namespace != a.Namespace {
		t.Fatal("project enumeration", c, projects)
	}
	if c := f.request("GET", "/api/projects/"+a.ID+"/snapshot", token, nil, nil); c != 200 {
		t.Fatal("selected project unavailable", c)
	}
	for _, suffix := range []string{"/snapshot", "/usage", "/jobs/unknown"} {
		if c := f.request("GET", "/api/projects/"+b.ID+suffix, token, nil, nil); c != 404 {
			t.Fatal("cross-project access", suffix, c)
		}
	}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/projects"}, {"GET", "/api/orgs"}, {"GET", "/api/account"}, {"POST", "/api/account/revoke-all"},
		{"POST", "/api/projects/" + a.ID + "/invites"}, {"DELETE", "/api/projects/" + a.ID},
		{"POST", "/api/projects/" + a.ID + "/put"},
	} {
		if c := f.request(route.method, route.path, token, nil, nil); c != 403 {
			t.Fatal("agent escaped scope", route, c)
		}
	}
	var listed []map[string]any
	if c := f.request("GET", "/api/projects/"+a.ID+"/agents", human, nil, &listed); c != 200 {
		t.Fatal(c)
	}
	encoded, _ := json.Marshal(listed)
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "token_hash") {
		t.Fatal("agent list exposed credential")
	}
	_ = f.db.View(func(st model.State) error {
		if st.AgentTokens[id].TokenHash == token || st.AgentTokens[id].TokenHash == "" {
			t.Error("plaintext token persistence")
		}
		return nil
	})
	if c := f.request("POST", "/api/logout", token, nil, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("GET", "/api/projects", token, nil, nil); c != 401 {
		t.Fatal("logged-out agent still works", c)
	}
	if c := f.request("GET", "/api/me", human, nil, nil); c != 200 {
		t.Fatal("agent logout affected human", c)
	}
}
func TestWriteAgentAcceptsURLsAndRevocationStopsQueuedWork(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	id, token := testAgent(t, f, human, p.ID, "write")
	for _, u := range []string{"https://docs.google.com/document/d/private/edit", "https://github.com/example/private/blob/main/secret.md", "https://example.com"} {
		if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent(u, "Source body")}}, nil); c != 202 {
			t.Fatal("provider/URL delegation", c)
		}
	}
	var job model.Job
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "note.md", Content: testSourceContent(testSourceURL("note.md", "Authorized file"), "Authorized file")}}, &job); c != 202 {
		t.Fatal(c, job)
	}
	job = f.generate(p.ID, token, false)
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/agents/"+id, human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	configureAutoRunner(t, f)
	f.db.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		j.Status = "running"
		st.Jobs[j.ID] = j
		job = j
		return nil
	})
	if err := f.s.startAgentJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if j := storedJob(t, f, job.ID); j.Status != "failed" {
		t.Fatal("revoked agent job published", j)
	}
	if c := f.request("GET", "/api/projects", token, nil, nil); c != 401 {
		t.Fatal("revoked token accepted", c)
	}
}
func TestMembershipRemovalPermanentlyRevokesDelegatedTokens(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	memberToken := f.login("member@example.com")
	p := securityProject(t, f, human)
	var user struct {
		ID string `json:"id"`
	}
	f.request("GET", "/api/me", memberToken, nil, &user)
	_ = f.db.Update(func(st *model.State) error {
		v := st.Projects[p.ID]
		v.Members[user.ID] = "member"
		st.Projects[p.ID] = v
		return nil
	})
	id, token := testAgent(t, f, memberToken, p.ID, "read")
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/members/"+user.ID, human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	_ = f.db.Update(func(st *model.State) error {
		v := st.Projects[p.ID]
		v.Members[user.ID] = "member"
		st.Projects[p.ID] = v
		return nil
	})
	if c := f.request("GET", "/api/projects", token, nil, nil); c != 401 {
		t.Fatal("rejoining resurrected delegated token", c)
	}
	_ = f.db.Update(func(st *model.State) error {
		v := st.AgentTokens[id]
		v.RevokedAt = time.Time{}
		v.Expires = time.Now().Add(-time.Minute)
		st.AgentTokens[id] = v
		return nil
	})
	if c := f.request("GET", "/api/projects", token, nil, nil); c != 401 {
		t.Fatal("expired token accepted", c)
	}
}

func TestRevokingAgentDuringRequestCannotBroadenItsIdentity(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	var other model.Project
	f.request("POST", "/api/projects", human, map[string]string{"name": "Other private project"}, &other)
	id, raw := testAgent(t, f, human, p.ID, "read")
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Pattern = "GET /api/projects"
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Kioku-Client-Version", "dev")
	recorder := httptest.NewRecorder()
	wrapped := f.s.wrap(func(w http.ResponseWriter, r *http.Request, uid string) error {
		if err := f.db.Update(func(st *model.State) error {
			token := st.AgentTokens[id]
			token.RevokedAt = time.Now().UTC()
			st.AgentTokens[id] = token
			return nil
		}); err != nil {
			return err
		}
		return f.s.projects(w, r, uid)
	}, true)
	wrapped(recorder, req)
	var projects []model.Project
	if err := json.Unmarshal(recorder.Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || len(projects) != 1 || projects[0].Namespace != p.Namespace {
		t.Fatal("revocation broadened credential into human identity", recorder.Code, projects)
	}
}
