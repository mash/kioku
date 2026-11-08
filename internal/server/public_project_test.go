package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func TestPublicProjectSurfacesDoNotExposeInternalID(t *testing.T) {
	f, token, p := orgProject(t)
	const jobID = "public-job"
	if err := f.db.Update(func(st *model.State) error {
		st.Jobs[jobID] = model.Job{ID: jobID, ProjectID: p.ID, UserID: "owner", Status: "failed", Kind: "generate", Created: time.Now(), Error: "storage projects/" + p.ID + "/failed"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivate := func(body []byte) {
		t.Helper()
		for _, forbidden := range []string{p.ID, `"project_id"`, `"storage_root"`, `"storage_bucket"`} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("internal project metadata exposed: %s", body)
			}
		}
	}
	for _, path := range []string{"/api/projects", "/api/projects/" + p.Namespace + "/jobs/" + jobID, "/api/projects/" + p.Namespace + "/audit"} {
		var response json.RawMessage
		if status := f.request("GET", path, token, nil, &response); status != 200 {
			t.Fatal(path, status)
		}
		assertPrivate(response)
	}
	// Inspect real HTML, including href, form actions and hidden values.
	for _, path := range []string{"/", "/" + p.Namespace} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
		out := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatal(path, out.Code)
		}
		assertPrivate(out.Body.Bytes())
	}
	params := connectorAuthorizeParams(f, connectorClient(t, f), auth.Token())
	page := connectorForm(f, "GET", "/oauth/authorize", token, params)
	assertPrivate(page.Body.Bytes())
	_, _, options := consentControls(t, page)
	if len(options) != 1 || options[0] != p.Namespace {
		t.Fatal(options)
	}
	// Persistence retains the ID; only the public representation changes.
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].ID != p.ID {
			t.Fatal("internal ID lost")
		}
		return nil
	})
}

func TestPublicProjectAPIRejectsIDSelectors(t *testing.T) {
	f, token, p := orgProject(t)
	req := httptest.NewRequest("GET", "/api/projects/"+p.ID+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, req)
	if out.Code != 404 {
		t.Fatal(out.Code, out.Body.String())
	}
}
