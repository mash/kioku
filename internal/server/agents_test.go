package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

// testAgent mints an agent credential the way the code exchange does.
func testAgent(t *testing.T, f *fixture, user, pid, role string) (string, string) {
	t.Helper()
	id := auth.Token()[:20]
	raw := "kioku_agent." + id + "." + auth.Token()
	token := fixtureAgentToken{ID: id, ProjectID: pid, Name: "Cloud test", Role: role, TokenHash: auth.Hash(raw), Created: time.Now(), Expires: time.Now().Add(time.Hour)}
	if err := f.stateStore.Write(t.Context(), func(st *store.Queries) error {
		token.UserID = fixtureSessionsGet(t, st, auth.Hash(user)).UserID
		fixtureAgentTokensPut(t, st, id, token)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return token.ID, raw
}

func TestWriteAgentAcceptsURLsAndRevocationStopsQueuedWork(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	id, token := testAgent(t, f, human, p.ID, "write")
	for _, u := range []string{"https://docs.google.com/document/d/private/edit", "https://github.com/example/private/blob/main/secret.md", "https://example.com"} {
		if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent(u, "Source body")}}, nil); c != 202 {
			t.Fatal("provider/URL delegation", c)
		}
	}
	var job fixtureJob
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "note.md", Content: testSourceContent(testSourceURL("note.md", "Authorized file"), "Authorized file")}}, &job); c != 202 {
		t.Fatal(c, job)
	}
	job = f.generate(p.ID, token, false)
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/agents/"+id, human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	configureAutoRunner(t, f)
	f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		j := fixtureJobsGet(t, st, job.ID)
		j.Status = "queued"
		fixtureJobsPut(t, st, j.ID, j)
		job = j
		return nil
	})
	if err := f.s.startAgentJob(context.Background(), fixtureRow(job)); err != nil {
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
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	memberToken := f.login("member@example.com")
	p := securityProject(t, f, human)
	var user struct {
		ID string `json:"id"`
	}
	f.request("GET", "/api/me", memberToken, nil, &user)
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		v := fixtureProjectsGet(t, st, p.ID)
		v.Members[user.ID] = "member"
		fixtureProjectsPut(t, st, p.ID, v)
		return nil
	})
	_, token := testAgent(t, f, memberToken, p.ID, "read")
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/members/"+user.ID, human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		v := fixtureProjectsGet(t, st, p.ID)
		v.Members[user.ID] = "member"
		fixtureProjectsPut(t, st, p.ID, v)
		return nil
	})
	if c := f.request("GET", "/api/projects", token, nil, nil); c != 401 {
		t.Fatal("rejoining resurrected delegated token", c)
	}
}

func TestRevokingAgentDuringRequestCannotBroadenItsIdentity(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	var other fixtureProject
	f.request("POST", "/api/projects", human, map[string]string{"name": "Other private project"}, &other)
	id, raw := testAgent(t, f, human, p.ID, "read")
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Pattern = "GET /api/projects"
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Kioku-Client-Version", "dev")
	recorder := httptest.NewRecorder()
	wrapped := f.s.wrap(func(w http.ResponseWriter, r *http.Request, uid string) error {
		if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
			return st.DeleteSession(fixtureAgentTokensGet(t, st, p.ID, id).TokenHash)
		}); err != nil {
			return err
		}
		return f.s.projects(w, r, uid)
	}, true)
	wrapped(recorder, req)
	var projects []fixtureProject
	if err := json.Unmarshal(recorder.Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || len(projects) != 1 || projects[0].Namespace != p.Namespace {
		t.Fatal("revocation broadened credential into human identity", recorder.Code, projects)
	}
}
