package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
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
func TestUnlinkIdentityPreservesOtherLoginAndRevokesSessions(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	uid := accountID(t, f, human)
	if c := f.request("DELETE", "/api/account/identities/google", human, nil, nil); c != 409 {
		t.Fatal("last login identity removed", c)
	}
	p := securityProject(t, f, human)
	_, agent := testAgent(t, f, human, p.ID, "read")
	extra := auth.Token()
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		u := fixtureUsersGet(t, st, uid)
		u.Identities["github"] = "github-user"
		fixtureUsersPut(t, st, uid, u)
		fixtureSessionsPut(t, st, auth.Hash(extra), fixtureSession{UserID: uid, IdentityProvider: "github", IdentityID: "github-user", Expires: time.Now().Add(time.Hour)})
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
func TestCoOwnerCanDeleteAccountWithoutDeletingOthersPersonalOrganization(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	alice := f.login("alice@example.com")
	bob := f.login("bob@example.com")
	aliceID, bobID := accountID(t, f, alice), accountID(t, f, bob)
	shared := consentProject(t, f, alice, "alice/shared")
	joinConsentProject(t, f, shared.ID, alice, bob)
	if status := f.request("POST", "/api/orgs/alice/owners", alice, map[string]string{"target": bobID}, nil); status != 200 {
		t.Fatal("add co-owner", status)
	}
	if status := f.request("DELETE", "/api/account", bob, map[string]string{"confirm": "delete-account"}, nil); status != 200 {
		t.Fatal("delete co-owner account", status)
	}
	if status := f.request("GET", "/api/me", bob, nil, nil); status != 401 {
		t.Fatal("co-owner session survived", status)
	}
	if status := f.request("GET", "/api/projects/alice/general/snapshot?manifest=1", alice, nil, nil); status != 200 {
		t.Fatal("Alice lost access to her General", status)
	}
	if err := f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		org, err := q.OrganizationBySlug("alice")
		if err != nil {
			return err
		}
		owners, err := q.OrganizationOwners(org.ID)
		if err == nil && (len(owners) != 1 || !owners[aliceID]) {
			t.Errorf("expected only Alice as owner, got %v", owners)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizationTransferPreservesOtherCoOwners(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	alice := f.login("alice@example.com")
	bob := f.login("bob@example.com")
	carol := f.login("carol@example.com")
	aliceID, bobID, carolID := accountID(t, f, alice), accountID(t, f, bob), accountID(t, f, carol)
	createTestOrg(t, f, alice, "team")
	project := consentProject(t, f, alice, "team/docs")
	joinConsentProject(t, f, project.ID, alice, bob)
	joinConsentProject(t, f, project.ID, alice, carol)
	if status := f.request("POST", "/api/orgs/team/owners", alice, map[string]string{"target": bobID}, nil); status != 200 {
		t.Fatal("add co-owner", status)
	}
	if status := f.request("POST", "/api/orgs/team/owner", bob, map[string]string{"target": carolID}, nil); status != 200 {
		t.Fatal("transfer co-ownership", status)
	}
	if err := f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		owners, err := q.OrganizationOwners(project.OrgID)
		if err == nil && (len(owners) != 2 || !owners[aliceID] || !owners[carolID]) {
			t.Errorf("expected Alice and Carol as owners, got %v", owners)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipHandoffAndAccountDeletionPreserveTeamData(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	other := f.login("member@example.com")
	uid, target := accountID(t, f, human), accountID(t, f, other)
	createTestOrg(t, f, human, "team")
	p := consentProject(t, f, human, "team/evidence")
	generalID := testGeneralID(t, f, uid)
	putFile(t, f, human, generalID, "private.md", "Private evidence")
	prefix := "projects/" + generalID + "/"
	if keys, err := f.s.Blobs.List(t.Context(), prefix); err != nil || len(keys) == 0 {
		t.Fatal("could not seed General content", keys, err)
	}
	if c := f.request("DELETE", "/api/account", human, map[string]string{"confirm": "yes"}, nil); c != 400 {
		t.Fatal("missing confirmation accepted", c)
	}
	if c := f.request("DELETE", "/api/account", human, map[string]string{"confirm": "delete-account"}, nil); c != 409 {
		t.Fatal("owned project orphaned", c)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/owner", human, map[string]string{"target": target}, nil); c != 409 {
		t.Fatal("nonmember made owner", c)
	}
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		v := fixtureProjectsGet(t, st, p.ID)
		v.Members[target] = "member"
		fixtureProjectsPut(t, st, v.ID, v)
		return nil
	})
	var job fixtureJob
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "facts.md", Content: testSourceContent(testSourceURL("facts.md", "Retained team evidence"), "Retained team evidence")}}, &job); c != 202 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/owner", human, map[string]string{"target": target}, nil); c != 200 {
		t.Fatal(c)
	}
	var slug string
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error { slug = fixtureOrganizationsGet(t, st, p.OrgID).Slug; return nil })
	if c := f.request("POST", "/api/orgs/"+slug+"/owner", human, map[string]string{"target": target}, nil); c != 200 {
		t.Fatal(c)
	}
	if keys, err := f.s.Blobs.List(t.Context(), prefix); err != nil || len(keys) == 0 {
		t.Fatal("409 deleted General content", keys, err)
	}
	if status := f.request("GET", "/api/projects/owner/general/snapshot?manifest=1", human, nil, nil); status != 200 {
		t.Fatal("409 made General inaccessible", status)
	}
	if c := f.request("DELETE", "/api/account", human, map[string]string{"confirm": "delete-account"}, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("GET", "/api/me", human, nil, nil); c != 401 {
		t.Fatal("deleted account session active", c)
	}
	if keys, err := f.s.Blobs.List(t.Context(), prefix); err != nil || len(keys) != 0 {
		t.Fatal("General content retained", keys, err)
	}
	var projects []model.ProjectEntry
	if c := f.request("GET", "/api/projects", other, nil, &projects); c != 200 || len(projects) != 2 ||
		!slices.ContainsFunc(projects, func(entry model.ProjectEntry) bool { return entry.Namespace == p.Namespace && entry.InboxCount == 1 }) {
		t.Fatal("team evidence lost", c, projects)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if _, ok := fixtureUsersFind(t, st, uid); ok {
			t.Error("profile retained")
		}
		if fixtureProjectsGet(t, st, p.ID).Members[target] != "owner" || fixtureProjectsGet(t, st, p.ID).Members[uid] != "" || !fixtureOrganizationsGet(t, st, p.OrgID).Owners[target] {
			t.Error("ownership or membership incorrect")
		}
		return nil
	})
}
