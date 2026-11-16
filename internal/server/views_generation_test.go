package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner/claudemanaged/fake"
	"github.com/mash/kioku/internal/store"
)

func createTestView(t *testing.T, f *fixture, token string, p fixtureProject, slug, prompt string) {
	t.Helper()
	if code := f.request("POST", "/api/projects/"+p.ID+"/views", token, map[string]string{"slug": slug, "title": slug, "prompt": prompt}, nil); code != 201 {
		t.Fatalf("create view: %d", code)
	}
}

func queueTestViewGeneration(t *testing.T, f *fixture, token string, p fixtureProject) fixtureJob {
	t.Helper()
	var accepted struct {
		ID         string                `json:"id"`
		WikiStatus string                `json:"wiki_status"`
		Views      []model.ViewJobResult `json:"views"`
	}
	if code := f.request("POST", "/api/projects/"+p.ID+"/generate", token, map[string]int{"budget_cents": 200}, &accepted); code != 202 || accepted.WikiStatus != "pending" || accepted.Views == nil || len(accepted.Views) != 0 {
		t.Fatalf("generate returned %d: %+v", code, accepted)
	}
	return startTestGeneration(t, f, accepted.ID)
}

func startTestGeneration(t *testing.T, f *fixture, id string) fixtureJob {
	t.Helper()
	var job fixtureJob
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		job = fixtureJobsGet(t, st, id)
		job.Status = "queued"
		if job.WikiStatus != "" {
			job.WikiStatus = "running"
		}
		fixtureJobsPut(t, st, id, job)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.startAgentJob(t.Context(), fixtureRow(job)); err != nil {
		t.Fatal(err)
	}
	return storedJob(t, f, id)
}

func nextTestViewTask(t *testing.T, f *fixture, parent fixtureJob) fixtureJob {
	t.Helper()
	if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	job := storedJob(t, f, parent.ID)
	if job.ViewGeneration == nil || job.ViewGeneration.ChildID == "" {
		t.Fatalf("no View task queued: %+v", job)
	}
	return startTestGeneration(t, f, job.ViewGeneration.ChildID)
}

func completeTestViewTask(t *testing.T, f *fixture, ma *fake.Server, child fixtureJob, html string, cost int64) {
	t.Helper()
	payload := outputArchive(t, map[string]string{"index.html": html, "summary.md": "# Delivery"})
	token := ma.Credential(child.Run.Ref.Resources["vault_id"])
	if status, body := agentRequest(t, f, child, token, "PUT", "output", payload); status != 200 {
		t.Fatalf("View upload returned %d: %s", status, body)
	}
	ma.SetStatus(child.Run.Ref.ID, "idle", "end_turn", cost)
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(child)); err != nil {
		t.Fatal(err)
	}
}

