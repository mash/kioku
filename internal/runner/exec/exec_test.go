package execrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runner"
)

func TestExecHelper(t *testing.T) {
	args := os.Args
	i := slices.Index(args, "--")
	if i < 0 {
		return
	}
	role := args[i+1]
	if role == "graceful-stop" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		_ = os.WriteFile("ready", nil, 0600)
		<-signals
		time.Sleep(200 * time.Millisecond)
		if err := os.Remove(args[i+2]); err != nil {
			os.Exit(97)
		}
		fmt.Println(`{"version":1,"cost_cents":0}`)
		os.Exit(0)
	}
	if role == "invalid-failure" {
		os.Exit(7)
	}
	if role == "child" {
		for {
			time.Sleep(time.Second)
		}
	}
	if role == "no-stdin" || role == "ignore-term" {
		if role == "ignore-term" {
			signal.Ignore(syscall.SIGTERM)
		}
		_ = os.WriteFile("ready", nil, 0600)
		for {
			time.Sleep(time.Second)
		}
	}
	var in input
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		os.Exit(90)
	}
	switch role {
	case "success", "environment":
		req, _ := http.NewRequest("GET", in.InputURL, nil)
		req.Header.Set("Authorization", "Bearer "+in.JobToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			os.Exit(91)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "input" {
			os.Exit(92)
		}
		output := []byte("output")
		if role == "environment" {
			output, _ = json.Marshal(map[string]any{"argv": os.Args, "env": os.Environ(), "input": in})
		}
		req, _ = http.NewRequest("PUT", in.OutputURL, bytes.NewReader(output))
		req.Header.Set("Authorization", "Bearer "+in.JobToken)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			os.Exit(93)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(94)
		}
		fmt.Println(`{"version":1,"cost_cents":137,"reason":""}`)
	case "long-reason":
		_ = json.NewEncoder(os.Stdout).Encode(result{Version: 1, Reason: strings.Repeat("x", 1024), CostCents: new(int64)})
		os.Exit(7)
	case "invalid-cost":
		fmt.Println(`{"version":1,"reason":"invalid cost","cost_cents":null}`)
	case "success-reason":
		fmt.Println(`{"version":1,"reason":"ignored on success","cost_cents":0}`)
	case "failure":
		fmt.Fprint(os.Stderr, "stderr-private")
		fmt.Println(`{"version":1,"cost_cents":23,"reason":"reason-private"}`)
		os.Exit(7)
	case "invalid":
		fmt.Println(`{"version":2,"cost_cents":99}`)
	case "grandchild":
		cmd := exec.Command(os.Args[0], "-test.run=^TestExecHelper$", "--", "child")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(95)
		}
		_ = os.WriteFile("grandchild", []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
		fmt.Println(`{"version":1,"cost_cents":4}`)
	default:
		os.Exit(96)
	}
	os.Exit(0)
}
func newRunner(t *testing.T, role string, onExit func(string, string)) (*Runner, runner.RunSpec) {
	t.Helper()
	root := t.TempDir()
	r := New(Config{Root: root, Command: []string{os.Args[0], "-test.run=^TestExecHelper$", "--", role}, Model: "model", Prompt: "system"}, onExit)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return r, runner.RunSpec{Key: runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}, Token: runner.Secret("private-token"), FirstMessage: "first", BudgetCents: 1000, Deadline: time.Now().Add(time.Hour).UTC()}
}
func startExec(t *testing.T, r *Runner, spec runner.RunSpec) runner.RunRef {
	t.Helper()
	ref, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func awaitExit(t *testing.T, r *Runner, ref runner.RunRef) runner.Status {
	t.Helper()
	r.mu.Lock()
	c := r.children[ref.ID]
	r.mu.Unlock()
	select {
	case <-c.done:
	case <-time.After(20 * time.Second):
		t.Fatal("child did not exit")
	}
	status, err := r.Inspect(t.Context(), runner.Attempt{Key: c.key, Run: ref})
	if err != nil {
		t.Fatal(err)
	}
	return status
}
func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not become ready")
}
func TestExecSuccessAndPrivateInput(t *testing.T) {
	t.Setenv("KIOKU_ENCRYPTION_KEY", "ambient-private")
	uploaded := make(chan []byte, 1)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer private-token" {
			w.WriteHeader(403)
			return
		}
		if req.Method == "GET" {
			fmt.Fprint(w, "input")
			return
		}
		b, _ := io.ReadAll(req.Body)
		uploaded <- b
	}))
	defer host.Close()
	exited := make(chan runner.AttemptKey, 1)
	r, spec := newRunner(t, "environment", func(job, attempt string) { exited <- runner.AttemptKey{JobID: job, AttemptID: attempt} })
	spec.SourceCredentials = []runner.Credential{{Name: "GOOGLE_ACCESS_TOKEN", Value: "provider-secret", AllowedHosts: []string{"docs.googleapis.com"}}}
	spec.InputURL, spec.OutputURL = host.URL+"/input", host.URL+"/output"
	ref := startExec(t, r, spec)
	status := awaitExit(t, r, ref)
	if status.State != runner.StateStopped || status.Cost == nil || *status.Cost != 137 {
		t.Fatal(status)
	}
	select {
	case key := <-exited:
		if key.JobID != "job" || key.AttemptID != "attempt" {
			t.Fatal(key)
		}
	case <-time.After(time.Second):
		t.Fatal("no onExit")
	}
	var report struct {
		Argv, Env []string
		Input     input
	}
	wire := <-uploaded
	if bytes.Contains(wire, []byte("provider-secret")) || bytes.Contains(wire, []byte("GOOGLE_ACCESS_TOKEN")) {
		t.Fatal("provider credential in exec input")
	}
	if err := json.Unmarshal(wire, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Env) != 3 || report.Input.JobToken != string(spec.Token) || report.Input.Version != 1 || report.Input.Model != "model" || report.Input.Prompt != "system" || report.Input.BudgetCents != 1000 || !report.Input.Deadline.Equal(spec.Deadline) {
		t.Fatal("wire contract mismatch")
	}
	for _, value := range append(report.Env, report.Argv...) {
		if strings.Contains(value, "private-token") || strings.Contains(value, "ambient-private") {
			t.Fatal("secret in process arguments or environment")
		}
	}
	dir := filepath.Join(r.config.Root, spec.Key.JobID+"-"+spec.Key.AttemptID)
	for _, path := range []string{dir, filepath.Join(dir, "tmp")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal(path, err)
		}
	}
	for _, value := range []any{ref, spec} {
		b, err := json.Marshal(value)
		if err != nil || strings.Contains(string(b), string(spec.Token)) || strings.Contains(string(b), r.config.Root) || strings.Contains(string(b), os.Args[0]) {
			t.Fatal("private data in persisted or public JSON")
		}
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	status, _ = r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
	if status.State != runner.StateStopped || status.Cost != nil {
		t.Fatal(status)
	}
}
func TestExecExitStatusAndLogs(t *testing.T) {
	for _, tc := range []struct {
		role   string
		state  runner.State
		known  bool
		cost   int64
		reason string
	}{{"failure", runner.StateStopped, true, 23, "reason-private"}, {"invalid", runner.StateStopped, false, 0, "agent failed"}, {"long-reason", runner.StateStopped, true, 0, strings.Repeat("x", 512)}, {"invalid-cost", runner.StateStopped, false, 0, "invalid cost"}, {"success-reason", runner.StateStopped, true, 0, ""}, {"invalid-failure", runner.StateStopped, false, 0, "agent failed"}} {
		t.Run(tc.role, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(old)
			r, spec := newRunner(t, tc.role, nil)
			ref := startExec(t, r, spec)
			status := awaitExit(t, r, ref)
			if status.Reason != tc.reason || status.State != tc.state || (status.Cost != nil) != tc.known || (status.Cost != nil && *status.Cost != tc.cost) {
				t.Fatal(status)
			}
			if strings.Contains(logs.String(), "stderr-private") || strings.Contains(logs.String(), "reason-private") {
				t.Fatal(logs.String())
			}
			if !tc.known && !strings.Contains(logs.String(), "invalid result line") {
				t.Fatal(logs.String())
			}
		})
	}
}
func TestExecStartDoesNotWaitForStdinOrUseCallerContext(t *testing.T) {
	r, spec := newRunner(t, "no-stdin", nil)
	r.config.Prompt = strings.Repeat("x", 2<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := time.Now()
	ref, err := r.Start(ctx, spec)
	if err != nil || time.Since(before) > time.Second {
		t.Fatal(err, "Start blocked")
	}
	awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
	status, _ := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
	if status.State != runner.StateRunning || status.Cost != nil {
		t.Fatal(status)
	}
	before = time.Now()
	if err := r.Stop(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil || time.Since(before) > time.Second {
		t.Fatal(err, "Stop blocked")
	}
	if status := awaitExit(t, r, ref); status.State != runner.StateStopped || status.Reason != "terminated" {
		t.Fatal(status)
	}
}
func TestExecStopEscalatesOnce(t *testing.T) {
	r, spec := newRunner(t, "ignore-term", nil)
	ref := startExec(t, r, spec)
	awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
	kills := make(chan time.Time, 4)
	r.mu.Lock()
	c := r.children[spec.Key.AttemptID]
	r.signal = func(pgid int, sig syscall.Signal) error {
		if pgid != c.pgid {
			t.Errorf("signal group %d, want %d", pgid, c.pgid)
		}
		if sig == syscall.SIGKILL {
			kills <- time.Now()
			// Keep the child alive so duplicate timers cannot hide behind c.exited.
			return nil
		}
		return signalGroup(pgid, sig)
	}
	r.mu.Unlock()
	t.Cleanup(func() {
		r.mu.Lock()
		r.signal = signalGroup
		_ = signalGroup(c.pgid, syscall.SIGKILL)
		r.mu.Unlock()
	})
	before := time.Now()
	for range 3 {
		if err := r.Stop(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(before) > time.Second {
		t.Fatal("Stop waited")
	}
	select {
	case sent := <-kills:
		if sent.Sub(before) < 9*time.Second {
			t.Fatal("SIGKILL sent before grace period", sent.Sub(before))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("missing delayed SIGKILL")
	}
	// All three Stop calls completed before the grace period began to elapse.
	// Allow duplicate timers to run while the child is still registered and alive.
	select {
	case <-kills:
		t.Fatal("more than one delayed SIGKILL")
	case <-time.After(time.Second):
	}
	r.mu.Lock()
	r.signal = signalGroup
	err := signalGroup(c.pgid, syscall.SIGKILL)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.State != runner.StateStopped || status.Reason != "terminated" {
		t.Fatal(status)
	}
}
func TestExecCleanupKillsGrandchildren(t *testing.T) {
	r, spec := newRunner(t, "grandchild", nil)
	ref := startExec(t, r, spec)
	status := awaitExit(t, r, ref)
	if status.State != runner.StateStopped {
		t.Fatal(status)
	}
	raw, err := os.ReadFile(filepath.Join(r.config.Root, "job-attempt", "grandchild"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("grandchild exited before cleanup", err)
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("grandchild survived cleanup")
}
func TestExecCleanupBoundaries(t *testing.T) {
	r, spec := newRunner(t, "no-stdin", nil)
	// An arbitrary ID must not become a process handle.
	if err := r.Stop(t.Context(), runner.Attempt{Key: runner.AttemptKey{AttemptID: strconv.Itoa(os.Getpid())}}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []runner.AttemptKey{{JobID: "../outside", AttemptID: "attempt"}, {JobID: "job", AttemptID: "../outside"}, {JobID: "", AttemptID: "attempt"}} {
		if _, err := r.Start(t.Context(), runner.RunSpec{Key: key}); err == nil {
			t.Fatal("invalid key accepted", key)
		}
		if err := r.Cleanup(t.Context(), runner.Attempt{Key: key}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(r.config.Root, "wrong-name"), filepath.Join(t.TempDir(), "job-attempt")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		c := &child{key: spec.Key, cmd: &exec.Cmd{Dir: path}, pgid: 1 << 30, exited: true, done: make(chan struct{})}
		close(c.done)
		r.children[spec.Key.AttemptID] = c
		if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removed unrelated directory", err)
		}
	}
	ref := startExec(t, r, spec)
	awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
	wrong := spec.Key
	wrong.ProjectID = "other"
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: wrong}); err == nil {
		t.Fatal("mismatched child cleaned")
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key}); err != nil {
		t.Fatal(err)
	}
	status, _ := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
	if status.State != runner.StateStopped || status.Reason != "exec run lost on restart" {
		t.Fatal(status)
	}
}
func TestExecReconcileOnlyAndCleanupTimeout(t *testing.T) {
	r := New(Config{}, nil)
	defer r.Close(t.Context())
	if _, err := r.Start(t.Context(), runner.RunSpec{}); err == nil {
		t.Fatal("reconcile-only start accepted")
	}
	key := runner.AttemptKey{JobID: "job", AttemptID: "attempt"}
	ref := runner.RunRef{ID: "attempt"}
	status, _ := r.Inspect(t.Context(), runner.Attempt{Key: key, Run: ref})
	if status.Run.ID != ref.ID || status.State != runner.StateStopped || status.Cost != nil {
		t.Fatal(status)
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{}); err != nil {
		t.Fatal(err)
	}
	c := &child{key: key, pgid: 1 << 30, done: make(chan struct{})}
	r.children[key.AttemptID] = c
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Cleanup(ctx, runner.Attempt{Key: key, Run: ref}); !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	if r.children[key.AttemptID] != c {
		t.Fatal("timed out cleanup forgot child")
	}
	c.exited = true
	close(c.done)
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: key, Run: ref}); err != nil {
		t.Fatal(err)
	}
}
func TestExecCloseWaitsForCallbacksAndRejectsStart(t *testing.T) {
	entered, returned := make(chan struct{}), make(chan struct{})
	var r *Runner
	r, spec := newRunner(t, "invalid", func(string, string) { close(entered); <-r.Context().Done(); close(returned) })
	_ = startExec(t, r, spec)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback not started")
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Close left callback running")
	}
	spec.Key.AttemptID = "after-close"
	if _, err := r.Start(t.Context(), spec); err == nil {
		t.Fatal("Start after Close")
	}
}
func TestExecCloseAndCleanupRace(t *testing.T) {
	for _, role := range []string{"no-stdin", "invalid"} {
		t.Run(role, func(t *testing.T) {
			r, spec := newRunner(t, role, nil)
			ref := startExec(t, r, spec)
			if role == "invalid" {
				awaitExit(t, r, ref)
			} else {
				awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
			}
			r.mu.Lock()
			c := r.children[spec.Key.AttemptID]
			finalKills := 0
			r.signal = func(pgid int, sig syscall.Signal) error {
				if pgid != c.pgid {
					t.Errorf("signal group %d, want %d", pgid, c.pgid)
				}
				// Signal calls hold r.mu; count the final group cleanup after Wait.
				if sig == syscall.SIGKILL && c.exited {
					finalKills++
				}
				return signalGroup(pgid, sig)
			}
			r.mu.Unlock()
			start := make(chan struct{})
			done := make(chan error, 2)
			go func() { <-start; done <- r.Close(t.Context()) }()
			go func() { <-start; done <- r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}) }()
			close(start)
			for range 2 {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if finalKills != 1 || len(r.children) != 0 {
				t.Fatalf("final group SIGKILLs=%d, remaining children=%d", finalKills, len(r.children))
			}
		})
	}
}

func TestExecCloseReclaimsExitedChildGroup(t *testing.T) {
	r, spec := newRunner(t, "grandchild", nil)
	ref := startExec(t, r, spec)
	_ = awaitExit(t, r, ref)
	raw, err := os.ReadFile(filepath.Join(r.config.Root, "job-attempt", "grandchild"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Close left a descendant running")
}

func TestExecFailedStartCleansByKey(t *testing.T) {
	r, spec := newRunner(t, "success", nil)
	r.config.Command = []string{filepath.Join(r.config.Root, "missing")}
	ref, err := r.Start(t.Context(), spec)
	if err == nil || ref.ID != "" || strings.Contains(err.Error(), r.config.Root) {
		t.Fatal(ref, err)
	}
	if status, _ := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); status.State != runner.StateStopped {
		t.Fatal(status)
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(r.config.Root)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestExecCleanupReadOnlyDirectories(t *testing.T) {
	r, spec := newRunner(t, "invalid", nil)
	workdir := filepath.Join(r.config.Root, "job-attempt")
	cache := filepath.Join(workdir, "cache", "module")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "source.go"), nil, 0400); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(cache, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "source.go"), nil, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chmod(outside, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	if err := os.Symlink(outside, filepath.Join(workdir, "outside")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cache, filepath.Dir(cache), workdir} {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workdir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0555 {
		t.Fatal(info, err)
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key}); err != nil {
		t.Fatal(err)
	}
}

func TestExecCleanupLogsFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-host-path")
	if err := os.WriteFile(root, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Root: root}, nil)
	defer r.Close(t.Context())
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	key := runner.AttemptKey{JobID: "job", AttemptID: "attempt"}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: key}); !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	got := logs.String()
	for _, want := range []string{"level=WARN", "exec cleanup failed", "job=job", "attempt=attempt"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, root) || strings.Contains(got, "private-host-path") {
		t.Fatal(got)
	}
}

