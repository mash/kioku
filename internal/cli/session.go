package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mash/kioku/internal/store"
)

const sessionRetention = 30 * 24 * time.Hour

const sessionPullFailedContext = "最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。"

type sessionPullResult string

const (
	sessionPullNone    sessionPullResult = ""
	sessionPullSuccess sessionPullResult = "success"
	sessionPullFailure sessionPullResult = "failure"
)

// Only CLI project operations write selection and pull results.
type sessionState struct {
	Server         string            `json:"server"`
	Namespace      string            `json:"namespace"`
	Mode           string            `json:"mode"`
	Path           string            `json:"path"`
	LastPullAt     time.Time         `json:"last_pull_at,omitzero"`
	LastPullResult sessionPullResult `json:"last_pull_result,omitempty"`
}

func sessionPath(sid string) (string, error) {
	if !safeID(sid) {
		return "", errors.New("invalid session ID")
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("XDG_STATE_HOME must be absolute")
	}
	return filepath.Join(base, "kioku", "sessions", sid+".json"), nil
}

func readSession(path string, now time.Time) (*sessionState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if now.Sub(info.ModTime()) > sessionRetention {
		return nil, errors.New("expired session state")
	}
	b, err := readRegular(path)
	if err != nil {
		return nil, err
	}
	var s sessionState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Mode != "local" || !filepath.IsAbs(s.Path) || !validNamespace(s.Namespace) {
		return nil, errors.New("invalid session state")
	}
	if _, err := normalizeOrigin(s.Server); err != nil {
		return nil, err
	}
	switch s.LastPullResult {
	case sessionPullNone:
		if !s.LastPullAt.IsZero() {
			return nil, errors.New("invalid last pull")
		}
	case sessionPullSuccess, sessionPullFailure:
		if s.LastPullAt.IsZero() {
			return nil, errors.New("invalid last pull")
		}
	default:
		return nil, errors.New("invalid last pull")
	}
	return &s, nil
}

// Save a complete CLI selection, then prune expired session files.
func saveSession(sid string, now time.Time, s *sessionState) error {
	path, err := sessionPath(sid)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDirectory(dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := store.AtomicWrite(path, append(b, '\n'), 0600); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || !safeID(strings.TrimSuffix(entry.Name(), ".json")) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > sessionRetention {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (a *App) recordSessionPull(result error) {
	pullResult := sessionPullSuccess
	if result != nil {
		pullResult = sessionPullFailure
	}
	a.recordSession(pullResult)
}

// An empty lastPullResult records a successful project selection without a pull.
func (a *App) recordSession(lastPullResult sessionPullResult) {
	sid := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if sid == "" {
		return
	}
	namespace := a.Config.Namespace
	if a.projectNamespace != "" {
		namespace = a.projectNamespace
	}
	now := time.Now().UTC()
	err := a.saveSessionSelection(sid, namespace, now, lastPullResult)
	if err != nil && a.Err != nil {
		fmt.Fprintf(a.Err, "warning: could not update session state: %v\n", err)
	}
}

func (a *App) saveSessionSelection(sid, namespace string, now time.Time, result sessionPullResult) error {
	path, err := sessionPath(sid)
	if err != nil {
		return err
	}
	// Serialize CLI selections so a failed pull cannot restore an older target.
	dir := filepath.Dir(path)
	if err := ensureDirectory(dir); err != nil {
		return err
	}
	lock, err := openLockedFile(dir, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer lock.Close()
	old, readErr := readSession(path, now)
	// Replace missing, expired, or malformed state, but preserve unreadable state.
	var pathErr *os.PathError
	if errors.As(readErr, &pathErr) && !os.IsNotExist(readErr) {
		return readErr
	}
	s := &sessionState{Server: a.Config.Server, Namespace: namespace, Mode: "local", Path: a.Dir}
	sameProject := old != nil && old.Server == s.Server && old.Namespace == s.Namespace && old.Path == s.Path
	if result == sessionPullFailure && !sameProject {
		return nil
	}
	if sameProject {
		*s = *old
	}
	if result != sessionPullNone {
		s.LastPullAt, s.LastPullResult = now, result
	}
	return saveSession(sid, now, s)
}

func sessionContext(s *sessionState) string {
	if s == nil {
		return ""
	}
	pull := "pull 未実行"
	if s.LastPullResult != sessionPullNone {
		result := "成功"
		if s.LastPullResult == sessionPullFailure {
			result = "失敗"
		}
		pull = "pull " + s.LastPullAt.Format(time.RFC3339) + " " + result
	}
	tail := "再選択と再 pull は不要。"
	if s.LastPullResult == sessionPullNone {
		tail = "再選択は不要。最初の pull と Wiki の参照を行う。"
	} else if s.LastPullResult == sessionPullFailure {
		tail = sessionPullFailedContext
	}
	return fmt.Sprintf("Kioku: %s（%s, server=%s, P=%q, %s）。%s", s.Namespace, s.Mode, s.Server, s.Path, pull, tail)
}

func (a *App) session(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku session hook [--event EVENT] --prompts DIR")
	}
	switch args[0] {
	case "hook":
		return a.sessionHook(args[1:], os.Stdin)
	default:
		return fmt.Errorf("unknown session command %q", args[0])
	}
}

// Session utilities require neither a project directory nor credentials.
func runSession(args []string, out, errOut io.Writer) error {
	err := (&App{Out: out, Err: errOut}).session(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}
