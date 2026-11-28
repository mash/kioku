package cli

import (
	"bytes"
	"context"
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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/atomicfile"
	"github.com/mash/kioku/prompts"
)

type captureLock struct {
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	BgID         string    `json:"bg_id,omitempty"`
	Token        string    `json:"token"`
	SnapshotHash string    `json:"snapshot_hash,omitempty"`
	invalid      bool
}

const captureLockTTL = 30 * time.Minute
const captureInstructionsFile = "instructions.md"
const captureGuardCLIEnv = "KIOKU_CAPTURE_CLI"

var captureWaitDelay = 5 * time.Second

var errCaptureBusy = errors.New("capture already running")
var captureBackgroundID = regexp.MustCompile(`backgrounded · ([0-9a-f]{8})\b`)
var captureTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func captureTokenFlag(f *flag.FlagSet) *string {
	var token string
	f.Func("capture-token", "capture ownership token", func(value string) error {
		if token != "" || !captureTokenPattern.MatchString(value) {
			return errors.New("invalid or repeated capture token")
		}
		token = value
		return nil
	})
	return &token
}

func readCaptureLock(path string) (*captureLock, error) {
	data, err := readRegular(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lock captureLock
	if decodeJSON(data, &lock) != nil || lock.StartedAt.IsZero() || !captureTokenPattern.MatchString(lock.Token) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		return &captureLock{StartedAt: info.ModTime(), invalid: true}, nil
	}
	return &lock, nil
}
func lockBusy(lock *captureLock, now time.Time) bool {
	return lock != nil && !lock.invalid && now.Sub(lock.StartedAt) <= captureLockTTL
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
func (a *App) capture(args []string, now time.Time) error {
	usage := errors.New("usage: kioku capture run|put|result|fetch")
	if len(args) == 0 {
		return usage
	}
	if _, err := a.projectPath(); err != nil {
		return err
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			return usage
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		return a.captureRun(cwd, now)
	case "put":
		return a.capturePut(args[1:])
	case "result":
		return a.captureResult(args[1:])
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
	lock, err := readCaptureLock(a.captureLockPath())
	if err != nil {
		return err
	}
	live := ""
	if lockBusy(lock, now) {
		live = lock.Token
	} else if lock != nil {
		if err := os.Remove(a.captureLockPath()); err != nil {
			return err
		}
	}
	files, err := filepath.Glob(filepath.Join(a.metadataDir(), "capture-progress-*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		info, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) > captureLockTTL {
			if err := os.Remove(file); err != nil {
				return err
			}
		}
	}
	return removeCaptureTmpExcept(filepath.Join(a.metadataDir(), "capture-tmp"), live)
}

func removeCaptureTmpExcept(root, keep string) error {
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
		if entry.Name() != keep {
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
	lock, err := readCaptureLock(a.captureLockPath())
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
	if err := removeCaptureTmpExcept(root, token); err != nil {
		return "", err
	}
	tmp := a.captureTmp(token)
	if err := os.Mkdir(tmp, 0700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(tmp)
}

func captureAllowedTools(command, project, tmp string, rows []queueRow) []string {
	token := filepath.Base(tmp)
	// Claude Code uses Edit(path) for Write permissions. --tools omits Edit.
	tools := []string{
		"Bash(" + command + " capture fetch:*)",
		"Bash(" + project + " capture put --capture-token " + token + ":*)",
		"Bash(" + project + " capture result --capture-token " + token + ":*)",
		"Read(/" + tmp + "/**)", "Edit(/" + tmp + "/**)", "Artifact",
	}
	seen := map[string]bool{}
	for _, row := range rows {
		server, _ := mcpToolParts(captureSourceTool(row.Source))
		if server != "" && !seen[server] {
			seen[server] = true
			tools = append(tools, "mcp__"+server+"__*")
		}
	}
	return tools
}
func (a *App) openCaptureLog() (*os.File, error) {
	log, err := openBackgroundLog(filepath.Join(a.metadataDir(), "capture.log"), os.O_APPEND|syscall.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	if stat, err := log.Stat(); err != nil {
		log.Close()
		return nil, err
	} else if !stat.Mode().IsRegular() {
		log.Close()
		return nil, errors.New("capture.log must be a regular file")
	}
	return log, nil
}

func (a *App) captureNote(note string) error {
	log, err := a.openCaptureLog()
	if err != nil {
		return err
	}
	defer log.Close()
	_, err = fmt.Fprintln(log, strings.NewReplacer("\r", " ", "\n", " ").Replace(note))
	return err
}

// beginCapture records the attempts and their snapshot before Claude can read either.
func (a *App) beginCapture(now time.Time) (*captureLock, []queueRow, error) {
	submit, unlock, err := a.lockCaptureQueue()
	if err != nil {
		return nil, nil, err
	}
	defer submit()
	defer unlock()
	if err := a.cleanupCaptureTmp(now); err != nil {
		return nil, nil, err
	}
	rows, err := readQueue(a.queuePath(), true)
	if err != nil {
		return nil, nil, err
	}
	if !slices.ContainsFunc(rows, func(row queueRow) bool { return row.State == queueQueued }) {
		return nil, nil, nil
	}
	path := a.captureLockPath()
	lock, err := acquireCaptureLock(path, now)
	if errors.Is(err, errCaptureBusy) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var snapshot []queueRow
	for i := range rows {
		row := &rows[i]
		if row.State != queueQueued {
			continue
		}
		if row.Attempts == maxCaptureAttempts {
			row.State = queueFailed
			if row.Reason == "" {
				row.Reason = "capture が 2 回とも結果を返さなかった"
			}
			continue
		}
		row.Attempts++
		snapshot = append(snapshot, *row)
	}
	if len(snapshot) != 0 {
		_, err = a.prepareCaptureTmp(lock.Token)
		if err == nil {
			err = a.saveCaptureSnapshot(lock.Token, snapshot)
		}
	}
	if err == nil {
		// queueLock still protects the rows read above, so do not read and parse them again.
		err = atomicfile.Write(a.queuePath(), formatQueue(rows), 0600)
	}
	if err != nil || len(snapshot) == 0 {
		return nil, nil, errors.Join(err, a.releaseCapture(lock.Token))
	}
	return lock, snapshot, nil
}

func (a *App) undoCaptureLaunch(token string, snapshot []queueRow, cause error) error {
	submit, unlock, err := a.lockCaptureQueue()
	if err != nil {
		return err
	}
	defer submit()
	defer unlock()
	lock, err := readCaptureLock(a.captureLockPath())
	if err != nil || lock == nil || lock.Token != token {
		return err
	}
	attempts := make(map[string]queueRow, len(snapshot))
	for _, row := range snapshot {
		attempts[row.Source] = row
	}
	err = a.updateQueue(func(rows []queueRow) []queueRow {
		for i, row := range rows {
			if prior, ok := attempts[row.Source]; ok && row == prior {
				rows[i].Attempts--
			}
		}
		return rows
	})
	// Publish the failure before another launcher can take ownership.
	return errors.Join(err, a.recordCaptureLaunchError(cause, false), a.releaseCapture(token))
}

func (a *App) captureRun(cwd string, now time.Time) (err error) {
	lock, rows, err := a.beginCapture(now)
	if err != nil {
		return errors.Join(err, a.recordCaptureLaunchError(err, false))
	}
	if lock == nil {
		return nil
	}
	path := a.captureLockPath()
	launched := false
	defer func() {
		if !launched {
			err = errors.Join(err, a.undoCaptureLaunch(lock.Token, rows, err))
		}
	}()
	pluginRoot := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if pluginRoot == "" {
		return errors.New("capture run: CLAUDE_PLUGIN_ROOT is required to load the capture guard")
	}
	root, err := filepath.Abs(pluginRoot)
	if err != nil {
		return err
	}
	tmp, err := filepath.EvalSymlinks(a.captureTmp(lock.Token))
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
	vars["tmp"] = tmp
	vars["instructions"] = filepath.Join(tmp, captureInstructionsFile)
	projectCommand := vars["kioku"] + " -C " + shellQuote(a.Dir) + " -n " + a.Config.Namespace
	// Keep arbitrary caller inputs out of the single argv element and do
	// not repeat the snapshot in the launcher's arguments.
	commands := captureCommands(vars["kioku"], projectCommand, tmp, lock.Token, rows)
	if err := atomicfile.Write(vars["instructions"], []byte(commands+"\n"), 0600); err != nil {
		return err
	}

	// The background daemon can reuse a process that never inherits cmd.Env.
	settings, err := json.Marshal(map[string]any{
		"env": map[string]string{
			captureModeEnv:           "1",
			captureDirEnv:            tmp,
			captureCommandEnv:        vars["kioku"],
			captureProjectCommandEnv: projectCommand,
			captureGuardCLIEnv:       exe,
		},
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": captureGuardMatcher,
				"hooks": []any{map[string]any{
					"type": "command", "command": "sh " + shellQuote(filepath.Join(root, "hooks", "kioku.sh")) + " tool-guard", "timeout": 10, "onFailure": "block",
				}},
			}},
		},
	})
	if err != nil {
		return err
	}
	args := []string{"--bg", "--permission-mode", "dontAsk", "--plugin-dir", root, "--settings", string(settings), "--tools", "Artifact,Bash,Read,Write"}
	args = append(args, "--allowedTools")
	args = append(args, captureAllowedTools(vars["kioku"], projectCommand, tmp, rows)...)
	prompt := renderTemplate(prompts.CaptureRun, vars)
	args = append(args, "--agent", "kioku:kioku-capture", "--", prompt)
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Dir = cwd
	cmd.Env = processEnv("CLAUDE_CODE_SESSION_ID", captureModeEnv, captureDirEnv, captureCommandEnv, captureProjectCommandEnv, captureGuardCLIEnv)
	cmd.WaitDelay = captureWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		reason := queueText(strings.TrimSpace(stderr.String()))
		if reason == "" {
			reason = runErr.Error()
		}
		return fmt.Errorf("capture run: claude --bg failed: %s", reason)
	}
	launched = true
	if errors.Is(runErr, exec.ErrWaitDelay) {
		fmt.Fprintln(a.Err, "warning: capture launcher exited but output pipes remained open; session may be running")
	}
	id := ""
	if match := captureBackgroundID.FindStringSubmatch(stdout.String()); len(match) > 1 {
		id = match[1]
	}
	if id == "" {
		fmt.Fprintln(a.Err, "warning: capture background id could not be parsed; lock retained until all results arrive or it expires")
		_, err := fmt.Fprintf(a.Out, "kioku-capture を背景で起動した（%s、%d 件、id 不明）。capture.log を確認する。\n", a.Config.Namespace, len(rows))
		return err
	}
	if err := a.recordCaptureID(path, lock.Token, id); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "kioku-capture を背景で起動した（%s、%d 件、id %s）。claude logs %s で見る。\n", a.Config.Namespace, len(rows), id, id)
	return err
}
func (a *App) recordCaptureID(path, token, id string) error {
	return updateCaptureLock(path, func(lock *captureLock) error {
		if lock != nil && lock.Token == token {
			lock.BgID = id
		}
		return nil
	})
}
func (a *App) releaseCapture(token string) error {
	guard, err := openLockedFile(a.metadataDir(), os.O_RDONLY)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	path := a.captureLockPath()
	lock, err := readCaptureLock(path)
	if err != nil || lock == nil {
		return err
	}
	if lock.invalid || lock.Token != token {
		return errors.New("capture lock ownership changed")
	}
	tmp := a.captureTmp(token)
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.Remove(captureProgressPath(tmp)); !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *App) captureLaunchErrorPath() string {
	return filepath.Join(a.metadataDir(), "capture-launch.failed")
}

