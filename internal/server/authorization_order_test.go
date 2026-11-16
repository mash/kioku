package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/store"
)

// A caller without permission must get the authorization error, not a hint
// about which input would be valid.
func TestAuthorizationRunsBeforeInputValidation(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	other := f.login("member@example.com")
	p := securityProject(t, f, human)
	fixtureCheck(t, f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		v := fixtureProjectsGet(t, st, p.ID)
		v.Members[accountID(t, f, other)] = "member"
		fixtureProjectsPut(t, st, v.ID, v)
		return nil
	}))
	var org string
	fixtureCheck(t, f.stateStore.Read(context.Background(), func(st *store.Queries) error { org = fixtureOrganizationsGet(t, st, p.OrgID).Slug; return nil }))
	invalid := map[string]any{"unknown": true}
	for _, tc := range []struct {
		method, path  string
		member, owner int
	}{
		{"POST", "/api/projects/" + p.ID + "/views", 403, 400},
		{"PATCH", "/api/projects/" + p.ID + "/views/project-map", 403, 400},
		{"POST", "/api/projects/" + p.ID + "/owner", 403, 400},
		{"POST", "/api/orgs/" + org + "/owner", 404, 400},
	} {
		if c := f.request(tc.method, tc.path, other, invalid, nil); c != tc.member {
			t.Errorf("%s %s by a member: %d, want %d", tc.method, tc.path, c, tc.member)
		}
		if c := f.request(tc.method, tc.path, human, invalid, nil); c != tc.owner {
			t.Errorf("%s %s by the owner: %d, want %d", tc.method, tc.path, c, tc.owner)
		}
	}
	if c := f.request("DELETE", "/api/account", "", invalid, nil); c != 401 {
		t.Errorf("account deletion without a session: %d", c)
	}
}
