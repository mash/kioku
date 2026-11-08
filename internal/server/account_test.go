package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func accountID(t *testing.T, f *fixture, token string) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if c := f.request("GET", "/api/me", token, nil, &out); c != 200 {
		t.Fatal(c)
	}
	return out.ID
}
func TestAccountSessionRevocationAndCredentialRedaction(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	second := f.login("owner@example.com")
	other := f.login("other@example.com")
	p := securityProject(t, f, human)
	_, agent := testAgent(t, f, human, p.ID, "read")
	var status map[string]any
	if c := f.request("GET", "/api/account", human, nil, &status); c != 200 {
		t.Fatal(c)
	}
	text, _ := json.Marshal(status)
	if strings.Contains(string(text), "identity_credential") || strings.Contains(string(text), human) {
		t.Fatal("account view exposed credentials")
	}
	if c := f.request("DELETE", "/api/account/sessions/"+auth.Hash(other), human, nil, nil); c != 404 {
		t.Fatal("revoked another user's session", c)
	}
	if c := f.request("DELETE", "/api/account/sessions/"+auth.Hash(second), human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("GET", "/api/me", second, nil, nil); c != 401 {
		t.Fatal("revoked session accepted", c)
	}
	if c := f.request("POST", "/api/account/revoke-all", human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	for _, token := range []string{human, second, agent} {
		if c := f.request("GET", "/api/me", token, nil, nil); c != 401 {
			t.Fatal("revoke-all left credential active", c)
		}
	}
	if c := f.request("GET", "/api/me", other, nil, nil); c != 200 {
		t.Fatal("other account affected", c)
	}
}
func TestUnlinkIdentityPreservesOtherLoginAndRevokesSessions(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	uid := accountID(t, f, human)
	if c := f.request("DELETE", "/api/account/identities/google", human, nil, nil); c != 409 {
		t.Fatal("last login identity removed", c)
	}
	p := securityProject(t, f, human)
	_, agent := testAgent(t, f, human, p.ID, "read")
	extra := auth.Token()
	_ = f.db.Update(func(st *model.State) error {
		u := st.Users[uid]
		u.Identities["github"] = "github-user"
		st.Users[uid] = u
		st.Sessions[auth.Hash(extra)] = model.Session{UserID: uid, IdentityProvider: "github", IdentityID: "github-user", Expires: time.Now().Add(time.Hour)}
		return nil
	})
	if c := f.request("DELETE", "/api/account/identities/google", human, nil, nil); c != 409 {
		t.Fatal("current login provider removed", c)
	}
	if c := f.request("DELETE", "/api/account/identities/github", human, nil, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("GET", "/api/me", extra, nil, nil); c != 401 {
		t.Fatal("unlinked identity's session survived", c)
	}
	if c := f.request("GET", "/api/me", agent, nil, nil); c != 401 {
		t.Fatal("delegated token survived identity unlink", c)
	}
	if c := f.request("GET", "/api/me", human, nil, nil); c != 200 {
		t.Fatal("remaining login lost", c)
	}
}
func TestOwnershipHandoffAndAccountDeletionPreserveTeamData(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	other := f.login("member@example.com")
	uid, target := accountID(t, f, human), accountID(t, f, other)
	p := securityProject(t, f, human)
	if c := f.request("DELETE", "/api/account", human, map[string]string{"confirm": "yes"}, nil); c != 400 {
		t.Fatal("missing confirmation accepted", c)
	}
	if c := f.request("DELETE", "/api/account", human, map[string]string{"confirm": "delete-account"}, nil); c != 409 {
		t.Fatal("owned project orphaned", c)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/owner", human, map[string]string{"target": target}, nil); c != 409 {
		t.Fatal("nonmember made owner", c)
	}
	_ = f.db.Update(func(st *model.State) error {
		v := st.Projects[p.ID]
		v.Members[target] = "member"
		st.Projects[v.ID] = v
		return nil
	})
	var job model.Job
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "facts.md", Content: testSourceContent(testSourceURL("facts.md", "Retained team evidence"), "Retained team evidence")}}, &job); c != 202 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/owner", human, map[string]string{"target": target}, nil); c != 200 {
		t.Fatal(c)
	}
	var slug string
	_ = f.db.View(func(st model.State) error { slug = st.Organizations[p.OrgID].Slug; return nil })
	if c := f.request("POST", "/api/orgs/"+slug+"/owner", human, map[string]string{"target": target}, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/projects", human, map[string]string{"name": "Cannot reclaim personal namespace"}, nil); c != 409 {
		t.Fatal("former owner reclaimed transferred namespace", c)
	}
	if c := f.request("DELETE", "/api/account", human, map[string]string{"confirm": "delete-account"}, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("GET", "/api/me", human, nil, nil); c != 401 {
		t.Fatal("deleted account session active", c)
	}
	var projects []model.ProjectEntry
	if c := f.request("GET", "/api/projects", other, nil, &projects); c != 200 || len(projects) != 1 || projects[0].ID != p.ID || projects[0].InboxCount != 1 {
		t.Fatal("team evidence lost", c, projects)
	}
	_ = f.db.View(func(st model.State) error {
		if _, ok := st.Users[uid]; ok {
			t.Error("profile retained")
		}
		if st.Projects[p.ID].Members[target] != "owner" || st.Projects[p.ID].Members[uid] != "" || st.Organizations[p.OrgID].Members[target] != "owner" {
			t.Error("ownership or membership incorrect")
		}
		return nil
	})
}
func accountUID(st *model.State, raw string) string { return st.Sessions[auth.Hash(raw)].UserID }