func TestProjectGenerationPublishesViewsInBudgetOrder(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	var projectMap viewDefinition
	if code := f.request("GET", "/api/projects/"+p.ID+"/views/project-map", token, nil, &projectMap); code != 200 {
		t.Fatal(code)
	}
	createTestView(t, f, token, p, "delivery", "Show this week's open work")
	putFile(t, f, token, p.ID, "delivery.md", "Deliver the report on Friday.")
	parent := queueTestViewGeneration(t, f, token, p)
	if parent.Run == nil || parent.Run.BudgetCents != 200 {
		t.Fatalf("Wiki should receive the entire budget: %+v", parent)
	}
	sendOutput(t, f, ma, parent, "Complete Wiki")
	ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 100)
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
		t.Fatal(err)
	}
	restartManagedFixture(t, f)
	first := nextTestViewTask(t, f, parent)
	if first.Run == nil || first.Run.BudgetCents != 100 {
		t.Fatalf("first View did not get 100 cents: %+v", first)
	}
	if first.Run.Ref.ID == "" || first.Run.Ref.ID == parent.Run.Ref.ID {
		t.Fatal("the first View must have its own runner session")
	}
	if got := first.Run.Deadline.Sub(first.Run.PreparedAt); got != f.s.Config.AgentJobTimeout {
		t.Fatalf("first View timeout = %s, want %s", got, f.s.Config.AgentJobTimeout)
	}
	raw := ma.Credential(first.Run.Ref.Resources["vault_id"])
	status, body := agentRequest(t, f, first, raw, "GET", "input", nil)
	if status != 200 {
		t.Fatalf("View input returned %d: %s", status, body)
	}
	input := readInput(t, body)
	if input["VIEW.md"] != projectMap.Prompt || !strings.Contains(input["wiki/index.md"], "Complete Wiki") {
		t.Fatal("View did not receive its prompt and completed Wiki")
	}
	for name := range input {
		if strings.HasPrefix(name, "inbox/") {
			t.Fatalf("View received an inbox file: %s", name)
		}
	}
	if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+first.ID, token, nil, nil); code != 404 {
		t.Fatalf("private View task returned %d", code)
	}
	var progress struct {
		Status     string                `json:"status"`
		WikiStatus string                `json:"wiki_status"`
		Views      []model.ViewJobResult `json:"views"`
	}
	if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &progress); code != 200 ||
		progress.Status != "running" || progress.WikiStatus != "completed" ||
		len(progress.Views) != 2 || progress.Views[0].Status != "running" {
		t.Fatalf("unexpected progress %d: %+v", code, progress)
	}
	completeTestViewTask(t, f, ma, first, "<h1>Project overview</h1>", 50)
	second := nextTestViewTask(t, f, parent)
	if second.Run == nil || second.Run.BudgetCents != 50 {
		t.Fatalf("second View did not get 50 cents: %+v", second)
	}
	if second.Run.Ref.ID == "" || second.Run.Ref.ID == first.Run.Ref.ID || second.Run.Ref.ID == parent.Run.Ref.ID {
		t.Fatal("the second View must have its own runner session")
	}
	completeTestViewTask(t, f, ma, second, `<h1>Delivery</h1><details><summary>Open work</summary><p>Friday</p></details><a href="wiki/index.md">Wiki index</a>`, 50)
	if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	var job struct {
		Status     string                `json:"status"`
		WikiStatus string                `json:"wiki_status"`
		Views      []model.ViewJobResult `json:"views"`
	}
	if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &job); code != 200 ||
		job.Status != "completed" || job.WikiStatus != "completed" || len(job.Views) != 2 ||
		job.Views[0].Slug != "project-map" || job.Views[0].Status != "completed" ||
		job.Views[1].Slug != "delivery" || job.Views[1].Status != "completed" {
		t.Fatalf("unexpected job response %d: %+v", code, job)
	}
	path := "/" + p.Namespace + "/views/delivery"
	get := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
		}
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"", f.login("outside-view@example.com")} {
		if response := get(token); response.Code != 404 || strings.Contains(response.Body.String(), "Open work") {
			t.Fatalf("unauthorized View response %d", response.Code)
		}
	}
	w := get(token)
	if html := w.Body.String(); w.Code != 200 || !strings.Contains(html, `sandbox srcdoc="`) ||
		!strings.Contains(html, "Open work") || !strings.Contains(html, "Wiki の出典") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "img-src data:") {
		t.Fatalf("View page response %d: %s", w.Code, html)
	}
	access := connectorAccessToken(t, f, token, p.ID)
	var list struct {
		Views []struct {
			Slug   string `json:"slug"`
			Status string `json:"status"`
		} `json:"views"`
	}
	// E2/K0: public creation times are milliseconds; ties use the view ID.
	firstSlug, secondSlug := "wiki", "project-map"
	if projectMap.CreatedAt.Before(p.Created) || projectMap.CreatedAt.Equal(p.Created) && projectMap.ID < newWikiViewRow(p.ID).ID {
		firstSlug, secondSlug = secondSlug, firstSlug
	}
	if msg := connectorTool(t, f, access, "view_list", map[string]string{"project": p.Namespace}, &list); msg != "" ||
		len(list.Views) != 3 || list.Views[0].Slug != firstSlug || list.Views[1].Slug != secondSlug ||
		list.Views[2].Slug != "delivery" || list.Views[2].Status != "completed" {
		t.Fatalf("view_list: %s %+v", msg, list)
	}
	args := map[string]any{"project": p.Namespace, "view": "delivery", "path": "summary.md"}
	var read connectorReadResult
	if msg := connectorTool(t, f, access, "view_read", args, &read); msg != "" || read.Content != "# Delivery" || read.NextOffset != nil {
		t.Fatalf("view_read: %s %+v", msg, read)
	}
	args["path"] = "data.json"
	if msg := connectorTool(t, f, access, "view_read", args, nil); msg == "" {
		t.Fatal("missing View file was readable")
	}
	var other fixtureProject
	if code := f.request("POST", "/api/projects", token, map[string]string{"name": "acme/other"}, &other); code != 201 {
		t.Fatal("create unrelated project", code)
	}
	ungranted := connectorAccessToken(t, f, token, other.ID)
	args["path"] = "summary.md"
	if msg := connectorTool(t, f, ungranted, "view_read", args, nil); msg != "project not found" {
		t.Fatalf("View outside OAuth project selection: %q", msg)
	}
}

