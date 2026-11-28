package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

type sessionHookInput struct {
	SessionID string          `json:"session_id"`
	Event     string          `json:"hook_event_name"`
	Source    string          `json:"source"`
	Cwd       string          `json:"cwd"`
	Prompt    string          `json:"prompt"`
	AgentID   json.RawMessage `json:"agent_id"`
}

func allowedHookEvent(event string) bool {
	switch event {
	case "SessionStart", "UserPromptSubmit", "Stop", "SessionEnd":
		return true
	default:
		return false
	}
}

// Parse hook JSON in the CLI so sh needs neither jq nor a language runtime.
func (a *App) sessionHook(args []string, input io.Reader) error {
	f := flag.NewFlagSet("session hook", flag.ContinueOnError)
	f.SetOutput(a.Err)
	event := f.String("event", "", "hook event (defaults to the input event)")
	prompts := f.String("prompts", "", "plugin prompt directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *prompts == "" || (*event != "" && !allowedHookEvent(*event)) {
		return errors.New("usage: kioku session hook [--event EVENT] --prompts DIR")
	}
	const maxHookInput = 8 << 20
	var b bytes.Buffer
	n, err := io.Copy(&b, io.LimitReader(input, maxHookInput+1))
	if err != nil || n > maxHookInput {
		return nil
	}
	var in sessionHookInput
	if json.Unmarshal(b.Bytes(), &in) != nil || (*event != "" && in.Event != *event) || len(in.AgentID) != 0 {
		return nil
	}
	if !allowedHookEvent(in.Event) {
		return nil
	}
	path, err := sessionPath(in.SessionID)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()

	if in.Event == "SessionStart" {
		var additionalContext string
		contextPrompt := "hook-start.txt"
		if in.Source == "compact" || in.Source == "resume" {
			s, _ := readSession(path, now)
			additionalContext = sessionContext(s)
			contextPrompt = "hook-compact.txt"
		}
		message := sessionStartCapture(in, now)
		if message != "" && additionalContext == "" {
			additionalContext, _ = readHookPrompt(*prompts, contextPrompt)
		}
		return writeHookOutput(a.Out, in.Event, additionalContext, message)
	}
	s, _ := readSession(path, now)
	if in.Event == "Stop" || in.Event == "SessionEnd" {
		return writeHookOutput(a.Out, in.Event, "", stopHook(in, s, now))
	}
	return writeHookOutput(a.Out, in.Event, promptHook(in, s, *prompts, now), "")
}

