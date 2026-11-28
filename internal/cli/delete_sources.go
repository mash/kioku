package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func (a *App) deleteSources(args []string) error {
	return a.deleteSourcesFrom(args, os.Stdin)
}

func (a *App) deleteSourcesFrom(args []string, input io.Reader) error {
	if len(args) == 1 && args[0] == "-" {
		data, err := io.ReadAll(io.LimitReader(input, maxCaptureSourceBytes+2))
		if err != nil {
			return err
		}
		src := strings.TrimSuffix(string(data), "\n")
		if len(src) > maxCaptureSourceBytes || !strings.HasPrefix(src, toolSourcePrefix) {
			return errors.New("delete - requires exactly one tool: source on stdin")
		}
		args = []string{src}
	}
	if len(args) == 0 {
		return errors.New("usage: kioku delete SOURCE... | kioku delete -")
	}
	for _, url := range args {
		if source.IsLocalKey(url) {
			continue
		}
		if _, err := parseCaptureSource(url); err != nil {
			if strings.HasPrefix(url, source.LocalSource) {
				return errors.New("delete: ローカルキーは kioku:local/<64 桁> の全体を書く")
			}
			return err
		}
	}
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	unlock, err := a.captureSubmissionLock()
	if err != nil {
		return err
	}
	defer unlock()
	authorized := false
	for _, url := range args {
		removed := false
		if !strings.HasPrefix(url, source.LocalSource) {
			unlockQueue, err := a.localLock()
			if err != nil {
				return err
			}
			err = a.updateQueue(func(rows []queueRow) []queueRow {
				next := removeQueueSource(rows, url)
				removed = len(next) != len(rows)
				return next
			})
			unlockQueue()
			if err != nil {
				return err
			}
		}
		if strings.HasPrefix(url, toolSourcePrefix) {
			if !removed {
				continue
			}
		} else {
			if !authorized {
				if err := a.selectCredential(false); err != nil {
					return err
				}
				authorized = true
			}
			if _, err := a.request("POST", p+"/put", model.PutRequest{Remove: true, SourceURL: url}, nil); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(a.Out, "removed %s\n", url); err != nil {
			return err
		}
	}
	return nil
}