func TestRemainingViewBudgetUsesReportedCosts(t *testing.T) {
	known, tooHigh, negative := int64(37), int64(210), int64(-1)
	for _, tc := range []struct {
		cost *int64
		want int64
	}{{nil, 200}, {&known, 163}, {&tooHigh, 0}, {&negative, 200}} {
		if got := remainingViewBudget(200, tc.cost); got != tc.want {
			t.Errorf("remaining budget = %d, want %d", got, tc.want)
		}
	}
}

func TestViewGenerationUsesRemainingBudgetWhenWikiCostIsUnknown(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	createTestView(t, f, token, p, "delivery", "Show decisions")
	putFile(t, f, token, p.ID, "decision.md", "Decide on Friday.")
	parent := queueTestViewGeneration(t, f, token, p)
	sendOutput(t, f, ma, parent, "Complete Wiki")
	ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 100)
	session := ma.Session(parent.Run.Ref.ID)
	session.CostAmount = ""
	ma.SetSession(session)
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
		t.Fatal(err)
	}
	first := nextTestViewTask(t, f, parent)
	if first.Run == nil || first.Run.BudgetCents != 200 {
		t.Fatalf("unknown Wiki cost was deducted: %+v", first)
	}
	completeTestViewTask(t, f, ma, first, "<h1>Decisions</h1>", 150)
	second := nextTestViewTask(t, f, parent)
	if second.Run == nil || second.Run.BudgetCents != 50 {
		t.Fatalf("known View cost was not deducted: %+v", second)
	}
	completeTestViewTask(t, f, ma, second, "<h1>Project map</h1>", 50)
	if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
}