func TestExecCloseWithinDeadline(t *testing.T) {
	r, spec := newRunner(t, "ignore-term", nil)
	startExec(t, r, spec)
	awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if len(r.children) != 0 {
		t.Fatal("Close left child entries")
	}
	if r.Context().Err() == nil {
		t.Fatal("Close did not cancel callbacks")
	}
}

func TestExecStopAllowsExternalCleanup(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	r, spec := newRunner(t, "graceful-stop", nil)
	external := filepath.Join(t.TempDir(), "external-resource")
	if err := os.WriteFile(external, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r.config.Command = append(r.config.Command, external)
	ref := startExec(t, r, spec)
	awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
	if err := r.Stop(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.State != runner.StateStopped || status.Reason != "" {
		t.Fatal(status)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	if err := r.Cleanup(ctx, runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(external); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("child did not finish external cleanup", err)
	}
	if strings.Contains(logs.String(), "invalid result line") {
		t.Fatal(logs.String())
	}
}

func TestExecInvalidResultLogLevels(t *testing.T) {
	for _, role := range []string{"invalid", "invalid-failure", "no-stdin"} {
		t.Run(role, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(old)
			r, spec := newRunner(t, role, nil)
			ref := startExec(t, r, spec)
			if role == "no-stdin" {
				awaitFile(t, filepath.Join(r.config.Root, "job-attempt", "ready"))
				if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
					t.Fatal(err)
				}
			} else {
				awaitExit(t, r, ref)
			}
			level := "DEBUG"
			if role == "invalid" {
				level = "WARN"
			}
			if !strings.Contains(logs.String(), "level="+level+" msg=\"invalid result line\"") {
				t.Fatal(logs.String())
			}
		})
	}
}

func TestExecStartLogsUnderlyingErrors(t *testing.T) {
	for _, failure := range []string{"workdir", "command"} {
		t.Run(failure, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			r, spec := newRunner(t, "invalid", nil)
			want := "exec runner could not start"
			causePath := filepath.Join(r.config.Root, "missing-command")
			r.config.Command = []string{causePath}
			if failure == "workdir" {
				causePath = filepath.Join(r.config.Root, "blocked-root")
				if err := os.WriteFile(causePath, nil, 0600); err != nil {
					t.Fatal(err)
				}
				r.config.Root = causePath
				want = "exec runner workdir unavailable"
			}
			_, err := r.Start(t.Context(), spec)
			if err == nil || err.Error() != want {
				t.Fatal(err)
			}
			for _, text := range []string{"level=WARN", "job=job", "attempt=attempt", "error=", causePath} {
				if !strings.Contains(logs.String(), text) {
					t.Fatalf("missing %q in %s", text, logs.String())
				}
			}
		})
	}
}
