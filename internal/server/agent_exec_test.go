package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func TestExecRunnerHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	role := os.Args[i+1]
	var in struct {
		InputURL     string `json:"input_url"`
		OutputURL    string `json:"output_url"`
		Token        string `json:"job_token"`
		Prompt       string `json:"prompt"`
		Model        string `json:"model"`
		FirstMessage string `json:"first_message"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		os.Exit(90)
	}
	view := in.Prompt == viewRunnerSystem
	switch {
	case view:
	case role == "custom":
		if in.Prompt != "Custom system\n\n"+prompts.WikiInstructionsParagraph || in.Model != "custom-model" || in.FirstMessage != "Fetch "+in.InputURL+" then upload "+in.OutputURL {
			os.Exit(97)
		}
	default:
		rendered, err := prompts.Load("", prompts.Variables{ClientVersion: MinimumClientVersion, MaxOutputBytes: 128 << 20}, MCPToolNames())
		if err != nil || in.Prompt != rendered.System() {
			os.Exit(96)
		}
	}
	if role == "success" && !view {
		checkExecJobScope(t, in.InputURL, in.Token)
	}
	if role == "wait" {
		for {
			time.Sleep(time.Second)
		}
	}
	req, _ := http.NewRequest("GET", in.InputURL, nil)
	req.Header.Set("Authorization", "Bearer "+in.Token)
	req.Header.Set("Kioku-Client-Version", "dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		os.Exit(91)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		os.Exit(92)
	}
	if len(readInput(t, data)) == 0 {
		os.Exit(93)
	}
	if role == "success" || role == "upload-failure" || role == "custom" || role == "view-failure" {
		pages := map[string]string{"wiki/index.md": "# Exec wiki\n"}
		if view {
			pages = map[string]string{"index.html": "<h1>Project map</h1>"}
		}
		output := outputArchive(t, pages)
		req, _ = http.NewRequest("PUT", in.OutputURL, bytes.NewReader(output))
		req.Header.Set("Authorization", "Bearer "+in.Token)
		req.Header.Set("Kioku-Client-Version", "dev")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			os.Exit(94)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(95)
		}
	}
	if role == "failure" || role == "upload-failure" && !view || role == "view-failure" && view {
		fmt.Fprint(os.Stderr, "stderr-private")
		fmt.Println(`{"version":1,"cost_cents":23,"reason":"reason-private"}`)
		os.Exit(7)
	}
	fmt.Println(`{"version":1,"cost_cents":137}`)
	os.Exit(0)
}
func execFixture(t *testing.T, role string) (*fixture, string, fixtureProject) {
	t.Helper()
	f := setupWithWorker(t, false)
	f.runnerConfig.Type = "exec"
	f.runnerConfig.Command = []string{os.Args[0], "-test.run=^TestExecRunnerHelper$", "--", role}
	f.runnerConfig.DataDir = t.TempDir()
	f.runnerConfig.BaseURL = f.s.Config.BaseURL
	f.setRunners()
	token := f.login("exec@example.com")
	createTestOrg(t, f, token, "acme")
	var p fixtureProject
	if c := f.request("POST", "/api/projects", token, map[string]string{"name": "acme/exec"}, &p); c != 201 {
		t.Fatal(c)
	}
	return f, token, p
}
func waitExecJob(t *testing.T, f *fixture, j fixtureJob) fixtureJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j = storedJob(t, f, j.ID)
		if j.ViewGeneration != nil {
			f.driveViewGeneration(j)
		}
		if j.Status != "running" && j.Run.CleanupComplete {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("exec job did not finish", j)
	return j
}

func TestExecTimeout(t *testing.T) {
	t.Parallel()
	f, token, p := execFixture(t, "wait")
	j := startManaged(t, f, token, p.ID, true)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		current := fixtureJobsGet(t, st, j.ID)
		current.Run.Deadline = time.Now().Add(-time.Second)
		fixtureJobsPut(t, st, j.ID, current)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(j)); err != nil && !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	j = waitExecJob(t, f, j)
	if j.Status != "failed" || j.Error != "agent job timed out" || j.Run.CostCents != nil {
		t.Fatal(j)
	}
}

func TestReconcileJobStopsWithShutdownContext(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	entered := make(chan struct{}, 1)
	f.s.SetRunner(&testRunner{
		inspect: func(ctx context.Context, _ runner.Attempt) (runner.Status, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return runner.Status{}, ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.s.ReconcileJob(ctx, j.ID)
	select {
	case <-entered:
		t.Fatal("shutdown started a new inspection")
	default:
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.s.ReconcileJob(ctx, j.ID) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("inspection did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel in-flight inspection")
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" || got.Run.AbortReason != "" {
		t.Fatalf("shutdown changed job: %+v", got)
	}
}

func TestExecPromptOverridesOnWire(t *testing.T) {
	t.Parallel()
	f, token, p := execFixture(t, "custom")
	overridePrompts(t, f, "---\nmodel: {id: custom-model, effort: high}\n---\nCustom system", "Fetch {{.InputURL}} then upload {{.OutputURL}}", "")
	j := startManaged(t, f, token, p.ID, true)
	j = waitExecJob(t, f, j)
	if j.Status != "completed" {
		t.Fatal(j)
	}
}