func TestViewRegenerationKeepsThePreviousResultUntilSourceRemoval(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	viewAPI := "/api/projects/" + p.ID + "/views/project-map"
	source := "https://example.com/view-evidence"
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{
		File: &model.FileInput{Name: "evidence.md", Content: testSourceContent(source, "Original evidence")},
	}, nil); code != 202 {
		t.Fatal("put source", code)
	}
	finishWiki := func(job fixtureJob, title string) {
		t.Helper()
		sendOutput(t, f, ma, job, title)
		ma.SetStatus(job.Run.Ref.ID, "idle", "end_turn", 100)
		if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(job)); err != nil {
			t.Fatal(err)
		}
	}
	advance := func(job fixtureJob) {
		t.Helper()
		if err := f.s.storage.advanceViews(t.Context(), job.ID); err != nil {
			t.Fatal(err)
		}
	}
	detail := func() viewDetail {
		t.Helper()
		var v viewDetail
		if code := f.request("GET", viewAPI, token, nil, &v); code != 200 {
			t.Fatal("View detail", code)
		}
		return v
	}
	parent := queueTestViewGeneration(t, f, token, p)
	finishWiki(parent, "Original Wiki")
	first := nextTestViewTask(t, f, parent)
	completeTestViewTask(t, f, ma, first, "<h1>Original View</h1>", 50)
	advance(parent)
	initial := detail()

	update := model.PutRequest{File: &model.FileInput{
		Name: "evidence.md", Content: testSourceContent(source, "Updated evidence"),
	}}
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, update, nil); code != 202 {
		t.Fatal("update source", code)
	}
	parent = queueTestViewGeneration(t, f, token, p)
	sendOutput(t, f, ma, parent, "Updated Wiki")
	ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 200)
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
		t.Fatal(err)
	}
	advance(parent)
	if v := detail(); v.Status != "skipped" || v.JobID != initial.JobID || !strings.Contains(v.Error, "予算") {
		t.Fatalf("exhausted budget should keep the previous result: %+v", v)
	}

	parent = queueTestViewGeneration(t, f, token, p)
	if !parent.Unchanged || parent.Run != nil {
		t.Fatalf("change-free Wiki should reuse the published Wiki: %+v", parent)
	}
	child := nextTestViewTask(t, f, parent)
	for _, prompt := range []string{"Temporary prompt", initial.Prompt} {
		if code := f.request("PATCH", viewAPI, token, map[string]string{"prompt": prompt}, nil); code != 200 {
			t.Fatal("update prompt", code)
		}
	}
	completeTestViewTask(t, f, ma, child, "<h1>Stale View</h1>", 50)
	advance(parent)
	if v := detail(); v.Status != "completed" || v.JobID != parent.ID {
		t.Fatalf("settings change should publish the result: %+v", v)
	}
	initial = detail()

	parent = queueTestViewGeneration(t, f, token, p)
	child = nextTestViewTask(t, f, parent)
	raw := ma.Credential(child.Run.Ref.Resources["vault_id"])
	if code, _ := agentRequest(t, f, child, raw, "PUT", "output", outputArchive(t, map[string]string{"index.html": "<form>Invalid element</form>"})); code != 400 {
		t.Fatalf("invalid View returned %d", code)
	}
	ma.SetStatus(child.Run.Ref.ID, "idle", "end_turn", 50)
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(child)); err != nil {
		t.Fatal(err)
	}
	advance(parent)
	if v := detail(); v.Status != "failed" || v.JobID != initial.JobID || v.Error != errViewHTML.Error() {
		t.Fatalf("failed generation should keep the previous result: %+v", v)
	}

	parent = queueTestViewGeneration(t, f, token, p)
	child = nextTestViewTask(t, f, parent)
	if code := f.request("DELETE", viewAPI, token, nil, nil); code != 204 {
		t.Fatalf("delete running View: %d", code)
	}
	if code := f.request("GET", viewAPI, token, nil, nil); code != 404 {
		t.Fatalf("deleted View is still accessible: %d", code)
	}
	createTestView(t, f, token, p, "project-map", "Replacement prompt")
	completeTestViewTask(t, f, ma, child, "<h1>Deleted View</h1>", 50)
	advance(parent)
	deleted := storedJob(t, f, parent.ID)
	if len(deleted.Views) != 1 || deleted.Views[0].ViewID != initial.ID || deleted.Views[0].Status != "skipped" || deleted.Views[0].Error != viewDeleted {
		t.Fatalf("late result for deleted View: %+v", deleted.Views)
	}
	if v := detail(); v.Status != "pending" || v.ID == initial.ID || v.JobID != "" {
		t.Fatalf("old run changed replacement View: %+v", v)
	}
	parent = queueTestViewGeneration(t, f, token, p)
	child = nextTestViewTask(t, f, parent)
	completeTestViewTask(t, f, ma, child, "<h1>Replacement View</h1>", 50)
	advance(parent)
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: source}, nil); code != 202 {
		t.Fatal("remove source", code)
	}
	parent = queueTestViewGeneration(t, f, token, p)
	finishWiki(parent, "Wiki without the removed source")
	if v := detail(); v.JobID != "" || v.GeneratedAt != nil {
		t.Fatalf("publishing a source removal should invalidate the old result: %+v", v)
	}
	access := connectorAccessToken(t, f, token, p.ID)
	args := map[string]any{"project": p.Namespace, "view": "project-map", "path": "summary.md"}
	if msg := connectorTool(t, f, access, "view_read", args, nil); msg == "" {
		t.Fatal("invalidated View summary was still accessible")
	}
}

