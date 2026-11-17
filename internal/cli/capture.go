package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mash/kioku/internal/atomicfile"
	"github.com/mash/kioku/prompts"
)

type captureLock struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	BgID      string    `json:"bg_id,omitempty"`
}

var errCaptureBusy = errors.New("capture already running")
var captureBackgroundID = regexp.MustCompile(`backgrounded · ([0-9a-f]{8})\b`)

func readCaptureLock(path string) (*captureLock, error) {
	data, err := readRegular(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lock captureLock
	if err := decodeJSON(data, &lock); err != nil {
		return nil, err
	}
	if lock.StartedAt.IsZero() {
		return nil, errors.New("invalid capture lock")
	}
	return &lock, nil
}

func lockBusy(lock *captureLock, now time.Time) bool {
	return lock != nil && now.Sub(lock.StartedAt) <= 30*time.Minute
}

func acquireCaptureLock(path string, now time.Time) (old *captureLock, err error) {
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	// Serialize stale replacement so two contenders cannot delete each other's new lock.
	guard, err := openLockedFile(filepath.Dir(path), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	create := func() error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = json.NewEncoder(f).Encode(captureLock{PID: os.Getpid(), StartedAt: now})
		err = errors.Join(err, f.Close())
		if err != nil {
			_ = os.Remove(path)
		}
		return err
	}
	if err = create(); !os.IsExist(err) {
		return nil, err
	}
	old, err = readCaptureLock(path)
	if err != nil {
		return nil, err
	}
	if lockBusy(old, now) {
		return nil, errCaptureBusy
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return old, create()
}

func readQueue(path string) ([]queueRow, error) {
	data, err := readRegular(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseQueue(data)
}

func (a *App) capture(args []string, now time.Time) error {
	usage := errors.New("usage: kioku capture run [--min N] | unlock")
	if len(args) == 0 {
		return usage
	}
	if _, err := a.projectPath(); err != nil {
		return err
	}
	switch args[0] {
	case "run":
		f := flag.NewFlagSet("capture run", flag.ContinueOnError)
		f.SetOutput(a.Err)
		min := f.Int("min", 5, "minimum queued sources")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *min < 1 {
			return usage
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		return a.captureRun(*min, cwd, now)
	case "unlock":
		if len(args) != 1 {
			return usage
		}
		return a.captureUnlock()
	default:
		return usage
	}
}

func (a *App) captureRun(min int, cwd string, now time.Time) error {
	rows, err := readQueue(filepath.Join(a.metadataDir(), "queue.tsv"))
	if err != nil || len(rows) < min {
		return err
	}
	path := filepath.Join(a.metadataDir(), "capture.lock")
	old, err := acquireCaptureLock(path, now)
	if errors.Is(err, errCaptureBusy) || os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	launched := false
	defer func() {
		if !launched {
			_ = os.Remove(path)
		}
	}()
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("capture run: claude not found")
	}
	claude, err = filepath.Abs(claude)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "kioku-capture-")
	if err != nil {
		return err
	}
	defer func() {
		if !launched {
			_ = os.RemoveAll(tmp)
		}
	}()
	resolved, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return err
	}
	tmp = resolved
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	vars := templateVars(exe, a.Config.Server, a.Config.Namespace, a.Dir)
	vars["tmp"], vars["queue"] = tmp, string(formatQueue(rows))
	prompt := renderTemplate(prompts.CaptureRun, vars)
	cmd := exec.Command(claude, "--bg", "--settings", `{"disableAllHooks":true}`, "--permission-mode", "dontAsk", "--allowedTools", "Bash(gh:*)", "Bash(curl:*)", "Bash("+exe+":*)", "Read("+tmp+"/**)", "Write("+tmp+"/**)", "WebFetch", "--agent", "kioku:kioku-capture", prompt)
	cmd.Dir = cwd
	cmd.Env = processEnv("CLAUDE_CODE_SESSION_ID")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return fmt.Errorf("capture run: claude --bg failed: %s", reason)
	}
	launched = true
	id := ""
	if match := captureBackgroundID.FindStringSubmatch(stdout.String()); len(match) > 1 {
		id = match[1]
	}
	if id != "" {
		previous := make(map[string]bool)
		if old != nil && old.BgID != "" {
			previous[old.BgID] = true
		}
		last := filepath.Join(a.metadataDir(), "capture.last")
		if data, err := readRegular(last); err == nil {
			if previousID := strings.TrimSpace(string(data)); previousID != "" {
				previous[previousID] = true
			}
		}
		if err := a.recordCaptureID(path, now, id); err != nil {
			return err
		}
		for _, previousID := range sortedKeys(previous) {
			if previousID == id {
				continue
			}
			cleanup := exec.Command(claude, "rm", previousID)
			cleanup.Dir = cwd
			cleanup.Env = processEnv("CLAUDE_CODE_SESSION_ID")
			_ = cleanup.Run()
		}
		// recordCaptureID may have saved this id after an early unlock; preserve it.
		if data, err := readRegular(last); err == nil && strings.TrimSpace(string(data)) != id {
			_ = os.Remove(last)
		}
	}
	idText := ""
	if id != "" {
		idText = "、id " + id
	}
	_, err = fmt.Fprintf(a.Out, "kioku-capture を背景で起動した（%s、%d 件%s）。claude agents で見る。\n", a.Config.Namespace, len(rows), idText)
	return err
}

func (a *App) recordCaptureID(path string, now time.Time, id string) error {
	return updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		if data == nil {
			// captureUnlock can finish before claude --bg reports its id.
			return nil, false, atomicfile.Write(filepath.Join(a.metadataDir(), "capture.last"), []byte(id+"\n"), 0600)
		}
		var lock captureLock
		if err := decodeJSON(data, &lock); err != nil {
			return nil, false, err
		}
		if lock.PID != os.Getpid() || !lock.StartedAt.Equal(now) {
			return nil, false, nil
		}
		lock.BgID = id
		next, err := json.Marshal(lock)
		return append(next, '\n'), true, err
	})
}

func (a *App) captureUnlock() error {
	guard, err := openLockedFile(a.metadataDir(), os.O_RDONLY)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	path := filepath.Join(a.metadataDir(), "capture.lock")
	lock, err := readCaptureLock(path)
	if err != nil || lock == nil {
		return err
	}
	if lock.BgID != "" {
		if err := atomicfile.Write(filepath.Join(a.metadataDir(), "capture.last"), []byte(lock.BgID+"\n"), 0600); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
