package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func (a *App) deleteSources(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku delete SOURCE_URL...")
	}
	for _, url := range args {
		if err := source.ValidateSourceURL(url); err != nil {
			if strings.HasPrefix(url, source.LocalSource) {
				return errors.New("delete: ローカルキーは kioku:local/YYYYMMDD-<64 桁> の全体を書く")
			}
			return err
		}
	}
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	for i, url := range args {
		if !strings.HasPrefix(url, source.LocalSource) {
			if err := a.updateQueue(func(rows []queueRow) []queueRow { return removeRows(rows, map[string]bool{url: true}) }); err != nil {
				return err
			}
		}
		if i == 0 {
			if err := a.selectCredential(false); err != nil {
				return err
			}
		}
		if _, err := a.request("POST", p+"/put", model.PutRequest{Remove: true, SourceURL: url}, nil); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(a.Out, "removed %s\n", url); err != nil {
			return err
		}
	}
	return nil
}
