package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func (a *App) deleteSources(args []string) (err error) {
	if len(args) == 0 {
		return errors.New("usage: kioku delete SOURCE...")
	}
	queuedSources := make(map[string]bool)
	for _, src := range args {
		if source.IsLocalKey(src) {
			continue
		}
		if strings.HasPrefix(src, source.LocalSource) {
			return errors.New("delete: ローカルキーは kioku:local/<64 桁> の全体を書く")
		}
		if _, err := parseCaptureSource(src); err != nil {
			return err
		}
		queuedSources[src] = true
	}
	endpoint, err := a.projectPath()
	if err != nil {
		return err
	}
	unlock, err := a.captureSubmissionLock()
	if err != nil {
		return err
	}
	defer unlock()
	removed := make(map[string]bool)
	if len(queuedSources) != 0 {
		unlockQueue, err := a.localLock()
		if err != nil {
			return err
		}
		err = a.updateQueue(func(rows []queueRow) []queueRow {
			return slices.DeleteFunc(rows, func(row queueRow) bool {
				if queuedSources[row.Source] {
					removed[row.Source] = true
					return true
				}
				return false
			})
		})
		unlockQueue()
		if err != nil {
			return err
		}
	}
	// Report completed local or remote removals even if a later request fails.
	// A broken stdout pipe must not prevent the server requests.
	defer func() {
		for _, src := range args {
			if removed[src] {
				if _, writeErr := fmt.Fprintf(a.Out, "removed %s\n", src); writeErr != nil {
					err = errors.Join(err, writeErr)
					return
				}
				delete(removed, src)
			}
		}
	}()
	authorized := false
	for _, src := range args {
		if strings.HasPrefix(src, toolSourcePrefix) {
			continue
		}
		if !authorized {
			if err := a.selectCredential(false); err != nil {
				return err
			}
			authorized = true
		}
		if _, err := a.postPut(endpoint, model.PutRequest{Remove: true, SourceURL: src}, nil); err != nil {
			return err
		}
		removed[src] = true
	}
	return nil
}