func TestViewPromptEditsUseArchiveBuildTime(t *testing.T) {
	for _, beforeStart := range []bool{false, true} {
		t.Run(fmt.Sprint("before-start=", beforeStart), func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "source.md", "Evidence")
			parent := queueTestViewGeneration(t, f, token, p)
			sendOutput(t, f, ma, parent, "Wiki")
			ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 50)
			if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
				t.Fatal(err)
			}
			if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
				t.Fatal(err)
			}
			parent = storedJob(t, f, parent.ID)
			childID := parent.ViewGeneration.ChildID
			var child fixtureJob
			if !beforeStart {
				child = startTestGeneration(t, f, childID)
			}
			api := "/api/projects/" + p.ID + "/views/project-map"
			var edited viewDefinition
			if code := f.request("PATCH", api, token, map[string]string{"prompt": "判断待ちを表示する"}, &edited); code != 200 || edited.Status != "running" || edited.Error != "" {
				t.Fatalf("edit: %d %+v", code, edited)
			}
			if beforeStart {
				child = startTestGeneration(t, f, childID)
			}
			checkPrompt := func(child fixtureJob, wantNew bool) {
				t.Helper()
				raw := ma.Credential(child.Run.Ref.Resources["vault_id"])
				code, body := agentRequest(t, f, child, raw, "GET", "input", nil)
				if code != 200 {
					t.Fatalf("input: %d", code)
				}
				prompt := readInput(t, body)["VIEW.md"]
				if (prompt == edited.Prompt) != wantNew || prompt == "" {
					t.Fatalf("prompt = %q, want new = %v", prompt, wantNew)
				}
			}
			checkPrompt(child, beforeStart)
			completeTestViewTask(t, f, ma, child, "<h1>Published</h1>", 50)
			if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
				t.Fatal(err)
			}
			parent = storedJob(t, f, parent.ID)
			if parent.Status != "completed" || parent.Views[0].Status != "completed" {
				t.Fatalf("parent: %+v", parent)
			}
			var view viewDefinition
			if code := f.request("GET", api, token, nil, &view); code != 200 || view.Status != "completed" || view.Prompt != edited.Prompt {
				t.Fatalf("published: %d %+v", code, view)
			}
			parent = queueTestViewGeneration(t, f, token, p)
			child = nextTestViewTask(t, f, parent)
			checkPrompt(child, true)
		})
	}
}

func TestViewGenerationStopsAfterFailure(t *testing.T) {
	for _, scenario := range []string{"failed", "missing", "failed-deleted", "remaining-deleted", "skipped-deleted", "write-retry"} {
		t.Run(scenario, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			api := "/api/projects/" + p.ID + "/views/"
			if code := f.request("DELETE", api+"project-map", token, nil, nil); code != 204 {
				t.Fatal(code)
			}
			for i, slug := range []string{"a-board", "b-map", "c-list"} {
				createTestView(t, f, token, p, slug, "Prompt")
				view, err := f.s.viewBySlugLocked(t.Context(), p.ID, slug)
				if err != nil {
					t.Fatal(err)
				}
				created := time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)
				view.CreatedAt = &created
				fixtureViewCreated(t, f, p.ID, view.ID, created)
			}
			putFile(t, f, token, p.ID, "source.md", "Evidence")
			parent := queueTestViewGeneration(t, f, token, p)
			sendOutput(t, f, ma, parent, "Wiki")
			ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 50)
			if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
				t.Fatal(err)
			}
			child := nextTestViewTask(t, f, parent)
			parent = storedJob(t, f, parent.ID)
			if parent.Views[0].Slug != "a-board" {
				t.Fatal(parent.Views)
			}
			if scenario == "failed-deleted" || scenario == "skipped-deleted" {
				if code := f.request("DELETE", api+"a-board", token, nil, nil); code != 204 {
					t.Fatal(code)
				}
			}
			if scenario == "remaining-deleted" {
				if code := f.request("DELETE", api+"b-map", token, nil, nil); code != 204 {
					t.Fatal(code)
				}
			}
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				child.Status, child.Error = "failed", "test failure"
				if scenario == "skipped-deleted" {
					child.Status, child.Error = "skipped", viewDeleted
				}
				child.Run.CleanupComplete = true
				fixtureJobsPut(t, st, child.ID, child)
				if e := st.RecordUsage(store.UsageRow{AttemptID: child.ID, JobID: child.ID, ProjectID: p.ID, Data: model.UsageValues{UserID: child.UserID}}); e != nil {
					return e
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing" {
				fixtureSQL(t, f, "DELETE FROM jobs WHERE id='"+child.ID+"'")
				if err := f.s.storage.advanceViews(t.Context(), parent.ID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("missing child must be unavailable", err)
				}
				if got := storedJob(t, f, parent.ID); got.ViewGeneration.ChildID != child.ID {
					t.Fatal("missing child checkpoint lost", got)
				}
				return
			}
			if scenario == "write-retry" {
				restore := fixtureSQLFault(t, f, "views", "UPDATE", "1")
				if err := f.s.storage.advanceViews(t.Context(), parent.ID); err == nil {
					t.Fatal("expected write failure")
				}
				checkpoint := storedJob(t, f, parent.ID)
				if !reflect.DeepEqual(checkpoint, parent) {
					t.Fatalf("parent changed before all writes succeeded: %+v", checkpoint)
				}
				restore()
			}
			if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
				t.Fatal(err)
			}
			parent = storedJob(t, f, parent.ID)
			if scenario == "skipped-deleted" || scenario == "failed-deleted" {
				if parent.Views[0].Status != "skipped" || parent.Views[0].Error != viewDeleted || parent.Views[1].Status != "running" || parent.ViewGeneration == nil {
					t.Fatal(parent)
				}
				next := startTestGeneration(t, f, parent.ViewGeneration.ChildID)
				completeTestViewTask(t, f, ma, next, "<h1>Next</h1>", 50)
				return
			}
			if parent.Status != "completed" || parent.ViewGeneration != nil {
				t.Fatal(parent)
			}
			if scenario == "missing" && parent.Views[0].Error != "View の生成ジョブが見つかりません" {
				t.Fatal(parent.Views)
			}
			for _, result := range parent.Views[1:] {
				if scenario == "remaining-deleted" && result.Slug == "b-map" {
					if result.Status != "skipped" || result.Error != viewDeleted {
						t.Fatal(result)
					}
					continue
				}
				if result.Status != "skipped" || result.Error != viewPreviousFailed {
					t.Fatal(result)
				}
				var view viewDefinition
				if code := f.request("GET", api+result.Slug, token, nil, &view); code != 200 || view.Status != "skipped" || view.Error != viewPreviousFailed {
					t.Fatalf("view: %d %+v", code, view)
				}
			}
		})
	}
}

