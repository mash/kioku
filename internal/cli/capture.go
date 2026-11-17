package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
	Token     string    `json:"token"`
	invalid   bool
}

var errCaptureBusy = errors.New("capture already running")
var captureBackgroundID = regexp.MustCompile(`backgrounded · ([0-9a-f]{8})\b`)
var captureTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func readCaptureLock(path string) (*captureLock, error) {
	data, err := readRegular(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lock captureLock
	if decodeJSON(data, &lock) != nil || lock.StartedAt.IsZero() {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		return &captureLock{StartedAt: info.ModTime(), invalid: true}, nil
	}
	return &lock, nil
}
func lockBusy(lock *captureLock, now time.Time) bool {
	return lock != nil && now.Sub(lock.StartedAt) <= 30*time.Minute
}
func acquireCaptureLock(path string, now time.Time) (*captureLock, error) {
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	guard, err := openLockedFile(filepath.Dir(path), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	old, err := readCaptureLock(path)
	if err != nil {
		return nil, err
	}
	if lockBusy(old, now) {
		return old, errCaptureBusy
	}
	if old != nil {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	lock := &captureLock{PID: os.Getpid(), StartedAt: now, Token: hex.EncodeToString(random[:])}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	err = errors.Join(json.NewEncoder(f).Encode(lock), f.Close())
	if err != nil {
		_ = os.Remove(path)
	}
	return lock, err
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
	usage := errors.New("usage: kioku capture run [--min N] | unlock --token TOKEN")
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
		f := flag.NewFlagSet("capture unlock", flag.ContinueOnError)
		f.SetOutput(a.Err)
		token := f.String("token", "", "capture ownership token")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return usage
		}
		return a.captureUnlock(*token)
	default:
		return usage
	}
}

func (a *App) captureTmp(token string) string {
	return filepath.Join(a.metadataDir(), "capture-tmp", token)
}
func (a *App) cleanupCaptureTmp(now time.Time) error {
	guard, err := openLockedFile(a.metadataDir(), os.O_RDONLY)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	lock, err := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
	if err != nil {
		return err
	}
	live := ""
	if lockBusy(lock, now) && !lock.invalid {
		live = lock.Token
	}
	root := filepath.Join(a.metadataDir(), "capture-tmp")
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("capture-tmp must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != live {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) prepareCaptureTmp(token string) (string, error) {
	guard, err := openLockedFile(a.metadataDir(), os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer guard.Close()
	lock, err := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
	if err != nil {
		return "", err
	}
	if lock == nil || lock.Token != token {
		return "", errors.New("capture lock ownership changed")
	}
	root := filepath.Join(a.metadataDir(), "capture-tmp")
	if err := ensureDirectory(root); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() != token {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return "", err
			}
		}
	}
	tmp := a.captureTmp(token)
	if err := os.Mkdir(tmp, 0700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(tmp)
}

func captureAllowedTools(exe, server, ns, p, tmp string) []string {
	command := kiokuCommand(exe, server)
	project := command + " -C " + shellQuote(p) + " -n " + ns
	return []string{
		"Bash(" + command + " fetch:*)",
		"Bash(" + project + " put:*)", "Bash(" + project + " queue remove:*)", "Bash(" + project + " capture unlock:*)",
		"Bash(gh pr view:*)", "Bash(gh issue view:*)", "Bash(gh pr diff:*)",
		"Read(" + tmp + "/**)", "Write(" + tmp + "/**)",
		"mcp__claude_ai_Notion__notion-fetch",
		"mcp__claude_ai_Notion__notion-search",
		"mcp__claude_ai_Slack__slack_read_thread",
		"mcp__claude_ai_Slack__slack_read_channel",
		"mcp__claude_ai_Google_Drive__read_file_content",
		"mcp__claude_ai_Google_Drive__download_file_content",
	}
}
func (a *App) captureNote(note string) error {
	log, err := backgroundLog(filepath.Join(a.metadataDir(), "capture.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	_, err = fmt.Fprintln(log, strings.NewReplacer("\r", " ", "\n", " ").Replace(note))
	return err
}
func (a *App) captureRun(min int, cwd string, now time.Time) error {
	if err := a.cleanupCaptureTmp(now); err != nil {
		return err
	}
	rows, err := readQueue(filepath.Join(a.metadataDir(), "queue.tsv"))
	if err != nil {
		return err
	}
	if len(rows) < min {
		_, err = fmt.Fprintf(a.Out, "capture: queue below minimum (%d < %d)\n", len(rows), min)
		return err
	}
	path := filepath.Join(a.metadataDir(), "capture.lock")
	lock, err := acquireCaptureLock(path, now)
	if errors.Is(err, errCaptureBusy) {
		id := "id pending"
		if lock.BgID != "" {
			id = "id " + lock.BgID + "; claude logs " + lock.BgID
		}
		_, err = fmt.Fprintf(a.Out, "capture: already running (%s)\n", id)
		return err
	}
	if err != nil {
		return err
	}
	launched := false
	defer func() {
		if !launched {
			_ = a.captureUnlock(lock.Token)
			_ = os.Remove(a.captureReceipt(lock.Token))
		}
	}()
	tmp, err := a.prepareCaptureTmp(lock.Token)
	if err != nil {
		return err
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("capture run: claude not found")
	}
	claude, err = filepath.Abs(claude)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	vars := templateVars(exe, a.Config.Server, a.Config.Namespace, a.Dir)
	vars["tmp"], vars["queue"], vars["token"] = tmp, string(formatQueue(rows)), lock.Token
	vars["project_kioku"] = vars["kioku"] + " -C " + shellQuote(a.Dir)
	prompt := renderTemplate(prompts.CaptureRun, vars)
	args := []string{"--bg", "--settings", `{"disableAllHooks":true}`, "--permission-mode", "dontAsk", "--allowedTools"}
	args = append(args, captureAllowedTools(exe, a.Config.Server, a.Config.Namespace, a.Dir, tmp)...)
	args = append(args, "--agent", "kioku:kioku-capture", prompt)
	cmd := exec.Command(claude, args...)
	cmd.Dir = cwd
	cmd.Env = append(processEnv("CLAUDE_CODE_SESSION_ID", "KIOKU_CAPTURE_DIR"), "KIOKU_CAPTURE_DIR="+tmp)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = runErr.Error()
		}
		return fmt.Errorf("capture run: claude --bg failed: %s", reason)
	}
	launched = true
	if errors.Is(runErr, exec.ErrWaitDelay) {
		warning := "warning: capture launcher exited but output pipes remained open; session may be running"
		fmt.Fprintln(a.Err, warning)
		if err := a.captureNote(warning); err != nil {
			return err
		}
	}
	id := ""
	if match := captureBackgroundID.FindStringSubmatch(stdout.String()); len(match) > 1 {
		id = match[1]
	}
	if id == "" {
		warning := "warning: capture background id could not be parsed; lock retained until token unlock"
		fmt.Fprintln(a.Err, warning)
		if err := a.captureNote(warning); err != nil {
			return err
		}
	} else {
		last := filepath.Join(a.metadataDir(), "capture.last")
		previous := ""
		if data, err := readRegular(last); err == nil {
			previous = strings.TrimSpace(string(data))
		}
		if err := a.recordCaptureID(path, lock.Token, id); err != nil {
			return err
		}
		if previous != "" && previous != id {
			cleanup := exec.Command(claude, "rm", previous)
			cleanup.Dir = cwd
			cleanup.Env = processEnv("CLAUDE_CODE_SESSION_ID")
			cleanup.WaitDelay = 5 * time.Second
			_ = cleanup.Run()
			guard, err := openLockedFile(a.metadataDir(), os.O_RDONLY)
			if err != nil {
				return err
			}
			if data, err := readRegular(last); err == nil && strings.TrimSpace(string(data)) == previous {
				_ = os.Remove(last)
			}
			guard.Close()
		}
	}
	if id == "" {
		_, err = fmt.Fprintf(a.Out, "kioku-capture を背景で起動した（%s、%d 件、id 不明）。capture.log を確認する。\n", a.Config.Namespace, len(rows))
	} else {
		_, err = fmt.Fprintf(a.Out, "kioku-capture を背景で起動した（%s、%d 件、id %s）。claude logs %s で見る。\n", a.Config.Namespace, len(rows), id, id)
	}
	return err
}
func (a *App) captureReceipt(token string) string {
	return filepath.Join(a.metadataDir(), "capture-unlocked-"+token)
}
func (a *App) recordCaptureID(path, token, id string) error {
	return updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		var lock captureLock
		if data != nil {
			if err := decodeJSON(data, &lock); err != nil {
				return nil, false, err
			}
		}
		if data == nil || lock.Token != token {
			receipt := a.captureReceipt(token)
			if _, err := readRegular(receipt); err != nil {
				if os.IsNotExist(err) {
					return nil, false, nil
				}
				return nil, false, err
			}
			if err := atomicfile.Write(filepath.Join(a.metadataDir(), "capture.last"), []byte(id+"\n"), 0600); err != nil {
				return nil, false, err
			}
			return nil, false, os.Remove(receipt)
		}
		lock.BgID = id
		next, err := json.Marshal(lock)
		return append(next, '\n'), true, err
	})
}
func (a *App) captureUnlock(token string) error {
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
	if !lock.invalid && (!captureTokenPattern.MatchString(token) || lock.Token != token) {
		_, err = fmt.Fprintln(a.Out, "capture: unlock ignored (ownership token does not match)")
		return err
	}
	if !lock.invalid {
		if lock.BgID != "" {
			err = atomicfile.Write(filepath.Join(a.metadataDir(), "capture.last"), []byte(lock.BgID+"\n"), 0600)
		} else {
			err = atomicfile.Write(a.captureReceipt(token), []byte("unlocked\n"), 0600)
		}
		if err != nil {
			return err
		}
	}
	if captureTokenPattern.MatchString(token) {
		if err := os.RemoveAll(a.captureTmp(token)); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
