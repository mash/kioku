package server

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func testGeneralID(t *testing.T, f *fixture, uid string) string {
	t.Helper()
	var id string
	if err := f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		u, err := q.User(uid)
		id = store.ValueString(u.GeneralProjectID)
		return err
	}); err != nil || id == "" {
		t.Fatal("missing General reference", err)
	}
	return id
}

func TestGeneralListingAndCoOwnerPermissions(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	other := f.login("other@example.com")
	uid := accountID(t, f, human)
	otherID := accountID(t, f, other)
	const namespace = "owner/general"
	createTestOrg(t, f, human, "team")
	regular := consentProject(t, f, human, "team/general")
	shared := consentProject(t, f, human, "owner/shared")
	joinConsentProject(t, f, shared.ID, human, other)
	if status := f.request("POST", "/api/orgs/owner/owners", human, map[string]string{"target": otherID}, nil); status != 200 {
		t.Fatal("could not add co-owner", status)
	}
	if status := f.request("POST", "/api/projects", other, map[string]string{"name": "owner/co-owner-project"}, nil); status != 201 {
		t.Fatal("co-owner could not create a normal project", status)
	}
	if status := f.request("POST", "/api/orgs/owner/owner", other, map[string]string{"target": uid}, nil); status != 409 {
		t.Fatal("allowed transfer of an organization containing General", status)
	}
	var projects []map[string]any
	if status := f.request("GET", "/api/projects", human, nil, &projects); status != 200 || len(projects) != 3 {
		t.Fatal(status, projects)
	}
	for _, project := range projects {
		_, flagged := project["general"]
		_, hasID := project["id"]
		_, hasProjectID := project["project_id"]
		if hasID || hasProjectID || flagged != (project["namespace"] == namespace) || flagged && project["general"] != true {
			t.Fatal("unexpected project list entry", project)
		}
	}
	base := "/api/projects/" + namespace
	if status := f.request("GET", base+"/snapshot?manifest=1", other, nil, nil); status != 404 {
		t.Fatal("General was accessible to an organization co-owner", status)
	}
	if status := f.request("GET", "/api/projects", other, nil, &projects); status != 200 || slices.ContainsFunc(projects, func(p map[string]any) bool { return p["namespace"] == namespace }) {
		t.Fatal("General was listed to an organization co-owner", status, projects)
	}
	if status := f.request("POST", base+"/invites", human, map[string]string{}, nil); status != 409 {
		t.Fatal("allowed a General membership invite", status)
	}
	if status := f.request("POST", base+"/owner", human, map[string]string{"target": otherID}, nil); status != 409 {
		t.Fatal("allowed a General project transfer", status)
	}
	if status := f.request("DELETE", base, human, nil, nil); status != 409 {
		t.Fatal("allowed a standalone General deletion", status)
	}
	var manifest model.SyncManifest
	if status := f.request("GET", base+"/snapshot?manifest=1", human, nil, &manifest); status != 200 || manifest.Files == nil || len(manifest.Files) != 0 {
		t.Fatal("new General could not be pulled", status, manifest)
	}

	var listing struct{ Projects []map[string]any }
	for _, tc := range []struct {
		id, namespace string
		general       bool
	}{{regular.ID, regular.Namespace, false}, {testGeneralID(t, f, uid), namespace, true}} {
		access := connectorAccessToken(t, f, human, tc.id)
		listing.Projects = nil
		msg := connectorTool(t, f, access, "projects_list", nil, &listing)
		if msg != "" || len(listing.Projects) != 1 {
			t.Fatal("connector returned the wrong projects", msg, listing)
		}
		entry := listing.Projects[0]
		_, flagged := entry["general"]
		wantFields := 3
		if tc.general {
			wantFields++
		}
		if entry["namespace"] != tc.namespace || len(entry) != wantFields || flagged != tc.general || flagged && entry["general"] != true {
			t.Fatal("connector returned the wrong granted project", entry)
		}
	}
	if status := f.request("DELETE", "/api/projects/"+regular.Namespace, human, nil, nil); status != 200 {
		t.Fatal("ordinary project with slug general could not be deleted", status)
	}
}

