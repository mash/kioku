package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mash/kioku/internal/store"
	"github.com/mattn/go-isatty"
)

// Only a terminal can approve a project-supplied origin; piped input cannot.
var confirmAuthenticationServer = func(out io.Writer, origin string) error {
	return promptAuthenticationServer(os.Stdin, out, origin, isatty.IsTerminal(os.Stdin.Fd()))
}

func promptAuthenticationServer(in io.Reader, out io.Writer, origin string, terminal bool) error {
	fmt.Fprintf(out, "Authentication server: %s\n", origin)
	if !terminal {
		return errors.New("untrusted authentication server: run in a terminal to approve it, or explicitly select a trusted server with --server or KIOKU_SERVER")
	}
	fmt.Fprint(out, "Trust this server for future authentication? [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return errors.New("authentication server was not approved")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("authentication server was not approved")
	}
}

func (a *App) trustAuthenticationServer() error {
	if a.Config.Server == defaultServer {
		return nil
	}
	if err := ensureDirectory(a.AuthDir); err != nil {
		return err
	}
	// Separate per-origin files avoid lost approvals when two CLI processes run.
	path := a.credentialPath() + ".trusted"
	b, err := readRegular(path)
	if err == nil {
		var origin string
		if err := json.Unmarshal(b, &origin); err != nil {
			return fmt.Errorf("invalid trusted server record: %w", err)
		}
		if origin != a.Config.Server {
			return errors.New("trusted server origin mismatch")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	// Explicit selection is also consent, including in non-interactive scripts.
	if !a.ServerExplicit {
		if err := confirmAuthenticationServer(a.Err, a.Config.Server); err != nil {
			return err
		}
	}
	b, err = json.Marshal(a.Config.Server)
	if err != nil {
		return err
	}
	return store.AtomicWrite(path, b, 0600)
}
