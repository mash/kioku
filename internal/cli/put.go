package cli

import (
	"errors"
	"fmt"
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
	switch scheme {
	case "file:":
		if len(args) != 1 {
			return errors.New(putUsage)
		}
		return a.putLocalFile(args[0])
	case "":
		return a.putPaths(args)
	default:
		return fmt.Errorf("put: 対応しないスキーム %s。コロンを含む相対パスは ./ を付ける\n%s", scheme, putUsage)
	}
}

func (a *App) postPut(endpoint string, input model.PutRequest, output *model.PutResponse) (int, error) {
	var result any
	if output != nil {
		result = output
	}
	code, err := a.request(http.MethodPost, endpoint+"/put", input, result)
	if code == http.StatusAccepted {
		// For operations without a response body contract, the status is the receipt.
		if output == nil {
			return code, nil
		}
		return code, err
	}
	if err != nil {
		return code, err
	}
	return code, fmt.Errorf("put: 受付を確かめられない（HTTP %d）", code)
}

func (a *App) putPaths(paths []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	if err = a.ensureProject(); err != nil {
		return err
	}
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
			if _, err := source.FileWithProvenance(name, content); err != nil {
				switch {
				case errors.Is(err, source.ErrLocalKeyForPut):
					return fmt.Errorf("put: %s のローカルキーは delete にだけ指定する", name)
				case errors.Is(err, source.ErrLocalMarker):
					return fmt.Errorf("put: %s の source_url は %s です。ローカル資料は file:///abs/notes.md の形で put する", name, source.LocalSource)
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
		if _, err = a.postPut(p, model.PutRequest{File: &input}, nil); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(a.Out, "accepted %s\n", input.Name); err != nil {
			return err
		}
	}
	return nil
}