type pausedGeneralDeletion struct {
	blob.Store
	prefix          string
	reached, resume chan struct{}
}

func (b pausedGeneralDeletion) List(ctx context.Context, prefix string) ([]blob.Key, error) {
	if prefix == b.prefix {
		close(b.reached)
		select {
		case <-b.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.Store.List(ctx, prefix)
}

func TestConcurrentCoOwnerDeletionsKeepAnOwnerForNonemptyOrganization(t *testing.T) {
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
	if status := f.request("POST", "/api/projects/team/docs/owner", alice, map[string]string{"target": carolID}, nil); status != 200 {
		t.Fatal("transfer project ownership", status)
	}
	gate := pausedGeneralDeletion{
		Store: f.s.Blobs, prefix: "projects/" + testGeneralID(t, f, aliceID) + "/",
		reached: make(chan struct{}), resume: make(chan struct{}),
	}
	f.s.storage.objects = gate
	confirm := map[string]string{"confirm": "delete-account"}
	result := make(chan int, 1)
	go func() { result <- f.request("DELETE", "/api/account", alice, confirm, nil) }()
	defer func() {
		close(gate.resume)
		if status := <-result; status != http.StatusOK {
			t.Error("first co-owner could not delete account", status)
		}
		if status := f.request("GET", "/api/projects/team/docs/snapshot?manifest=1", carol, nil, nil); status != http.StatusOK {
			t.Error("organization project did not survive", status)
		}
	}()
	select {
	case <-gate.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("account deletion did not reach content cleanup")
	}
	if status := f.request("DELETE", "/api/account", bob, confirm, nil); status != http.StatusConflict {
		t.Fatal("last remaining owner could delete account", status)
	}
}

func TestAccountDeletionBlocksConcurrentCreation(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	other := f.login("other@example.com")
	uid := accountID(t, f, human)
	otherID := accountID(t, f, other)
	shared := consentProject(t, f, human, "owner/shared")
	joinConsentProject(t, f, shared.ID, human, other)
	if status := f.request("POST", "/api/orgs/owner/owners", human, map[string]string{"target": otherID}, nil); status != 200 {
		t.Fatal(status)
	}
	if status := f.request("DELETE", "/api/projects/owner/shared", human, nil, nil); status != 200 {
		t.Fatal(status)
	}
	generalID := testGeneralID(t, f, uid)
	putFile(t, f, human, generalID, "private.md", "Private evidence")
	prefix := "projects/" + generalID + "/"
	objects := f.s.Blobs
	gate := pausedGeneralDeletion{Store: objects, prefix: prefix, reached: make(chan struct{}), resume: make(chan struct{})}
	f.s.storage.objects = gate
	result := make(chan int, 1)
	go func() {
		result <- f.request("DELETE", "/api/account", human, map[string]string{"confirm": "delete-account"}, nil)
	}()
	defer func() {
		close(gate.resume)
		if status := <-result; status != http.StatusOK {
			t.Error("account deletion failed after starting content cleanup", status)
		}
		if keys, err := objects.List(t.Context(), prefix); err != nil || len(keys) != 0 {
			t.Error("General content survived account deletion", keys, err)
		}
	}()
	select {
	case <-gate.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("account deletion did not reach content cleanup")
	}
	for _, tc := range []struct {
		token, path string
		body        map[string]string
	}{
		{human, "/api/projects", map[string]string{"name": "Race"}},
		{human, "/api/orgs", map[string]string{"slug": "race"}},
		{other, "/api/projects", map[string]string{"name": "owner/race"}},
	} {
		if status := f.request("POST", tc.path, tc.token, tc.body, nil); status != http.StatusConflict {
			t.Errorf("creation during deletion: %s %v returned %d", tc.path, tc.body, status)
		}
	}
}
