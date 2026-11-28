package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

const putUsage = "usage: kioku put PATH... | kioku put file:///PATH"

var putScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func (a *App) put(args []string) error {
	if len(args) == 0 {
		return errors.New(putUsage)
	}
	scheme := putScheme.FindString(args[0])
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || putScheme.FindString(arg) != scheme {
			return errors.New(putUsage)
		}
	}
	switch {
	case scheme == "file:" && len(args) == 1:
		return a.putLocalFile(args[0])
	case scheme == "":
		return a.putPaths(args)
	case scheme != "file:":
		return fmt.Errorf("put: 対応しないスキーム %s。コロンを含む相対パスは ./ を付ける\n%s", scheme, putUsage)
	default:
		return errors.New(putUsage)
	}
}

func (a *App) capturePut(args []string) error {
	f := flag.NewFlagSet("capture put", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	captureToken := captureTokenFlag(f)
	src := f.String("source", "", "capture snapshot source")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 || *captureToken == "" || *src == "" {
		return errors.New("usage: kioku capture put --capture-token TOKEN --source SOURCE FILE")
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
	i := snapshotRow(snapshot, *src)
	if i < 0 {
		return errors.New("put: 取得元は capture の snapshot にない")
	}
	attempt := snapshot[i]
	queue, err := readQueue(a.queuePath())
	if err != nil {
		return err
	}
	j := snapshotRow(queue, *src)
	if j < 0 {
		return errors.New("put: queue に行が無い（delete 済み）")
	}
	if queue[j].State != queueQueued || !sameQueueAttempt(queue[j], attempt) {
		return errors.New("put: queue の行が再登録されたため、この capture では送らない")
	}
	results, err := readCaptureResults(dir)
	if err != nil {
		return err
	}
	if results[*src] != "" {
		return errors.New("put: この capture は取得元の結果を記録済み")
	}
	path := f.Arg(0)
	if !strings.EqualFold(filepath.Ext(path), ".md") {
		return errors.New("put: capture の資料は Markdown (.md) にする")
	}
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
	if !strings.HasPrefix(*src, toolSourcePrefix) && doc.URL != *src {
		return errors.New("put: source_url が capture の HTTPS 取得元と一致しない")
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
	logErr := a.captureNote("accepted " + doc.URL)
	_, err = fmt.Fprintf(a.Out, "accepted %s\n", doc.URL)
	return errors.Join(logErr, err)
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
					if source.IsLocalKey(u) {
						return fmt.Errorf("put: %s のローカルキーは delete にだけ指定する", name)
					}
					if u == source.LocalSource {
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
