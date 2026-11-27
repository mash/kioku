package server

import (
	"net/http"
	"slices"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestGeneralProvisioningAndAccess(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	other := f.login("other@example.com")
	uid := accountID(t, f, human)
	namespace := "u-" + auth.Hash(uid)[:12] + "-general/general"
	regular := consentProject(t, f, human, "general")
	for range 2 {
		if err := f.s.EnsureGeneralWikis(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var projects []model.Project
	if status := f.request("GET", "/api/projects", human, nil, &projects); status != http.StatusOK || len(projects) != 2 {
		t.Fatal(status, projects)
	}
	i := slices.IndexFunc(projects, func(p model.Project) bool { return p.Namespace == namespace })
	if i == -1 || projects[i].Name != "General" || projects[1-i].Namespace != regular.Namespace {
		t.Fatalf("unexpected projects: %+v", projects)
	}
	general := projects[i]
	base := "/api/projects/" + general.Namespace
	if status := f.request("GET", base+"/snapshot?manifest=1", other, nil, nil); status != http.StatusNotFound {
		t.Fatal("General was accessible to a different account", status)
	}
	if status := f.request("POST", base+"/invites", human, map[string]string{}, nil); status != http.StatusConflict {
		t.Fatal("allowed a General membership invite", status)
	}
	if status := f.request("DELETE", base, human, nil, nil); status != http.StatusConflict {
		t.Fatal("allowed a standalone General deletion", status)
	}
	var manifest model.SyncManifest
	if status := f.request("GET", base+"/snapshot?manifest=1", human, nil, &manifest); status != http.StatusOK || manifest.Files == nil || len(manifest.Files) != 0 {
		t.Fatal("new General could not be pulled", status, manifest)
	}

	var generalID string
	if err := f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		row, err := q.GeneralProject(uid)
		generalID = row.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var listing struct{ Projects []model.Project }
	for _, tc := range []struct {
		id, namespace string
	}{{regular.ID, regular.Namespace}, {generalID, general.Namespace}} {
		access := connectorAccessToken(t, f, human, tc.id)
		msg := connectorTool(t, f, access, "projects_list", nil, &listing)
		if msg != "" || len(listing.Projects) != 1 || listing.Projects[0].Namespace != tc.namespace {
			t.Fatal("connector returned the wrong granted project", msg, listing)
		}
	}
	if status := f.request("DELETE", "/api/projects/"+regular.Namespace, human, nil, nil); status != http.StatusOK {
		t.Fatal("ordinary project with slug general could not be deleted", status)
	}
}
