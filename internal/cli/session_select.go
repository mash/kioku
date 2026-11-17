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
	const maxEntryBytes = 40 << 10
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
		b, err := io.ReadAll(io.LimitReader(f, maxEntryBytes+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		truncated := len(b) > maxEntryBytes
		if truncated {
			b = b[:maxEntryBytes]
		}
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
	for i := len(files) - 1; i >= 0 && utf8.RuneCountInString(render()) > limit; i-- {
		files[i].Truncated = true
		over := utf8.RuneCountInString(render()) - limit
		body := []rune(files[i].Body)
		files[i].Body = string(body[:max(0, len(body)-over)])
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
	if pullErr != nil && !localCopy {
		return pullErr
	}
	result := sessionPullSuccess
	if pullErr != nil {
		result = sessionPullFailure
		fmt.Fprintf(a.Err, "warning: pull failed: %v; using the local copy\n", pullErr)
	}
	ns := a.Config.Namespace
	if a.projectNamespace != "" {
		ns = a.projectNamespace
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
