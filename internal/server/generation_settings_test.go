package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestServerGenerationBudget(t *testing.T) {
	for _, budget := range []int64{0, 50, 100} {
		t.Run(strconv.FormatInt(budget, 10), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			putFile(t, f, token, p.ID, "budget.md", "Evidence")
			var j model.Job
			if code := f.request("POST", "/api/projects/"+p.Namespace+"/generate", token, map[string]any{"budget_cents": budget}, &j); code != 202 {
				t.Fatal(code)
			}
			if err := f.db.Update(func(st *model.State) error {
				j = st.Jobs[j.ID]
				j.Status = "running"
				st.Jobs[j.ID] = j
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.s.startAgentJob(t.Context(), j); err != nil {
				t.Fatal(err)
			}
			actual := storedJob(t, f, j.ID)
			want := budget
			if want == 0 {
				want = f.s.Config.AgentMaxSessionCents
			}
			if actual.Run == nil || actual.Run.BudgetCents != want {
				t.Fatalf("want budget %d, got %+v", want, actual.Run)
			}
		})
	}
	f, token, p, _ := managedFixture(t)
	for _, budget := range []int64{-1, 49, f.s.Config.AgentMaxSessionCents + 1} {
		if code := f.request("POST", "/api/projects/"+p.Namespace+"/generate", token, map[string]any{"budget_cents": budget}, nil); code != 400 {
			t.Fatal(budget, code)
		}
	}
}

func TestRetiredBYOKJobCannotRetry(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	j := managedJob(t, f, token, p.ID, true)
	if err := f.db.Update(func(st *model.State) error {
		j.Status = "failed"
		j.AnthropicConnectionID = "retired"
		st.Jobs[j.ID] = j
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := f.request("POST", "/api/projects/"+p.Namespace+"/jobs/"+j.ID+"/retry", token, nil, nil); code != 409 {
		t.Fatal(code)
	}
	if got := storedJob(t, f, j.ID); got.Status != "failed" {
		t.Fatal(got)
	}
	if len(ma.Requests()) != 0 {
		t.Fatal("retired job contacted provider")
	}
}

func TestGenerationBrowserWithoutPersonalConnection(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	req, _ := http.NewRequest("GET", f.url+"/"+p.Namespace, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(body), "budget_usd") || strings.Contains(string(body), "/settings/anthropic") {
		t.Fatal(resp.StatusCode, string(body))
	}
	req, _ = http.NewRequest("POST", f.url+"/api/projects/"+p.Namespace+"/generate", strings.NewReader("budget_usd=0.50"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || strings.Contains(string(body), "Console") {
		t.Fatal(resp.StatusCode, string(body))
	}
	if err := f.db.View(func(st model.State) error {
		found := false
		for _, j := range st.Jobs {
			if j.ProjectID == p.ID && j.Kind == "generate" {
				found = true
				if j.AgentBudgetCents != 50 || j.AnthropicConnectionID != "" {
					t.Fatal(j)
				}
			}
		}
		if !found {
			t.Fatal("no browser job")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalKeyRoutesRemoved(t *testing.T) {
	f, token, _, _ := managedFixture(t)
	for _, route := range []struct{ method, path string }{
		{"GET", "/settings/anthropic"},
		{"POST", "/settings/anthropic"},
		{"POST", "/settings/anthropic/disconnect"},
		{"PUT", "/api/account/anthropic"},
		{"DELETE", "/api/account/anthropic"},
	} {
		code := f.request(route.method, route.path, token, nil, nil)
		if code != 404 && code != 405 {
			t.Fatal(route, code)
		}
	}
}
