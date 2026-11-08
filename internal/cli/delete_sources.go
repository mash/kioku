package cli

import (
	"errors"
	"fmt"

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
		if _, err := a.request("POST", p+"/put", model.PutRequest{Remove: true, SourceURL: url}, nil); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(a.Out, "removed %s\n", url); err != nil {
			return err
		}
	}
	return nil
}
