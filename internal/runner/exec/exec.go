// Package execrunner runs a trusted external runner using the JSON v1 contract.
package execrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/runner"
)

type Config struct {
	Root    string
	Command []string
	Model   string
	Prompt  string
}

type Runner struct {
	config    Config
	onExit    func(string, string)
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closing   bool
	children  map[string]*child
	callbacks sync.WaitGroup
	signal    func(int, syscall.Signal) error
}
type child struct {
	key       runner.AttemptKey
	cmd       *exec.Cmd
	pgid      int
	done      chan struct{}
	stdinDone chan struct{}
	exited    bool
	status    runner.Status
	killOnce  sync.Once
	stopped   bool
	stdout    tailBuffer
	stderr    byteCounter
}

func New(config Config, onExit func(jobID, attemptID string)) *Runner {
	config.Command = slices.Clone(config.Command)
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{config: config, onExit: onExit, ctx: ctx, cancel: cancel, children: map[string]*child{}, signal: signalGroup}
}
func (r *Runner) Context() context.Context { return r.ctx }
func (r *Runner) Start(_ context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	if r.config.Root == "" || len(r.config.Command) == 0 {
		return runner.RunRef{}, errors.New("exec runner is not configured")
	}
	name := spec.Key.JobID + "-" + spec.Key.AttemptID
	if !validName(name) || spec.Key.JobID == "" || spec.Key.AttemptID == "" {
		return runner.RunRef{}, errors.New("invalid exec attempt")
	}
	ref := runner.RunRef{ID: spec.Key.AttemptID}
	dir := filepath.Join(r.config.Root, name)
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0700); err != nil {
		slog.Warn("exec runner workdir unavailable", "job", spec.Key.JobID, "attempt", spec.Key.AttemptID, "error", err)
		return runner.RunRef{}, errors.New("exec runner workdir unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return runner.RunRef{}, errors.New("exec runner is closing")
	}
	if _, ok := r.children[ref.ID]; ok {
		return runner.RunRef{}, errors.New("exec attempt already exists")
	}
	cmd := exec.Command(r.config.Command[0], r.config.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + filepath.Join(dir, "tmp")}
	cmd.SysProcAttr = processAttributes()
	cmd.WaitDelay = 5 * time.Second
	c := &child{key: spec.Key, cmd: cmd, done: make(chan struct{}), stdinDone: make(chan struct{}), status: runner.Status{Run: ref, State: runner.StateRunning}}
	cmd.Stdout, cmd.Stderr = &c.stdout, &c.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Warn("exec runner input unavailable", "job", spec.Key.JobID, "attempt", spec.Key.AttemptID, "error", err)
		return runner.RunRef{}, errors.New("exec runner input unavailable")
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		slog.Warn("exec runner could not start", "job", spec.Key.JobID, "attempt", spec.Key.AttemptID, "error", err)
		return runner.RunRef{}, errors.New("exec runner could not start")
	}
	c.pgid = cmd.Process.Pid
	r.children[ref.ID] = c
	// The private wire value is written only to stdin, never formatted or logged.
	wire := input{Version: 1, JobID: spec.Key.JobID, AttemptID: spec.Key.AttemptID, ProjectID: spec.Key.ProjectID, InputURL: spec.InputURL, OutputURL: spec.OutputURL, JobToken: string(spec.Token), Model: r.config.Model, FirstMessage: spec.FirstMessage, Prompt: strings.TrimSpace(r.config.Prompt), BudgetCents: spec.BudgetCents, Deadline: spec.Deadline}
	go func() {
		defer close(c.stdinDone)
		defer stdin.Close()
		data, _ := json.Marshal(wire)
		data = append(data, '\n')
		if n, err := stdin.Write(data); err != nil {
			slog.Warn("exec input write failed", "job", spec.Key.JobID, "bytes", n)
		}
	}()
	go r.wait(c)
	return ref, nil
}
func (r *Runner) wait(c *child) {
	_ = c.cmd.Wait()
	<-c.stdinDone
	status := runner.Status{Run: runner.RunRef{ID: c.key.AttemptID}, State: runner.StateStopped}
	result, valid := parseResult(c.stdout.line())
	successful, signaled := false, false
	if c.cmd.ProcessState != nil {
		successful = c.cmd.ProcessState.ExitCode() == 0
		if state, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			signaled = state.Signaled()
		}
	}
	if valid {
		status.Cost = result.CostCents
	} else if successful {
		slog.Warn("invalid result line", "job", c.key.JobID)
	} else {
		slog.Debug("invalid result line", "job", c.key.JobID)
	}
	if !successful || !valid {
		status.Reason = result.Reason[:min(len(result.Reason), 512)]
		if status.Reason == "" {
			status.Reason = "agent failed"
		}
	}
	r.mu.Lock()
	if c.stopped && signaled {
		status.Reason = "terminated"
	}
	r.mu.Unlock()
	slog.Debug("exec runner stderr", "job", c.key.JobID, "bytes", c.stderr.total)
	r.mu.Lock()
	c.status, c.exited = status, true
	notify := !r.closing && r.onExit != nil
	if notify {
		r.callbacks.Add(1)
	}
	close(c.done)
	r.mu.Unlock()
	if notify {
		defer r.callbacks.Done()
		r.onExit(c.key.JobID, c.key.AttemptID)
	}
}
func (r *Runner) Inspect(_ context.Context, a runner.Attempt) (runner.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.children[a.Key.AttemptID]; c != nil {
		if c.key != a.Key {
			return runner.Status{}, errors.New("exec inspection attempt changed")
		}
		return c.status, nil
	}
	return runner.Status{Run: runner.RunRef{ID: a.Key.AttemptID}, State: runner.StateStopped, Reason: "exec run lost on restart"}, nil
}
func (r *Runner) Stop(_ context.Context, a runner.Attempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.children[a.Key.AttemptID]
	if c != nil && c.key != a.Key {
		return errors.New("exec stop attempt changed")
	}
	if c == nil || c.exited {
		return nil
	}
	err := r.signal(c.pgid, syscall.SIGTERM)
	if err != nil {
		return err
	}
	c.stopped = true
	c.killOnce.Do(func() {
		deadline := time.Now().Add(10 * time.Second)
		go func() {
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-c.done:
				return
			case <-r.ctx.Done():
				return
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.children[a.Key.AttemptID] == c && !c.exited {
				_ = r.signal(c.pgid, syscall.SIGKILL)
			}
		}()
	})
	return nil
}
func (r *Runner) Cleanup(ctx context.Context, a runner.Attempt) (cleanupErr error) {
	key := a.Key
	path := filepath.Join(r.config.Root, key.JobID+"-"+key.AttemptID)
	defer func() {
		if cleanupErr != nil {
			slog.Warn("exec cleanup failed", "job", key.JobID, "attempt", key.AttemptID, "error", cleanupErr)
		}
	}()
	r.mu.Lock()
	c := r.children[key.AttemptID]
	if c != nil && c.key != key {
		r.mu.Unlock()
		return errors.New("exec cleanup attempt changed")
	}
	if c != nil && c.cmd != nil {
		path = c.cmd.Dir
	}
	if c != nil && !c.exited {
		c.stopped = true
		err := r.signal(c.pgid, syscall.SIGKILL)
		r.mu.Unlock()
		if err != nil {
			return fmt.Errorf("%w: exec stop failed", runner.ErrTemporary)
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			return fmt.Errorf("%w: exec cleanup timed out", runner.ErrTemporary)
		}
		r.mu.Lock()
	}
	if c != nil && r.children[key.AttemptID] == c {
		if err := r.signal(c.pgid, syscall.SIGKILL); err != nil {
			r.mu.Unlock()
			return fmt.Errorf("%w: exec group cleanup failed", runner.ErrTemporary)
		}
		delete(r.children, key.AttemptID)
	}
	r.mu.Unlock()
	if r.config.Root == "" {
		return nil
	}
	name := filepath.Base(path)
	expected := key.JobID + "-" + key.AttemptID
	path = filepath.Clean(path)
	rel, err := filepath.Rel(r.config.Root, path)
	if key.JobID == "" || key.AttemptID == "" || !validName(expected) || !validName(name) || name != expected || err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		slog.Warn("invalid exec workdir", "job", key.JobID)
		return nil
	}
	if err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && name == path {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(name, info.Mode()|0700)
	}); err != nil {
		return fmt.Errorf("%w: exec workdir cleanup failed", runner.ErrTemporary)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("%w: exec workdir cleanup failed", runner.ErrTemporary)
	}
	return nil
}
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	r.cancel()
	children := make([]*child, 0, len(r.children))
	for _, c := range r.children {
		children = append(children, c)
		if !c.exited {
			c.stopped = true
			_ = r.signal(c.pgid, syscall.SIGTERM)
		}
	}
	r.mu.Unlock()
	gracePeriod := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		gracePeriod = max(0, min(gracePeriod, time.Until(deadline)-6*time.Second))
	}
	grace, cancel := context.WithTimeout(ctx, gracePeriod)
	for _, c := range children {
		select {
		case <-c.done:
		case <-grace.Done():
		}
	}
	cancel()
	r.mu.Lock()
	for _, c := range children {
		if r.children[c.key.AttemptID] == c && !c.exited {
			_ = r.signal(c.pgid, syscall.SIGKILL)
		}
	}
	r.mu.Unlock()
	var cleanupErr error
	for _, c := range children {
		select {
		case <-c.done:
		case <-ctx.Done():
			cleanupErr = ctx.Err()
		}
	}
	r.mu.Lock()
	for _, c := range children {
		if r.children[c.key.AttemptID] == c {
			cleanupErr = errors.Join(cleanupErr, r.signal(c.pgid, syscall.SIGKILL))
			delete(r.children, c.key.AttemptID)
		}
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.callbacks.Wait(); close(done) }()
	select {
	case <-done:
		return cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, "\\\x00")
}
func signalGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
