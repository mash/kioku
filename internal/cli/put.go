package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

const putUsage = "usage: kioku put PATH... | put [--date DATE] file:///PATH | put [--note TEXT] [--now [--plugin-root DIR]] https://URL..."

var putScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func (a *App) put(args []string) error {
	f := flag.NewFlagSet("put", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	date := f.String("date", "", "document date")
	note := f.String("note", "kioku put", "why the URL was queued")
	now := f.Bool("now", false, "start capture now")
	pluginRoot := f.String("plugin-root", os.Getenv("CLAUDE_PLUGIN_ROOT"), "plugin root")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprintln(a.Err, "Usage of put:\n"+putUsage)
		return err
	} else if err != nil || f.NArg() == 0 {
		return errors.New(putUsage)
	}
	scheme := putScheme.FindString(f.Arg(0))
	for _, arg := range f.Args() {
		if s := putScheme.FindString(arg); s != "" && s != "file:" && s != "https:" {
			return fmt.Errorf("put: 対応しないスキーム %s です。https://、file:///、通常のパスのどれかを使う。コロンを含む相対パスは ./ を付ける", s)
		}
		if putScheme.FindString(arg) != scheme {
			return errors.New(putUsage)
		}
	}
	var dateSet, noteSet, nowSet, rootSet bool
	f.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "date":
			dateSet = true
		case "note":
			noteSet = true
		case "now":
			nowSet = true
		case "plugin-root":
			rootSet = true
		}
	})
	switch {
	case scheme == "file:" && f.NArg() == 1 && !noteSet && !nowSet && !rootSet:
		return a.putLocalFile(f.Arg(0), *date, dateSet)
	case scheme == "https:" && !dateSet && (!rootSet || *now):
		return a.putURLs(f.Args(), *note, *now, *pluginRoot)
	case scheme == "" && f.NFlag() == 0:
		return a.putPaths(f.Args())
	default:
		return errors.New(putUsage)
	}
}

func (a *App) putURLs(urls []string, note string, now bool, pluginRoot string) error {
	for _, u := range urls {
		if strings.ContainsAny(u, "\t\r\n") {
			return errors.New("put: URL にタブと改行を書かない")
		}
		if err := source.ValidateHTTPS(u); err != nil {
			return fmt.Errorf("put: %w", err)
		}
	}
	if now {
		if _, err := exec.LookPath("claude"); err != nil {
			return errors.New("put: claude コマンドが無いので --now を使えない。--now を外すか、Markdown を用意して kioku put PATH で送る")
		}
		if pluginRoot == "" {
			return errors.New("put: --now には plugin root が要る。--plugin-root DIR を付ける")
		}
	} else if os.Getenv("CLAUDE_CODE_SESSION_ID") == "" {
		return errors.New("put: Claude Code の外では capture が動かないので put https:// を使えない。--now を付けるか、Markdown を用意して kioku put PATH で送る")
	}

	at := time.Now()
	count, err := a.addQueueURLs(urls, note, at)
	if err != nil {
		return err
	}
	var outErr error
	for _, u := range urls {
		if _, outErr = fmt.Fprintf(a.Out, "queued %s（queue %d 件）\n", u, count); outErr != nil {
			break
		}
	}
	if !now {
		return outErr
	}
	cwd, err := os.Getwd()
	if err != nil {
		return errors.Join(outErr, err)
	}
	if outErr != nil {
		app := *a
		app.Out = io.Discard
		a = &app
	}
	return errors.Join(outErr, a.captureRun(1, cwd, at.UTC(), pluginRoot))
}

func (a *App) capturePut(args []string) error {
	f := flag.NewFlagSet("capture put", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	captureToken := captureTokenFlag(f)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 || *captureToken == "" {
		return errors.New("usage: kioku capture put --capture-token TOKEN FILE")
	}
	endpoint, err := a.projectPath()
	if err != nil {
		return err
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := resolveCaptureDir(*captureToken, a.captureTmp(*captureToken))
	if err != nil {
		return err
	}
	snapshot, err := captureSnapshot(dir)
	if err != nil {
		return fmt.Errorf("put: %w", err)
	}
	path := f.Arg(0)
	b, err := readCaptureSource(dir, path)
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	content := string(b)
	doc, err := source.FileWithProvenance(name, content)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	i := slices.IndexFunc(snapshot, func(row queueRow) bool { return row.URL == doc.URL })
	if i == -1 {
		i = slices.IndexFunc(snapshot, func(row queueRow) bool {
			u, err := url.Parse(row.URL)
			return err == nil && captureFetchURL(u).String() == doc.URL
		})
		if i == -1 {
			return errors.New("put: source_url が capture のスナップショット URL と一致しない")
		}
	}
	attempt := snapshot[i]
	queue, err := readQueue(a.queuePath())
	if err != nil {
		return err
	}
	j := slices.IndexFunc(queue, func(row queueRow) bool { return row.URL == attempt.URL })
	if j == -1 {
		return errors.New("put: queue に行が無い（delete 済み）")
	}
	if !sameQueueAttempt(queue[j], attempt) {
		return errors.New("put: queue の行が再登録されたため、この capture では送らない")
	}
	if attempt.URL != doc.URL {
		content, err = source.RewriteFrontmatter(content, attempt.URL, "")
		if err != nil {
			return fmt.Errorf("put: %w", err)
		}
	}
	if err := a.selectCredential(false); err != nil {
		return err
	}
	input := model.FileInput{Name: name, Content: content}
	code, err := a.request(http.MethodPost, endpoint+"/put", model.PutRequest{File: &input}, nil)
	if err != nil {
		return err
	}
	if code != http.StatusAccepted {
		return fmt.Errorf("put: 受付を確かめられない（HTTP %d）", code)
	}
	_, err = fmt.Fprintf(a.Out, "accepted %s\n", input.Name)
	return err
}

func (a *App) putPaths(paths []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	if err = a.ensureProject(); err != nil {
		return err
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	inputs := []model.FileInput{}
	for _, root := range paths {
		info, err := os.Lstat(root)
		if err != nil {
			return err
		}
		base := filepath.Dir(root)
		if info.IsDir() {
			base = root
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink source is not supported: %s", p)
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return errors.New("only regular files are supported")
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Size() > maxSourceFileBytes {
				return fmt.Errorf("%s exceeds 1 MiB", p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			name, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			content := string(b)
			// This is a pre-flight check; the server is authoritative.
			if _, err := source.FileWithProvenance(name, content); err != nil {
				if metadata, parseErr := source.Frontmatter(content); parseErr == nil {
					u, _ := source.FrontmatterSourceURL(metadata)
					if u == source.LocalSource || source.LocalKeyDate(u) != "" {
						return fmt.Errorf("put: %s の source_url は %s です。ローカル資料は file:///abs/notes.md の形で put する", name, u)
					}
				}
				return fmt.Errorf("%s: %w", p, err)
			}
			inputs = append(inputs, model.FileInput{Name: filepath.ToSlash(name), Content: content})
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(inputs) == 0 {
		fmt.Fprintln(a.Out, "新しい投入ファイルはありません。")
		return nil
	}
	if err := a.selectCredential(false); err != nil {
		return err
	}
	for _, input := range inputs {
		if _, err = a.request("POST", p+"/put", model.PutRequest{File: &input}, nil); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(a.Out, "accepted %s\n", input.Name); err != nil {
			return err
		}
	}
	return nil
}
