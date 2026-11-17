package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const selectInstruction = `Kioku: %s を選んだ（P=%s）。上が wiki の入口。本文のページは %s にあり、index.md から必要なものだけ読む。
返答の冒頭に 1 行だけ書く: 「Kioku: %s の wiki を読み込みました（理由: <選んだ理由>）」。違うと利用者が言ったら、利用者が示した project で %s -n <namespace> session select を実行する。`

const selectNoneText = "Kioku: このセッションは Kioku なしで進める。返答の冒頭に 1 行だけ書く: 「Kioku: 合う project が無いので Kioku なしで進めます」。"

type entryFile struct {
	Path, Body string
	Truncated  bool
}

func readEntryFiles(p string) ([]entryFile, error) {
	const maxEntryBytes = contextLimit * 4
	var files []entryFile
	for _, name := range []string{"index.md", "overview.md"} {
		path := filepath.Join(p, "wiki", name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("expected a regular file: %s", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, maxEntryBytes))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		truncated := info.Size() > int64(len(b))
		files = append(files, entryFile{Path: path, Body: strings.ToValidUTF8(string(b), ""), Truncated: truncated})
	}
	return files, nil
}

func fitEntry(files []entryFile, tail string, limit int) string {
	files = append([]entryFile(nil), files...)
	render := func() string {
		var parts []string
		for _, file := range files {
			part := "# " + file.Path + "\n" + file.Body
			if file.Truncated {
				part += fmt.Sprintf("\n…（上限 10,000 文字で以降を省いた。続きは %s を読む）。", file.Path)
			}
			parts = append(parts, part)
		}
		return strings.Join(append(parts, tail), "\n")
	}
	remaining := limit - utf8.RuneCountInString(render())
	for i := len(files) - 1; i >= 0 && remaining < 0; i-- {
		if !files[i].Truncated {
			files[i].Truncated = true
			remaining -= utf8.RuneCountInString(fmt.Sprintf("\n…（上限 10,000 文字で以降を省いた。続きは %s を読む）。", files[i].Path))
		}
		body := []rune(files[i].Body)
		cut := min(len(body), -remaining)
		files[i].Body = string(body[:len(body)-cut])
		remaining += cut
	}
	return render()
}

func (a *App) sessionSelect(args []string, now time.Time) error {
	if len(args) != 0 {
		return errors.New("usage: kioku [-n ORG/PROJECT] session select")
	}
	// syncWiki creates wiki/ before contacting the server; only a preexisting copy is a fallback.
	info, err := os.Stat(filepath.Join(a.Dir, "wiki"))
	localCopy := err == nil && info.IsDir()
	pullErr := a.selectCredential(false)
	if pullErr == nil {
		_, pullErr = a.syncProject()
	}
	ns := a.Config.Namespace
	if a.projectNamespace != "" {
		ns = a.projectNamespace
	}
	if pullErr != nil && (!localCopy || !validNamespace(ns)) {
		return pullErr
	}
	if !validNamespace(ns) {
		return errors.New("session select requires a valid project namespace")
	}
	result := sessionPullSuccess
	if pullErr != nil {
		result = sessionPullFailure
		fmt.Fprintf(a.Err, "warning: pull failed: %v; using the local copy\n", pullErr)
	}
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		err := updateSession(sid, now, func(*sessionState) (*sessionState, bool) {
			return &sessionState{Server: a.Config.Server, Namespace: ns, Mode: "local", Path: a.Dir, LastPullAt: now, LastPullResult: result}, true
		})
		if err != nil {
			fmt.Fprintf(a.Err, "warning: could not update session state: %v\n", err)
		}
	}
	files, err := readEntryFiles(a.Dir)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	vars := templateVars(exe, a.Config.Server, ns, a.Dir)
	tail := fmt.Sprintf(selectInstruction, ns, a.Dir, vars["wiki"], ns, vars["kioku"])
	if result == sessionPullFailure {
		tail += "\n" + sessionPullFailedContext
	}
	if len(files) == 0 {
		tail = "wiki/index.md と overview.md が無い。" + vars["wiki"] + " を grep する。\n" + tail
	}
	_, err = fmt.Fprintln(a.Out, fitEntry(files, tail, contextLimit-1))
	return err
}
