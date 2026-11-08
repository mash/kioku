package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func (a *App) put(args []string) error {
	f := flag.NewFlagSet("put", flag.ContinueOnError)
	f.SetOutput(a.Err)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		return errors.New("usage: kioku put FILE...")
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
			_, err = source.FileWithProvenance(name, content)
			if err != nil {
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
