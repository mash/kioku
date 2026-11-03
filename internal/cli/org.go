package cli

import (
	"errors"

	"github.com/mash/kioku/internal/model"
)

func (a *App) organization(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku org create <slug> | kioku org list")
	}
	switch args[0] {
	case "transfer":
		return a.orgTransfer(args)
	case "create":
		return a.orgCreate(args)
	case "list":
		return a.orgList(args)
	default:
		return errors.New("unknown organization command")
	}
}

func safeSlug(s string) bool { return model.ValidSlug(s) }