func TestViewGenerationCreationOrderAndPendingCapture(t *testing.T) {
	for _, stage := range []string{"captured", "empty-inbox", "publish"} {
		t.Run(stage, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			api := "/api/projects/" + p.ID + "/views/"
			if code := f.request("DELETE", api+"project-map", token, nil, nil); code != 204 {
				t.Fatal(code)
			}
			for i, slug := range []string{"zeta", "alpha"} {
				createTestView(t, f, token, p, slug, slug)
				view, err := f.s.viewBySlugLocked(t.Context(), p.ID, slug)
				if err != nil {
					t.Fatal(err)
				}
				created := time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)
				view.CreatedAt = &created
				fixtureViewCreated(t, f, p.ID, view.ID, created)
			}
			putFile(t, f, token, p.ID, "source.md", "Evidence")
			parent := queueTestViewGeneration(t, f, token, p)
			sendOutput(t, f, ma, parent, "Wiki")
			ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 50)
			if stage != "publish" {
				if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "publish" {
				if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
					t.Fatal(err)
				}
			} else if stage == "empty-inbox" {
				if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
					job := fixtureJobsGet(t, st, parent.ID)
					job.Status = "completed"
					job.ViewGeneration = nil
					fixtureJobsPut(t, st, job.ID, job)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				parent = queueTestViewGeneration(t, f, token, p)
			}
			parent = storedJob(t, f, parent.ID)
			if parent.ViewGeneration == nil || parent.ViewGeneration.Pending {
				t.Fatal("view list was not captured in SQL transaction", parent)
			}
			if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
				t.Fatal(err)
			}
			parent = storedJob(t, f, parent.ID)
			if len(parent.Views) != 2 || parent.Views[0].Slug != "zeta" || parent.Views[1].Slug != "alpha" || parent.Views[0].Status != "running" || parent.ViewGeneration.Pending {
				t.Fatal(parent.Views)
			}
			child := startTestGeneration(t, f, parent.ViewGeneration.ChildID)
			raw := ma.Credential(child.Run.Ref.Resources["vault_id"])
			code, body := agentRequest(t, f, child, raw, "GET", "input", nil)
			if code != 200 || readInput(t, body)["VIEW.md"] != "zeta" {
				t.Fatalf("first child input = %d: %s", code, body)
			}
		})
	}
}

func TestViewGenerationCapturedEmptyListDoesNotRecapture(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	if code := f.request("DELETE", "/api/projects/"+p.ID+"/views/project-map", token, nil, nil); code != 204 {
		t.Fatal(code)
	}
	parent := queueTestViewGeneration(t, f, token, p)
	if parent.ViewGeneration != nil || len(parent.Views) != 0 {
		t.Fatal(parent)
	}
	createTestView(t, f, token, p, "later", "Later")
	if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	parent = storedJob(t, f, parent.ID)
	if parent.Status != "completed" || len(parent.Views) != 0 {
		t.Fatal(parent)
	}
}
