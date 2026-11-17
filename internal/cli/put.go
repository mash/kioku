package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"gopkg.in/yaml.v3"
)

func (a *App) put(args []string) error {
	f := flag.NewFlagSet("put", flag.ContinueOnError)
	f.SetOutput(a.Err)
	token := ""
	tokenSet := false
	f.Func("capture-token", "capture ownership token", func(value string) error {
		if tokenSet || !captureTokenPattern.MatchString(value) {
			return errors.New("invalid or repeated capture token")
		}
		token, tokenSet = value, true
		return nil
	})
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		return errors.New("usage: kioku put FILE...")
	}
	captureDir := ""
	var snapshot []queueRow
	if token != "" {
		if _, err := a.projectPath(); err != nil {
			return err
		}
		dir, err := resolveCaptureDir(token, a.captureTmp(token))
		if err != nil {
			return err
		}
		captureDir = dir
		snapshot, err = captureSnapshot(dir)
		if err != nil {
			return fmt.Errorf("put: %w", err)
		}
	}
	if captureDir != "" {
		for _, path := range f.Args() {
			if _, err := captureSourcePath(captureDir, path); err != nil {
				return err
			}
		}
	}
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
	paths := f.Args()
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
			if info.Size() > 1<<20 {
				return fmt.Errorf("%s exceeds 1 MiB", p)
			}
			var b []byte
			if captureDir == "" {
				b, err = os.ReadFile(p)
			} else {
				b, err = readCaptureSource(captureDir, p)
			}
			if err != nil {
				return err
			}
			name, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			content := string(b)
			// This is a pre-flight check; the server is authoritative.
			doc, err := source.FileWithProvenance(name, content)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if captureDir != "" {
				canonical := ""
				for _, row := range snapshot {
					u, parseErr := url.Parse(row.URL)
					if row.URL == doc.URL || (parseErr == nil && captureFetchURL(u).String() == doc.URL) {
						canonical = row.URL
						break
					}
				}
				if canonical == "" {
					return errors.New("put: source_url が capture のスナップショット URL と一致しない")
				}
				if canonical != doc.URL {
					content, err = captureCanonicalSource(content, canonical)
					if err != nil {
						return fmt.Errorf("put: %w", err)
					}
				}
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

// captureCanonicalSource preserves the body and records the snapshot URL.
func captureCanonicalSource(content, canonical string) (string, error) {
	metadata, err := source.Frontmatter(content)
	if err != nil {
		return "", err
	}
	for i := 0; i < len(metadata.Content); i += 2 {
		if metadata.Content[i].Value == "source_url" {
			metadata.Content[i+1].Value = canonical
		}
	}
	encoded, err := yaml.Marshal(metadata)
	if err != nil {
		return "", err
	}
	_, rest, _ := strings.Cut(content, "\n")
	for rest != "" {
		line, body, _ := strings.Cut(rest, "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			return "---\n" + string(encoded) + "---\n" + body, nil
		}
		rest = body
	}
	return "", errors.New("frontmatter closing delimiter missing")
}
