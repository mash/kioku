package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func TestPublicProjectSurfacesDoNotExposeInternalID(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	const jobID = "public-job"
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		fixtureJobsPut(t, st, jobID, fixtureJob{ID: jobID, ProjectID: p.ID, UserID: "owner", Status: "failed", Kind: "generate", Created: time.Now(), Error: "storage projects/" + p.ID + "/failed"})
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
	if !slices.Equal(options, []string{p.Namespace, "owner/general"}) {
		t.Fatal(options)
	}
	// Persistence retains the ID; only the public representation changes.
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureProjectsGet(t, st, p.ID).ID != p.ID {
			t.Fatal("internal ID lost")
		}
		return nil
	})
}

func TestPublicProjectAPIRejectsIDSelectors(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	req := httptest.NewRequest("GET", "/api/projects/"+p.ID+"/snapshot?manifest=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, req)
	if out.Code != 404 {
		t.Fatal(out.Code, out.Body.String())
	}
}

func TestProjectPageShowsFiveRecentGenerations(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
		for i, id := range []string{"oldest", "older", "third", "fourth", "fifth", "latest-a", "latest-b"} {
			created := min(i, 5)
			if err := q.PutJob(store.JobRow{ID: id, ProjectID: p.ID, Status: "completed", Kind: "generate", Created: int64(created)}); err != nil {
				return err
			}
		}
		if err := q.PutJob(store.JobRow{ID: "view-child", ProjectID: p.ID, Status: "completed", Kind: "view", Created: 10, ParentID: store.NullableString("latest-b"), Data: store.JobData{ViewTask: &store.ViewTaskData{}}}); err != nil {
			return err
		}
		if err := q.PutJob(store.JobRow{ID: "view-consumed", ProjectID: p.ID, Status: "completed", Kind: "view", Created: 10}); err != nil {
			return err
		}
		return q.PutJob(store.JobRow{ID: "unrelated", ProjectID: "other", Status: "completed", Kind: "generate", Created: 10})
	}))
	req := httptest.NewRequest("GET", "/"+p.Namespace, nil)
	req.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	body := out.Body.String()
	if got := strings.Count(body, ">生成ジョブ</a>"); got != 5 {
		t.Fatalf("recent job count = %d", got)
	}
	for _, id := range []string{"view-child", "view-consumed"} {
		if code := f.request("GET", "/api/projects/"+p.Namespace+"/jobs/"+id, token, nil, nil); code != 404 {
			t.Fatalf("child job %s became public: %d", id, code)
		}
	}
	previous := -1
	for _, id := range []string{"latest-b", "latest-a", "fifth", "fourth", "third"} {
		index := strings.Index(body, "/jobs/"+id+`"`)
		if index <= previous {
			t.Fatalf("missing or misordered recent job %q: %s", id, body)
		}
		previous = index
	}
	for _, id := range []string{"oldest", "older", "view-child", "view-consumed", "unrelated"} {
		if strings.Contains(body, "/jobs/"+id+`"`) {
			t.Fatalf("unexpected recent job %q", id)
		}
	}
}