func readHookPrompt(prompts, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(prompts, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeHookOutput(out io.Writer, event, additionalContext, message string) error {
	result := map[string]any{}
	if additionalContext != "" {
		result["hookSpecificOutput"] = map[string]string{"hookEventName": event, "additionalContext": additionalContext}
	}
	if message != "" {
		result["systemMessage"] = message
	}
	if len(result) == 0 {
		return nil
	}
	return json.NewEncoder(out).Encode(result)
}

const sessionPullPendingContext = "背景の pull は終わっていない。wiki/ は 1 回前の pull のままか、まだ無い。"

func renderTemplate(text string, vars map[string]string) string {
	pairs := make([]string, 0, len(vars)*2)
	for _, key := range sortedKeys(vars) {
		pairs = append(pairs, "{{"+key+"}}", vars[key])
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func promptTurnContext(prompts string, s *sessionState) string {
	prompt, _ := readHookPrompt(prompts, "hook-turn.txt")
	exe, _ := os.Executable()
	line := renderTemplate(prompt, templateVars(exe, s.Server, s.Namespace, s.Path))
	var suffix string
	switch s.LastPullResult {
	case sessionPullNone:
		suffix = sessionPullPendingContext
	case sessionPullFailure:
		suffix = sessionPullFailedContext
	}
	if suffix != "" {
		if line != "" {
			line += "\n"
		}
		line += suffix
	}
	return line
}

func promptHook(in sessionHookInput, s *sessionState, prompts string, now time.Time) string {
	if s != nil {
		return promptTurnContext(prompts, s)
	}
	var ps []projectEntry
	if authDir, err := defaultAuthDir(); err == nil {
		if catalog, err := readProjects((&App{AuthDir: authDir}).projectsPath()); err == nil {
			ps = foldProjects(catalog.Projects)
		}
	}
	hits, words := matchTriggers(ps, in.Prompt)
	branch, config := walkAncestors(in.Cwd)
	if branch != "" {
		_, branchWords := matchTriggers(ps, branch)
		hits = nil
		for _, p := range ps {
			key := projectKey(p)
			matched := append(words[key], branchWords[key]...)
			words[key] = nil
			for _, word := range p.Triggers {
				if slices.Contains(matched, word) {
					words[key] = append(words[key], word)
				}
			}
			if len(words[key]) > 0 {
				hits = append(hits, p)
			}
		}
	}
	if len(hits) == 1 {
		if line := autoSelect(in, hits[0], words[projectKey(hits[0])], prompts, now); line != "" {
			return line
		}
	}
	if len(ps) == 0 && config.Namespace != "" {
		ps = []projectEntry{{Namespace: config.Namespace, Server: config.Server}}
	}
	exe, _ := os.Executable()
	vars := templateVars(exe, defaultServer, "", "")
	server := os.Getenv("KIOKU_SERVER")
	if server == "" {
		server = config.Server
	}
	origin, _ := resolveServer(server)
	vars["general_kioku"] = kiokuCommand(exe, origin)
	hints := make(map[string][]string)
	for _, p := range ps {
		key := projectKey(p)
		if p.Namespace == config.Namespace && p.Server == config.Server {
			hints[key] = append(hints[key], "cwd の .kioku/config")
		}
		if len(words[key]) > 0 {
			hints[key] = append(hints[key], "trigger 語 "+strings.Join(words[key], "、"))
		}
	}
	header, _ := readHookPrompt(prompts, "hook-candidates.txt")
	if header == "" {
		return ""
	}
	header = renderTemplate(header, vars)
	list := renderProjectList(vars["kioku"], ps, hints, contextLimit-utf8.RuneCountInString(header)-1)
	if list == "" {
		return header
	}
	return header + "\n" + list
}

func autoSelect(in sessionHookInput, p projectEntry, words []string, prompts string, now time.Time) string {
	files, err := readEntryFiles(p.Path)
	if err != nil {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	prompt, err := readHookPrompt(prompts, "hook-trigger.txt")
	if err != nil || prompt == "" {
		return ""
	}
	wikiInfo, wikiErr := os.Stat(filepath.Join(p.Path, "wiki"))
	localCopy := wikiErr == nil && wikiInfo.IsDir()
	s := &sessionState{Server: p.Server, Namespace: p.Namespace, Mode: "local", Path: p.Path}
	written := false
	err = updateSession(in.SessionID, now, func(old *sessionState) (*sessionState, bool) {
		if old != nil {
			return nil, false
		}
		written = true
		return s, true
	})
	if err != nil || !written {
		return ""
	}
	if err = startBackgroundPull(in, p, exe); err != nil {
		a := &App{Dir: p.Path, Config: Config{Server: p.Server, Namespace: p.Namespace}, backgroundPull: true}
		_ = a.saveSessionSelection(in.SessionID, p.Namespace, now, sessionPullFailure)
		s.LastPullAt, s.LastPullResult = now, sessionPullFailure
	}
	vars := templateVars(exe, p.Server, p.Namespace, p.Path)
	vars["words"] = strings.Join(words, "、")
	turn, _ := readHookPrompt(prompts, "hook-turn.txt")
	tail := renderTemplate(prompt, vars) + "\n" + renderTemplate(turn, vars)
	if s.LastPullResult == sessionPullFailure {
		tail += "\n" + sessionPullFailedContext
	}
	if !localCopy {
		tail = "wiki は背景で取得中。" + filepath.Join(p.Path, "wiki/index.md") + " ができたら読む。\n" + tail
	} else if len(files) == 0 {
		tail = "wiki/index.md と overview.md が無い。" + vars["wiki"] + " を grep する。\n" + tail
	}
	return fitEntry(files, tail, contextLimit)
}

var startProcess = func(c *exec.Cmd) error {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

func backgroundLog(path string) (*os.File, error) {
	return openBackgroundLog(path, os.O_TRUNC)
}
func openBackgroundLog(path string, mode int) (*os.File, error) {
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|mode|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func processEnv(exclude ...string) []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !slices.Contains(exclude, key) {
			env = append(env, value)
		}
	}
	return env
}

func startBackgroundPull(in sessionHookInput, p projectEntry, exe string) error {
	log, err := backgroundLog(filepath.Join(p.Path, ".kioku/pull.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	c := exec.Command(exe, "--server", p.Server, "-n", p.Namespace, "pull")
	c.Dir = p.Path
	c.Env = append(processEnv("CLAUDE_CODE_SESSION_ID", "KIOKU_SESSION_PULL"), "CLAUDE_CODE_SESSION_ID="+in.SessionID, "KIOKU_SESSION_PULL=background")
	c.Stdout, c.Stderr = log, log
	return startProcess(c)
}

func stopHook(in sessionHookInput, s *sessionState, now time.Time) string {
	if s == nil || s.Mode != "local" || !filepath.IsAbs(in.Cwd) {
		return ""
	}
	p := projectEntry{Server: s.Server, Namespace: s.Namespace, Path: s.Path}
	return projectCaptureNotice(in, p, now)
}

func sessionStartCapture(in sessionHookInput, now time.Time) string {
	authDir, err := defaultAuthDir()
	if err != nil {
		return ""
	}
	catalog, err := readProjects((&App{AuthDir: authDir}).projectsPath())
	if err != nil {
		return ""
	}
	messages := make([]string, len(catalog.Projects))
	var launches sync.WaitGroup
	for i, p := range catalog.Projects {
		launches.Go(func() { messages[i] = projectCaptureNotice(in, p, now) })
	}
	launches.Wait()
	return strings.Join(slices.DeleteFunc(messages, func(s string) bool { return s == "" }), "\n")
}

func projectCaptureNotice(in sessionHookInput, p projectEntry, now time.Time) string {
	a := &App{Dir: p.Path}
	rows, err := readHookQueue(a.queuePath())
	if err != nil {
		return ""
	}
	queued, failed := queueCounts(rows)
	var messages []string
	if queued != 0 {
		lock, err := readCaptureLock(a.captureLockPath())
		if err == nil && !lockBusy(lock, now) {
			cwd := in.Cwd
			if !filepath.IsAbs(cwd) {
				cwd = p.Path
			}
			// Let a detached process outlive the hook. Briefly observe the
			// launcher exit, not the capture session, to report fast failures.
			wait := 750 * time.Millisecond
			if in.Event == "SessionStart" {
				wait = 8 * time.Second
			} else if in.Event == "SessionEnd" {
				wait = 0
			}
			if err := startHookCapture(cwd, p, wait); err != nil {
				messages = append(messages, fmt.Sprintf("Kioku: %s の capture の起動に失敗した。kioku -C %s status と capture.log を確認する", p.Namespace, shellQuote(p.Path)))
			}
			if rows, err := readHookQueue(a.queuePath()); err == nil {
				_, failed = queueCounts(rows)
			}
		}
	}
	if in.Event == "SessionEnd" {
		return ""
	}
	if failed != 0 {
		messages = append(messages, fmt.Sprintf("Kioku: %s で受付済みにならなかった資料 %d 件。kioku -C %s status で理由を見る", p.Namespace, failed, shellQuote(p.Path)))
	}
	return strings.Join(messages, "\n")
}

func startHookCapture(cwd string, p projectEntry, wait time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var args []string
	if p.Server != defaultServer {
		args = append(args, "--server", p.Server)
	}
	args = append(args, "-C", p.Path, "-n", p.Namespace, "capture", "run")
	log, err := openBackgroundLog(filepath.Join(p.Path, ".kioku/capture.log"), os.O_APPEND|syscall.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer log.Close()
	if stat, err := log.Stat(); err != nil {
		return err
	} else if !stat.Mode().IsRegular() {
		return errors.New("capture.log must be a regular file")
	}
	c := exec.Command(exe, args...)
	c.Dir = cwd
	c.Env = processEnv("KIOKU_SERVER", "CLAUDE_CODE_SESSION_ID", "KIOKU_SESSION_PULL")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdout, c.Stderr = log, log
	if wait == 0 {
		return startProcess(c)
	}
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return nil
	}
}

// Bound hook reads without counting failed or malformed rows as pending work.
func readHookQueue(path string) ([]queueRow, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("queue must be a regular file")
	}
	const maxQueueBytes = 8 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxQueueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxQueueBytes {
		return nil, errors.New("queue exceeds 8 MiB")
	}
	return parseQueue(data)
}
