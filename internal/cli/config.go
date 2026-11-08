package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

const defaultServer = "https://kioku.magicians.ee"

type Config struct {
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
	Token     string `json:"-"`
}

func normalizeOrigin(value string) (string, error) {
	u, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("server must be an HTTP(S) origin")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return "", errors.New("HTTPS is required for remote servers")
	}
	return u.String(), nil
}

func decodeJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("expected a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func (a *App) readProjectConfig() (Config, error) {
	var c Config
	if info, err := os.Lstat(a.metadataDir()); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return c, errors.New(".kioku must be a real directory")
		}
	} else if !os.IsNotExist(err) {
		return c, err
	}
	b, err := readRegular(filepath.Join(a.metadataDir(), "config"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = decodeJSON(b, &c); err != nil {
		return c, fmt.Errorf(".kioku/config: %w", err)
	}
	origin, err := normalizeOrigin(c.Server)
	if err != nil {
		return c, err
	}
	c.Server = origin
	if !validNamespace(c.Namespace) {
		return c, errors.New(".kioku/config namespace must be organization/project")
	}
	return c, nil
}

// Call under localLock when binding a directory; concurrent CLI calls cannot
// replace a different binding. Credentials are never serialized into this file.
func (a *App) saveProjectConfig() error {
	if !validNamespace(a.Config.Namespace) {
		return errors.New("invalid namespace")
	}
	current, err := a.readProjectConfig()
	if err != nil {
		return err
	}
	if current.Server != "" && (current.Server != a.Config.Server || current.Namespace != a.Config.Namespace) {
		return errors.New("working directory is already bound to a different project")
	}
	if current.Server != "" {
		return nil
	}
	if err := a.checkUnboundMirror(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a.Config, "", "  ")
	if err != nil {
		return err
	}
	return store.AtomicWrite(filepath.Join(a.metadataDir(), "config"), b, 0600)
}

type credentials struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func (a *App) credentialPath() string {
	return filepath.Join(a.AuthDir, auth.Hash(a.Config.Server)+".json")
}
func (a *App) loadCredentials() error {
	if info, err := os.Lstat(a.AuthDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("authentication directory must be a real directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := readRegular(a.credentialPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var c credentials
	if err = decodeJSON(b, &c); err != nil {
		return err
	}
	if c.Server != a.Config.Server {
		return errors.New("credential origin mismatch")
	}
	a.Config.Token = c.Token
	return nil
}
func (a *App) saveCredentials() error {
	if a.kind == credentialAgent {
		return errors.New("agent credentials cannot update human login state")
	}
	if a.AuthDir == "" {
		return errors.New("authentication directory is required")
	}
	if err := ensureDirectory(a.AuthDir); err != nil {
		return err
	}
	b, err := json.Marshal(credentials{Server: a.Config.Server, Token: a.Config.Token})
	if err != nil {
		return err
	}
	return store.AtomicWrite(a.credentialPath(), b, 0600)
}