type captureLaunchFailure struct {
	PID      int    `json:"pid"`
	Reason   string `json:"reason"`
	Reported bool   `json:"reported"`
}

func readCaptureLaunchFailure(data []byte) captureLaunchFailure {
	var failure captureLaunchFailure
	if decodeJSON(data, &failure) != nil {
		return captureLaunchFailure{Reason: strings.TrimSpace(string(data))}
	}
	return failure
}

func (a *App) recordCaptureLaunchError(err error, reported bool) error {
	data, err := json.Marshal(captureLaunchFailure{PID: os.Getpid(), Reason: queueText(err.Error()), Reported: reported})
	if err != nil {
		return err
	}
	return updateLockedFile(a.captureLaunchErrorPath(), func([]byte) ([]byte, bool, error) {
		return data, true, nil
	})
}

// With a nonzero PID, acknowledge only the child this hook waited for.
func (a *App) acknowledgeCaptureLaunchError(pid int) bool {
	path := a.captureLaunchErrorPath()
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	acknowledged := false
	err := updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		failure := readCaptureLaunchFailure(data)
		if failure.Reason == "" || failure.Reported || pid != 0 && pid != failure.PID {
			return nil, false, nil
		}
		failure.Reported = true
		next, err := json.Marshal(failure)
		acknowledged = err == nil
		return next, acknowledged, err
	})
	return err == nil && acknowledged
}

