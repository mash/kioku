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

// Session state remembers selection and notification, never source acceptance.
type sessionState struct {
	Server         string    `json:"server"`
	Namespace      string    `json:"namespace"`
	Mode           string    `json:"mode"`
	Path           string    `json:"path"`
	Operation      string    `json:"operation"`
	Result         string    `json:"result"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastPullAt     time.Time `json:"last_pull_at"`
	LastPullResult string    `json:"last_pull_result"`
	NotifiedURLs   []string  `json:"notified_urls,omitempty"`
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
	b, err := readRegular(path)
	if err != nil {
		return nil, err
	}
	var s sessionState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.UpdatedAt.IsZero() || now.Sub(s.UpdatedAt) > sessionRetention ||
		s.Mode != "local" || !filepath.IsAbs(s.Path) ||
		(s.Namespace != "" && !validNamespace(s.Namespace)) ||
		(s.Result != "success" && s.Result != "failure") ||
		(s.Operation != "pull" && s.Operation != "project join" && s.Operation != "project create") {
		return nil, errors.New("invalid or expired session state")
	}
	if _, err := normalizeOrigin(s.Server); err != nil {
		return nil, err
	}
	if (s.LastPullResult == "" && !s.LastPullAt.IsZero()) ||
		(s.LastPullResult != "" && (s.LastPullAt.IsZero() || (s.LastPullResult != "success" && s.LastPullResult != "failure"))) {
		return nil, errors.New("invalid last pull")
	}
	return &s, nil
}

// Lock the directory, as projects.json does, so atomic replacement and pruning
// cannot lose a concurrent hook notification or another CLI selection.
func updateSession(sid string, now time.Time, change func(*sessionState) *sessionState) error {
	path, err := sessionPath(sid)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDirectory(dir); err != nil {
		return err
	}
	lock, err := openLockedFile(dir, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer lock.Close()
	old, _ := readSession(path, now)
	s := change(old)
	if s == nil {
		return nil
	}
	s.UpdatedAt = now
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

func (a *App) recordSession(operation string, result error) {
	sid := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if sid == "" || errors.Is(result, flag.ErrHelp) {
		return
	}
	namespace := a.Config.Namespace
	if a.projectNamespace != "" {
		namespace = a.projectNamespace
	}
	now := time.Now().UTC()
	err := updateSession(sid, now, func(old *sessionState) *sessionState {
		s := &sessionState{Server: a.Config.Server, Namespace: namespace, Mode: "local", Path: a.Dir}
		if old != nil && old.Server == s.Server && old.Namespace == s.Namespace && old.Path == s.Path {
			*s = *old
		}
		if old != nil {
			s.NotifiedURLs = old.NotifiedURLs
		}
		s.Operation, s.Result = operation, "success"
		if result != nil {
			s.Result = "failure"
		}
		if operation == "pull" {
			s.LastPullAt, s.LastPullResult = now, s.Result
		}
		return s
	})
	if err != nil && a.Err != nil {
		fmt.Fprintf(a.Err, "warning: could not update session state: %v\n", err)
	}
}

func sessionContext(s *sessionState) string {
	if s == nil || s.Namespace == "" {
		return ""
	}
	pull := "pull 未実行"
	if s.LastPullResult != "" {
		result := "成功"
		if s.LastPullResult == "failure" {
			result = "失敗"
		}
		pull = "pull " + s.LastPullAt.Format(time.RFC3339) + " " + result
	}
	tail := "再選択と再 pull は不要。"
	if s.Result == "failure" {
		tail = s.Operation + " 失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。"
	} else if s.LastPullResult == "" {
		tail = "再選択は不要。最初の pull と Wiki の参照を行う。"
	} else if s.LastPullResult == "failure" {
		tail = "最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。"
	}
	return fmt.Sprintf("Kioku: %s（%s, server=%s, P=%q, %s）。%s", s.Namespace, s.Mode, s.Server, s.Path, pull, tail)
}

func (a *App) session(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku session context --session SID")
	}
	switch args[0] {
	case "hook":
		return a.sessionHook(args[1:], os.Stdin)
	case "context":
		f := flag.NewFlagSet("session context", flag.ContinueOnError)
		f.SetOutput(a.Err)
		sid := f.String("session", "", "Claude Code session ID")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *sid == "" || f.NArg() != 0 {
			return errors.New("usage: kioku session context --session SID")
		}
		path, err := sessionPath(*sid)
		if err != nil {
			return nil
		}
		s, err := readSession(path, time.Now())
		if err == nil {
			if line := sessionContext(s); line != "" {
				_, err = fmt.Fprintln(a.Out, line)
				return err
			}
		}
		return nil
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
