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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runners"
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
	if role == "custom" {
		if in.Prompt != "Custom system" || in.Model != "custom-model" || in.FirstMessage != "Fetch "+in.InputURL+" then upload "+in.OutputURL {
			os.Exit(97)
		}
	} else {
		rendered, err := prompts.Load("", prompts.Variables{ClientVersion: MinimumClientVersion, MaxOutputBytes: 128 << 20}, MCPToolNames())
		if err != nil || in.Prompt != rendered.System() {
			os.Exit(96)
		}
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
	if role == "success" || role == "upload-failure" || role == "custom" {
		output := outputArchive(t, map[string]string{"wiki/index.md": "# Exec wiki\n"})
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
	if role == "failure" || role == "upload-failure" {
		fmt.Fprint(os.Stderr, "stderr-private")
		fmt.Println(`{"version":1,"cost_cents":23,"reason":"reason-private"}`)
		os.Exit(7)
	}
	fmt.Println(`{"version":1,"cost_cents":137}`)
	os.Exit(0)
}
func execFixture(t *testing.T, role string) (*fixture, string, model.Project) {
	t.Helper()
	f := setupWithWorker(t, false)
	f.runnerConfig.Type = "exec"
	f.runnerConfig.Command = []string{os.Args[0], "-test.run=^TestExecRunnerHelper$", "--", role}
	f.runnerConfig.DataDir = t.TempDir()
	f.runnerConfig.BaseURL = f.s.Config.BaseURL
	f.setRunners()
	token := f.login("exec@example.com")
	createTestOrg(t, f, token, "acme")
	var p model.Project
	if c := f.request("POST", "/api/projects", token, map[string]string{"name": "acme/exec"}, &p); c != 201 {
		t.Fatal(c)
	}
	return f, token, p
}
func waitExecJob(t *testing.T, f *fixture, j model.Job) model.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j = storedJob(t, f, j.ID)
		if j.Status != "running" && j.Run.CleanupComplete {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("exec job did not finish", j)
	return j
}
func TestExecGenerationLifecycle(t *testing.T) {
	for _, role := range []string{"success", "failure", "upload-failure"} {
		t.Run(role, func(t *testing.T) {
			f, token, p := execFixture(t, role)
			j := startManaged(t, f, token, p.ID, true)
			j = waitExecJob(t, f, j)
			want, cost := "failed", int64(23)
			if role == "success" {
				want, cost = "completed", 137
			} else if role == "upload-failure" {
				want = "completed"
			}
			if j.Status != want || j.Run.CostCents == nil || *j.Run.CostCents != cost {
				t.Fatal(j)
			}
			if role == "failure" && j.Error != "reason-private" {
				t.Fatal(j.Error)
			}
			if strings.Contains(j.Error, "stderr-private") {
				t.Fatal(j.Error)
			}
			if _, err := os.Stat(filepath.Join(f.runnerConfig.DataDir, "exec-runs", j.ID+"-"+j.Run.Attempt.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("workdir remains", err)
			}
		})
	}
}
func TestExecTimeoutAndStaleExit(t *testing.T) {
	f, token, p := execFixture(t, "wait")
	j := startManaged(t, f, token, p.ID, true)
	if err := f.db.Update(func(st *model.State) error { st.Jobs[j.ID].Run.Deadline = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
	f.s.ReconcileJob(t.Context(), j.ID, "older-attempt")
	if got := storedJob(t, f, j.ID); got.Status != "running" || got.Run.AbortReason != "" {
		t.Fatal("stale callback reconciled job", got)
	}
	if err := f.s.reconcileAgentJob(t.Context(), j); err != nil && !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	j = waitExecJob(t, f, j)
	if j.Status != "failed" || j.Error != "agent job timed out" || j.Run.CostCents != nil {
		t.Fatal(j)
	}
}
func TestWorkerReconcilesLostExecBeforeDequeuing(t *testing.T) {
	for _, phase := range []model.ManagedPhase{model.PhasePreparing, model.PhaseAgentRunning, model.PhaseOutputReceived, model.PhaseFinalizing} {
		for _, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/aborted=%v", phase, abort), func(t *testing.T) {
				f, token, p, ma := managedFixture(t)
				j := startManaged(t, f, token, p.ID, true)
				raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
				if phase == model.PhaseOutputReceived || phase == model.PhaseFinalizing {
					sendOutput(t, f, ma, j, "Restart output")
				}
				if err := f.db.Update(func(st *model.State) error {
					run := st.Jobs[j.ID].Run
					run.Phase = phase
					if phase == model.PhasePreparing {
						run.Ref.ID = ""
					}
					if abort {
						run.AbortReason = "original abort"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				f.runnerConfig.Type = "exec"
				f.runnerConfig.Command = []string{os.Args[0]}
				f.runnerConfig.DataDir = t.TempDir()
				restored := New(f.s.Config, f.db, f.s.Blobs, f.s.Cipher)
				g, err := runners.Load(f.runnerConfig, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer g.Close(context.Background())
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				// A second project can be dequeued even while the lost run occupies a slot.
				queued := model.Job{ID: "queued-after-restart", ProjectID: "other-project", Status: "queued"}
				if err := f.db.Update(func(st *model.State) error { st.Jobs[queued.ID] = queued; return nil }); err != nil {
					t.Fatal(err)
				}
				observed := make(chan model.Job, 1)
				restored.SetRunners(observingRunners{Runners: g, newRun: func(next model.Job) (runner.Runner, error) {
					if next.ID == queued.ID {
						var previous model.Job
						if err := f.db.View(func(st model.State) error { previous = st.Jobs[j.ID]; return nil }); err != nil {
							return nil, err
						}
						observed <- previous
						cancel()
						return nil, runner.ErrNotConfigured
					}
					return g.New(next)
				}})
				if err := restored.Prepare(); err != nil {
					t.Fatal(err)
				}
				if got := storedJob(t, f, j.ID); got.Status != "running" {
					t.Fatal("Prepare reconciled the run", got)
				}
				done := make(chan error, 1)
				go func() { done <- restored.RunWorker(ctx) }()
				select {
				case previous := <-observed:
					if previous.Status == "running" || !previous.Run.CleanupComplete {
						t.Error("worker dequeued before startup reconciliation", previous)
					}
				case <-time.After(5 * time.Second):
					cancel()
					t.Error("worker did not pick up the queued job")
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				lost := phase == model.PhasePreparing || phase == model.PhaseAgentRunning
				if code, _ := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, map[string]string{"wiki/index.md": "replacement"})); code != 403 {
					t.Fatal("old token accepted", code)
				}
				j = storedJob(t, f, j.ID)
				want := "completed"
				if lost || abort {
					want = "failed"
				}
				if j.Status != want || !j.Run.CleanupComplete || j.Run.CostCents != nil {
					t.Fatal(j)
				}
				if abort && j.Error != "original abort" {
					t.Fatal(j.Error)
				}
				if lost && !abort && j.Error != "exec run lost on restart" {
					t.Fatal(j.Error)
				}
			})
		}
	}
}

func TestReconcileJobStopsWithShutdownContext(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	entered := make(chan struct{}, 1)
	f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{
		inspect: func(ctx context.Context, _ runner.Attempt) (runner.Status, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return runner.Status{}, ctx.Err()
		},
	}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.s.ReconcileJob(ctx, j.ID, j.Run.Attempt.ID)
	select {
	case <-entered:
		t.Fatal("shutdown started a new inspection")
	default:
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.s.ReconcileJob(ctx, j.ID, j.Run.Attempt.ID) }()
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

type observingRunners struct {
	Runners
	newRun func(model.Job) (runner.Runner, error)
}

func (r observingRunners) New(job model.Job) (runner.Runner, error) { return r.newRun(job) }

func TestExecPromptOverridesOnWire(t *testing.T) {
	f, token, p := execFixture(t, "custom")
	overridePrompts(t, f, "---\nmodel: {id: custom-model, effort: high}\n---\nCustom system", "Fetch {{.InputURL}} then upload {{.OutputURL}}", "")
	j := startManaged(t, f, token, p.ID, true)
	j = waitExecJob(t, f, j)
	if j.Status != "completed" {
		t.Fatal(j)
	}
}