func (a *App) lastCaptureLaunchError() string {
	data, _ := readRegular(a.captureLaunchErrorPath())
	return readCaptureLaunchFailure(data).Reason
}

func (a *App) captureLockPath() string { return filepath.Join(a.metadataDir(), "capture.lock") }
func captureLockPathFromTmp(dir string) string {
	return filepath.Join(captureMetadataDir(dir), "capture.lock")
}
func captureMetadataDir(dir string) string { return filepath.Dir(filepath.Dir(dir)) }

func updateCaptureLock(path string, change func(*captureLock) error) error {
	return updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		var lock *captureLock
		var before captureLock
		if data != nil {
			lock = &captureLock{}
			if err := decodeJSON(data, lock); err != nil {
				return nil, false, err
			}
		}
		if lock != nil {
			before = *lock
		}
		if err := change(lock); err != nil {
			return nil, false, err
		}
		if lock == nil || *lock == before {
			return nil, false, nil
		}
		next, err := json.Marshal(lock)
		return next, true, err
	})
}
func captureCommands(command, project, tmp, token string, rows []queueRow) string {
	var b strings.Builder
	for i, row := range rows {
		src := shellQuote(row.Source)
		fmt.Fprintf(&b, "行 %d: %s\n", i+1, row.Source)
		if captureNeedsFetch(row.Source) {
			raw := filepath.Join(tmp, fmt.Sprintf("source-%03d.input%s", i+1, captureFetchExtension(row.Source, "")))
			fmt.Fprintf(&b, "公開 HTTPS の fetch（出力先は Read で読む）:\n```sh\n%s capture fetch %s --capture-token %s --dir %s --out %s\n```\n", command, src, token, shellQuote(tmp), shellQuote(raw))
		}
		path := filepath.Join(tmp, fmt.Sprintf("source-%03d.md", i+1))
		fmt.Fprintf(&b, "Markdown を %s に Write する。\nput:\n```sh\n%s capture put --capture-token %s --source %s %s\n```\n", path, project, token, src, shellQuote(path))
		fmt.Fprintf(&b, "capture result（受付を確認した場合）:\n```sh\n%s capture result --capture-token %s --result accepted %s\n```\nunsaved または skipped の場合は accepted を置き換え、その直後に --reason '<理由>' を入れる。\n", project, token, src)
	}
	return strings.TrimSpace(b.String())
}
