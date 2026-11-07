package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func putFile(t *testing.T, f *fixture, token, pid, name, body string) model.Job {
	t.Helper()
	var j model.Job
	if c := f.request("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{Files: []model.FileInput{{Name: name, Content: body, SourceURL: testSourceURL(name, body)}}}, &j); c != 202 {
		t.Fatal(c)
	}
	if j.Kind != "put" || j.Status != "completed" || j.Revision != "" {
		t.Fatal("unexpected source receipt", j)
	}
	return j
}

func TestPeriodicGenerationOnlyForChangesAndFixedDeadline(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	const interval = 24 * time.Hour
	f.s.Config.GenerateEvery = interval
	now := time.Now().UTC()
	if err := f.s.scheduleGenerations(now); err != nil {
		t.Fatal(err)
	}
	putFile(t, f, token, p.ID, "one.md", "First fact")
	if err := f.s.scheduleGenerations(now.Add(interval / 2)); err != nil {
		t.Fatal(err)
	}
	putFile(t, f, token, p.ID, "two.md", "Second fact")
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].NextGenerateAt != now.Add(interval) {
			t.Error("put postponed deadline")
		}
		for _, j := range st.Jobs {
			if j.Kind != "put" {
				t.Error("put triggered generation")
			}
		}
		if len(st.Revisions) != 0 {
			t.Error("put published wiki")
		}
		return nil
	})
	if err := f.s.scheduleGenerations(now.Add(interval)); err != nil {
		t.Fatal(err)
	}
	var job model.Job
	_ = f.db.View(func(st model.State) error {
		for _, j := range st.Jobs {
			if j.Kind == "generate" {
				if job.ID != "" {
					t.Error("duplicate generation")
				}
				job = j
			}
		}
		return nil
	})
	if job.ID == "" || !job.Automatic || job.Full {
		t.Fatal("periodic generation missing", job)
	}
	if _, err := f.s.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := f.s.scheduleGenerations(now.Add(2 * interval)); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		if p.SourceVersion != 2 || p.WikiSourceVersion != 2 || p.Revision == "" {
			t.Error("freshness tracking", p)
		}
		n := 0
		for _, j := range st.Jobs {
			if j.Kind == "generate" {
				n++
			}
		}
		if n != 1 {
			t.Error("unchanged project generated again", n)
		}
		return nil
	})
}

func TestDisablingSchedulingClearsPersistedDeadline(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	f.s.Config.GenerateEvery = time.Hour
	now := time.Now().UTC()
	if err := f.s.scheduleGenerations(now); err != nil {
		t.Fatal(err)
	}
	putFile(t, f, token, p.ID, "one.md", "Dirty but manual only")
	f.s.Config.GenerateEvery = 0
	if err := f.s.scheduleGenerations(now.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if !st.Projects[p.ID].NextGenerateAt.IsZero() {
			t.Error("disabled schedule still advertises deadline")
		}
		for _, j := range st.Jobs {
			if j.Kind == "generate" {
				t.Error("disabled scheduler generated wiki")
			}
		}
		return nil
	})
}

func TestPutDuringGenerationDoesNotBlockAndRemainsDirty(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "one.md", "Initial fact")
	job := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, job, "First revision")
	job = storedJob(t, f, job.ID)
	ma.SetStatus(job.Managed.SessionID, "idle", "end_turn", 123)
	b := &pauseOutputRead{Store: f.s.Blobs, key: job.Managed.OutputKey, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.Blobs = b
	done := make(chan error, 1)
	go func() { done <- f.s.finalizeAgentJob(context.Background(), job) }()
	<-b.entered
	started := time.Now()
	putFile(t, f, token, p.ID, "two.md", "Arrived during generation")
	if time.Since(started) > time.Second {
		t.Error("put waited for generation")
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		rev := st.Revisions[p.Revision]
		if p.SourceVersion != 2 || p.WikiSourceVersion != 1 || rev.SourceVersion != 1 || len(rev.SourceIDs) != 1 || !p.SourcesUpdatedAt.After(rev.SourcesUpdatedAt) {
			t.Error("new input coverage lost", p, rev)
		}
		return nil
	})
}

