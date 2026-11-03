package cli

import (
	"github.com/mash/kioku/internal/buildinfo"
)

func (a *App) versionCommand() error {
	return a.print(buildinfo.Info())
}
