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
	"strings"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/atomicfile"
	"github.com/mash/kioku/prompts"
)

type captureLock struct {
	PID            int       `json:"pid"`
	StartedAt      time.Time `json:"started_at"`
	BgID           string    `json:"bg_id,omitempty"`
	Token          string    `json:"token"`
	SnapshotHash   string    `json:"snapshot_hash,omitempty"`
	LaunchComplete bool      `json:"launch_complete,omitempty"`
	invalid        bool
}

const captureLockTTL = 30 * time.Minute

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
	if lockBusy(lock, now) && !lock.invalid {
		live = lock.Token
	}
	receipts, err := filepath.Glob(filepath.Join(a.metadataDir(), "capture-unlocked-*"))
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		info, err := os.Lstat(receipt)
		if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) <= captureLockTTL {
			continue
		}
		if err := os.Remove(receipt); err != nil {
			return err
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

func captureAllowedTools(exe, server, ns, p, tmp string) []string {
	command := kiokuCommand(exe, server)
	project := command + " -C " + shellQuote(p) + " -n " + ns
	token := filepath.Base(tmp)
	return []string{
		"Bash(" + command + " capture fetch:*)",
		"Bash(" + project + " capture put --capture-token " + token + ":*)",
		"Bash(" + project + " capture result --capture-token " + token + ":*)",
		"Bash(gh pr view:*)", "Bash(gh issue view:*)", "Bash(gh pr diff:*)",
		"Read(/" + tmp + "/**)", "Edit(/" + tmp + "/**)",
		"mcp__notion__notion-fetch", "mcp__notion__notion-search",
		"mcp__claude_ai_Notion__notion-fetch",
		"mcp__claude_ai_Notion__notion-search",
		"mcp__claude_ai_Slack__slack_read_thread",
		"mcp__claude_ai_Slack__slack_read_channel",
		"mcp__claude_ai_Google_Drive__read_file_content",
		"mcp__claude_ai_Google_Drive__download_file_content",
	}
}
func (a *App) captureNote(note string) error {
	log, err := openBackgroundLog(filepath.Join(a.metadataDir(), "capture.log"), os.O_APPEND|syscall.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer log.Close()
	_, err = fmt.Fprintln(log, strings.NewReplacer("\r", " ", "\n", " ").Replace(note))
	return err
}

// beginCapture records the attempts and their snapshot before Claude can read either.
func (a *App) beginCapture(now time.Time) (*captureLock, []queueRow, error) {
	unlock, err := a.localLock()
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	rows, err := readQueue(a.queuePath())
	if err != nil {
		return nil, nil, err
	}
	queued, _ := queueCounts(rows)
	if queued == 0 {
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
		// localLock still protects the rows read above, so do not read and parse them again.
		err = atomicfile.Write(a.queuePath(), formatQueue(rows), 0600)
	}
	if err != nil || len(snapshot) == 0 {
		return nil, nil, errors.Join(err, a.discardCapture(lock.Token))
	}
	return lock, snapshot, nil
}

// discardCapture releases a capture whose launcher never started.
func (a *App) discardCapture(token string) error {
	err := a.releaseCapture(token)
	if e := os.Remove(a.captureReceipt(token)); e != nil && !os.IsNotExist(e) {
		err = errors.Join(err, e)
	}
	return err
}

func (a *App) undoCaptureLaunch(token string, snapshot []queueRow) error {
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
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
	return errors.Join(err, a.discardCapture(token))
}

func (a *App) captureRun(cwd string, now time.Time) (err error) {
	if err := a.cleanupCaptureTmp(now); err != nil {
		return err
	}
	lock, rows, err := a.beginCapture(now)
	if err != nil || lock == nil {
		return err
	}
	path := a.captureLockPath()
	launched := false
	defer func() {
		if !launched {
			err = errors.Join(err, a.undoCaptureLaunch(lock.Token, rows))
		}
	}()
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
	vars["tmp"], vars["queue"], vars["token"] = tmp, string(formatQueue(rows)), lock.Token
	vars["project_kioku"] = vars["kioku"] + " -C " + shellQuote(a.Dir) + " -n " + a.Config.Namespace
	vars["commands"] = captureCommands(vars["kioku"], vars["project_kioku"], tmp, lock.Token, rows)

	settings, err := a.captureSettings(cwd, tmp)
	if err != nil {
		return err
	}
	args := []string{"--bg", "--settings", settings, "--permission-mode", "dontAsk", "--allowedTools"}
	args = append(args, captureAllowedTools(exe, a.Config.Server, a.Config.Namespace, a.Dir, tmp)...)
	if pluginRoot := os.Getenv("CLAUDE_PLUGIN_ROOT"); pluginRoot != "" {
		root, err := filepath.Abs(pluginRoot)
		if err != nil {
			return err
		}
		vars["capture_md"] = filepath.Join(root, "skills", "kioku", "references", "capture.md")
		args = append(args, "Read(/"+filepath.Join(root, "skills")+"/**)", "Read(/"+filepath.Join(root, "agents")+"/**)")
	}
	template := prompts.CaptureRun
	if vars["capture_md"] == "" {
		_, template, _ = strings.Cut(template, "\n")
	}
	prompt := renderTemplate(template, vars)
	args = append(args, "--agent", "kioku:kioku-capture", "--", prompt)
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Dir = cwd
	cmd.Env = processEnv("CLAUDE_CODE_SESSION_ID")
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
		if err := a.recordCaptureID(path, lock.Token, ""); err != nil {
			return err
		}
		warning := "warning: capture background id could not be parsed; lock retained until all results arrive or it expires"
		fmt.Fprintln(a.Err, warning)
		if err := a.captureNote(warning); err != nil {
			return err
		}

		return a.captureStarted(fmt.Sprintf("kioku-capture を背景で起動した（%s、%d 件、id 不明）。capture.log を確認する。", a.Config.Namespace, len(rows)))
	}
	last := filepath.Join(a.metadataDir(), "capture.last")
	previous := ""
	if data, err := readRegular(last); err == nil {
		previous = strings.TrimSpace(string(data))
	}
	if err := a.recordCaptureID(path, lock.Token, id); err != nil {
		return err
	}
	if captureBackgroundID.MatchString("backgrounded · "+previous) && len(previous) == 8 && previous != id {
		cleanup := exec.CommandContext(ctx, claude, "rm", previous)
		cleanup.Dir = cwd
		cleanup.Env = processEnv("CLAUDE_CODE_SESSION_ID")
		cleanup.WaitDelay = captureWaitDelay
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
	return a.captureStarted(fmt.Sprintf("kioku-capture を背景で起動した（%s、%d 件、id %s）。claude logs %s で見る。", a.Config.Namespace, len(rows), id, id))
}

func (a *App) captureStarted(message string) error {
	// SessionStart may stop waiting for stdout, while Stop already sends it to capture.log.
	logged := false
	if out, ok := a.Out.(*os.File); ok {
		outInfo, outErr := out.Stat()
		logInfo, logErr := os.Stat(filepath.Join(a.metadataDir(), "capture.log"))
		logged = outErr == nil && logErr == nil && os.SameFile(outInfo, logInfo)
	}
	if !logged {
		_ = a.captureNote(message)
	}
	_, err := fmt.Fprintln(a.Out, message)
	return err
}
func (a *App) captureReceipt(token string) string {
	return filepath.Join(a.metadataDir(), "capture-unlocked-"+token)
}
func (a *App) recordCaptureID(path, token, id string) error {
	return updateCaptureLock(path, func(lock *captureLock) error {
		if lock == nil || lock.Token != token {
			receipt := a.captureReceipt(token)
			if _, err := readRegular(receipt); err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if id != "" {
				if err := atomicfile.Write(filepath.Join(a.metadataDir(), "capture.last"), []byte(id+"\n"), 0600); err != nil {
					return err
				}
			}
			return os.Remove(receipt)
		}
		lock.BgID, lock.LaunchComplete = id, true
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
	if lock.invalid || !captureTokenPattern.MatchString(token) || lock.Token != token {
		return errors.New("capture lock ownership changed")
	}
	{
		if lock.BgID != "" {
			err = atomicfile.Write(filepath.Join(a.metadataDir(), "capture.last"), []byte(lock.BgID+"\n"), 0600)
		} else if !lock.LaunchComplete {
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

func (a *App) captureLockPath() string { return filepath.Join(a.metadataDir(), "capture.lock") }
func captureLockPathFromTmp(dir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(dir)), "capture.lock")
}
func updateCaptureLock(path string, change func(*captureLock) error) error {
	return updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		var lock *captureLock
		if data != nil {
			lock = &captureLock{}
			if err := decodeJSON(data, lock); err != nil {
				return nil, false, err
			}
		}
		if err := change(lock); err != nil {
			return nil, false, err
		}
		if lock == nil {
			return nil, false, nil
		}
		next, err := json.Marshal(lock)
		return next, !bytes.Equal(bytes.TrimSpace(data), next), err
	})
}
func (a *App) captureSettings(cwd, tmp string) (string, error) {
	deny := []string{"Glob", "Grep", "WebFetch", "WebSearch", "Bash(curl:*)", "Bash(wget:*)", "Read(~/.ssh/**)", "Read(~/.aws/**)", "Read(~/.config/**)", "Read(~/.netrc)", "Read(~/.claude.json)", "Read(~/.claude/.credentials.json)"}
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(cwd, tmp)
	if err != nil {
		return "", err
	}
	if filepath.IsLocal(rel) {
		if err := a.captureNote("capture: 一時ディレクトリが起動 cwd 内にあるため Read(./**) の deny を省いた。"); err != nil {
			return "", err
		}
	} else {
		deny = append(deny, "Read(./**)")
	}
	data, err := json.Marshal(map[string]any{"disableAllHooks": true, "permissions": map[string]any{"deny": deny}})
	return string(data), err
}

func captureCommands(command, project, tmp, token string, rows []queueRow) string {
	var b strings.Builder
	for i, row := range rows {
		src := shellQuote(row.Source)
		fmt.Fprintf(&b, "行 %d: %s\n", i+1, row.Source)
		if !strings.HasPrefix(row.Source, toolSourcePrefix) {
			raw := filepath.Join(tmp, fmt.Sprintf("source-%03d.input%s", i+1, captureFetchExtension(row.Source, "")))
			fmt.Fprintf(&b, "公開 HTTPS の fetch（出力先は Read で読む）:\n```sh\n%s capture fetch %s --capture-token %s --dir %s --out %s\n```\n", command, src, token, shellQuote(tmp), shellQuote(raw))
		}
		path := filepath.Join(tmp, fmt.Sprintf("source-%03d.md", i+1))
		fmt.Fprintf(&b, "Markdown を %s に Write する。\nput:\n```sh\n%s capture put --capture-token %s --source %s %s\n```\n", path, project, token, src, shellQuote(path))
		fmt.Fprintf(&b, "capture result（結果と理由だけ置き換える）:\n```sh\n%s capture result --capture-token %s --result '<結果>' --reason '<理由>' %s\n```\n", project, token, src)
	}
	return strings.TrimSpace(b.String())
}
