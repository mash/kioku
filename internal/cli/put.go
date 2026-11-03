package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func (a *App) put(args []string) error {
	f := flag.NewFlagSet("put", flag.ContinueOnError)
	f.SetOutput(a.Err)
	wait := f.Bool("wait", false, "wait for source intake to complete (does not generate or pull wiki)")
	if err := f.Parse(args); err != nil {
		return err
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
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	r, err := a.readReceipts()
	if err != nil {
		return err
	}
	inputs := []model.FileInput{}
	hashes := map[string]string{}
	paths := f.Args()
	if len(paths) == 1 && strings.HasPrefix(paths[0], "https://") {
		var job model.Job
		if _, err = a.request("POST", p+"/put", model.PutRequest{URL: paths[0]}, &job); err != nil {
			return err
		}
		unlock()
		locked = false
		if *wait {
			return a.wait(job)
		}
		return a.print(job)
	}
	inbox := len(paths) == 0
	if inbox {
		paths = []string{filepath.Join(a.localDir(), "inbox")}
	}
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
			if info.Size() > 4<<20 {
				return fmt.Errorf("%s exceeds 4 MiB", p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := auth.Hash(string(b))
			abs, err := filepath.Abs(p)
			if err != nil {
				return err
			}
			if inbox && r.Hashes[abs] == h {
				return nil
			}
			name, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			inputs = append(inputs, model.FileInput{Name: filepath.ToSlash(name), Content: string(b)})
			hashes[abs] = h
			if len(inputs) > 30 {
				return errors.New("at most 30 files per put")
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(inputs) == 0 {
		fmt.Fprintln(a.Out, "新しい投入ファイルはありません。")
		if *wait && r.LastJob != "" {
			unlock()
			locked = false
			return a.job([]string{"wait", r.LastJob})
		}
		return nil
	}
	var job model.Job
	if _, err = a.request("POST", p+"/put", model.PutRequest{Files: inputs}, &job); err != nil {
		return err
	}
	for path, h := range hashes {
		r.Hashes[path] = h
	}
	r.LastJob = job.ID
	b, _ := json.MarshalIndent(r, "", "  ")
	if err = store.AtomicWrite(filepath.Join(a.metadataDir(), "receipts.json"), b, 0600); err != nil {
		return err
	}
	unlock()
	locked = false
	if !*wait {
		return a.print(job)
	}
	return a.wait(job)
}