func TestURLQueueIsIndependentAndOlderFetchCannotReplaceNewer(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.ID
	entered, release := make(chan struct{}), make(chan struct{})
	n := 0
	f.s.Fetcher.Public = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		n++
		body := "new content"
		if n == 1 {
			close(entered)
			select {
			case <-release:
				body = "old content"
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	queue := func() model.Job {
		var j model.Job
		if c := f.request("POST", base+"/put", token, model.PutRequest{URL: "https://example.com/source.txt"}, &j); c != 202 || j.Status != "queued" {
			t.Fatal(c, j)
		}
		return j
	}
	first := queue()
	// Simulate durable worker claims without a timer race in this ordering test.
	claim := func(j model.Job) {
		t.Helper()
		if err := f.db.Update(func(st *model.State) error { j.Status = "running"; st.Jobs[j.ID] = j; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	claim(first)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.processPut(ctx, first) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fetch did not start")
	}
	second := queue()
	claim(second)
	if err := f.s.processPut(ctx, second); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		head := st.Sources[p.SourceHeads[second.InputURL]]
		if p.SourceVersion != 1 || !p.SourcesUpdatedAt.Equal(head.UpdatedAt) {
			t.Error("late old fetch marked active sources changed", p)
		}
		b, err := f.s.Blobs.Get(context.Background(), head.TextKey)
		if err != nil || string(b) != "new content" {
			t.Error("late old fetch replaced new observation", string(b), err)
		}
		for _, j := range st.Jobs {
			if j.Kind != "put" {
				t.Error("URL put queued generation")
			}
		}
		return nil
	})
}

func TestExplicitGenerateWaitsForAcceptedURLPut(t *testing.T) {
	f := setup(t)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	// setup starts workers immediately; inject HTTP transport before submitting.
	entered, release := make(chan struct{}), make(chan struct{})
	f.s.Fetcher.Public = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader("Queued URL evidence"))}, nil
	})}
	var intake, gen model.Job
	base := "/api/projects/" + p.ID
	f.request("POST", base+"/put", token, model.PutRequest{URL: "https://example.com/source.txt"}, &intake)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("URL worker did not start")
	}
	if c := f.request("POST", base+"/generate", token, nil, &gen); c != 202 {
		t.Fatal(c)
	}
	if len(gen.Dependencies) != 1 || gen.Dependencies[0] != intake.ID {
		t.Fatal("missing intake dependency", gen)
	}
	_ = f.db.View(func(st model.State) error {
		if len(st.Revisions) != 0 {
			t.Error("published before intake")
		}
		return nil
	})
	close(release)
	f.wait(p.ID, token, gen.ID)
	var snapshot model.Snapshot
	f.request("GET", base+"/snapshot", token, nil, &snapshot)
	found := false
	for _, body := range snapshot.Files {
		if strings.Contains(body, "Queued URL evidence") {
			found = true
		}
	}
	if !found {
		t.Fatal("generate omitted accepted URL")
	}
}

func TestIntakeAndGenerationLimitsAreSeparate(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	f.s.Config.MaxQueued = 1
	f.s.Config.MaxIntakeJobs = 1
	base := "/api/projects/" + p.ID
	if c := f.request("POST", base+"/generate", token, nil, nil); c != 202 {
		t.Fatal(c)
	}
	if c := f.request("POST", base+"/generate", token, nil, nil); c != 429 {
		t.Fatal("generation limit missing", c)
	}
	putFile(t, f, token, p.ID, "one.md", "Not blocked by generation queue")
	input := model.PutRequest{URL: "https://example.com/source.txt"}
	if c := f.request("POST", base+"/put", token, input, nil); c != 202 {
		t.Fatal("URL blocked by generation queue", c)
	}
	if c := f.request("POST", base+"/put", token, input, nil); c != 429 {
		t.Fatal("intake limit missing", c)
	}
	putFile(t, f, token, p.ID, "two.md", "Not blocked by URL queue")
}

func TestFailedURLDependencyFailsGenerationWithoutPublication(t *testing.T) {
	f := setup(t)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	entered, release := make(chan struct{}), make(chan struct{})
	f.s.Fetcher.Public = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("missing"))}, nil
	})}
	base := "/api/projects/" + p.ID
	var intake, gen model.Job
	f.request("POST", base+"/put", token, model.PutRequest{URL: "https://example.com/missing"}, &intake)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fetch did not start")
	}
	f.request("POST", base+"/generate", token, nil, &gen)
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.request("GET", base+"/jobs/"+gen.ID, token, nil, &gen)
		if gen.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gen.Status != "failed" || !strings.Contains(gen.Error, "dependency failed") {
		t.Fatal("failed intake did not stop generation", gen)
	}
	_ = f.db.View(func(st model.State) error {
		if len(st.Revisions) != 0 {
			t.Error("published despite failed intake")
		}
		return nil
	})
}

func TestAgentPermissionsForPutAndGenerate(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	_, read := testAgent(t, f, human, p.ID, "read")
	_, write := testAgent(t, f, human, p.ID, "write")
	base := "/api/projects/" + p.ID
	for _, suffix := range []string{"/put", "/generate"} {
		if c := f.request("POST", base+suffix, read, nil, nil); c != 403 {
			t.Fatal("read agent could write", suffix, c)
		}
	}
	if c := f.request("POST", base+"/put", write, model.PutRequest{URL: "https://example.com/"}, nil); c != 403 {
		t.Fatal("agent could use provider URL access", c)
	}
	putFile(t, f, write, p.ID, "agent.md", "Authorized project file")
	var job model.Job
	if c := f.request("POST", base+"/generate", write, nil, &job); c != 202 || job.AgentID == "" {
		t.Fatal(c, job)
	}
	if _, err := f.s.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var other model.Project
	if c := f.request("POST", "/api/projects", human, map[string]string{"name": "other-project"}, &other); c != 201 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/projects/"+other.ID+"/generate", write, nil, nil); c != 404 {
		t.Fatal("agent escaped project", c)
	}
}

func testSourceURL(name, body string) string {
	return "https://example.com/" + auth.Hash(name+"\x00"+body)
}
