package cli

import (
	"errors"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func (a *App) deleteSources(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku delete SOURCE_URL...")
	}
	for _, url := range args {
		if err := source.ValidateSourceURL(url); err != nil {
			return err
		}
	}
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	for _, url := range args {
		var item model.InboxItem
		if _, err := a.request("POST", p+"/put", model.PutRequest{Remove: true, SourceURL: url}, &item); err != nil {
			return err
		}
		if err := a.print(item); err != nil {
			return err
		}
	}
	return nil
}
