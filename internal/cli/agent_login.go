package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

type agentCredentials struct {
	Server    string    `json:"server"`
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Role      string    `json:"role"`
	Expires   time.Time `json:"expires"`
	Namespace string    `json:"namespace"`
}

func (a *App) agentCredentialPath() string {
	return filepath.Join(filepath.Dir(a.AuthDir), "agents", auth.Hash(a.Config.Server), filepath.FromSlash(a.Config.Namespace)+".json")
}

func (a *App) loadAgentCredentials(allowExpired bool) error {
	if a.Config.Namespace == "" {
		return nil
	}
	path := a.agentCredentialPath()
	b, err := readRegular(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved agentCredentials
	if err = decodeJSON(b, &saved); err != nil {
		return errors.New("invalid agent credential; run kioku agent login with a new code")
	}
	if saved.Server != a.Config.Server || saved.Namespace != a.Config.Namespace {
		return errors.New("agent credential server or namespace mismatch")
	}
	if err = saved.validate(allowExpired); err != nil {
		return err
	}
	a.AgentToken, a.storedAgentPath = saved.Token, path
	return nil
}

func (c agentCredentials) validate(allowExpired bool) error {
	parts := strings.Split(c.Token, ".")
	if c.ID == "" || len(parts) != 3 || parts[0] != "kioku_agent" || parts[1] != c.ID || parts[2] == "" || c.Role != "read" && c.Role != "write" || !validNamespace(c.Namespace) {
		return errors.New("invalid agent credential; run kioku agent login with a new code")
	}
	if !allowExpired && !c.Expires.After(time.Now()) {
		return errors.New("agent credential expired; run kioku agent login with a new code")
	}
	return nil
}

func (a *App) agentLogin(args []string) error {
	if len(args) != 1 || !strings.HasPrefix(args[0], "kioku_code.") {
		return errors.New("usage: kioku agent login CODE")
	}
	// A human credential wins over any agent credential, so refuse before the
	// code exchange and leave the code unused.
	human := *a
	if err := human.loadCredentials(); err != nil {
		return err
	}
	if human.Config.Token != "" {
		return errors.New("a human login exists; run kioku logout first to use connector login")
	}
	name, err := os.Hostname()
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(name)) == 0 || len(name) > 100 {
		return errors.New("hostname must contain 1–100 bytes")
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err = a.preflightProjectStorage(); err != nil {
		return err
	}
	current, err := a.readProjectConfig()
	if err != nil {
		return err
	}
	if current.Server != "" && current.Server != a.Config.Server {
		return errors.New("server conflicts with .kioku/config")
	}
	namespace := current.Namespace
	if namespace == "" {
		namespace = a.projectNamespace
	}
	// The one-use code is the only credential sent to the exchange endpoint.
	client := *a
	client.AgentToken, client.Config.Token, client.kind = "", "", credentialNone
	var issued agentCredentials
	if _, err = client.request("POST", "/api/agent/login", map[string]string{"code": args[0], "name": name, "namespace": namespace}, &issued); err != nil {
		return err
	}
	stored := false
	defer func() {
		if !stored && issued.Token != "" {
			client.AgentToken, client.kind = issued.Token, credentialAgent
			_, _ = client.request("POST", "/api/logout", nil, nil)
		}
	}()
	if err = issued.validate(false); err != nil {
		return err
	}
	if namespace != "" && namespace != issued.Namespace {
		return errors.New("working directory is already bound to a different project")
	}
	issued.Server = a.Config.Server
	a.Config.Namespace = issued.Namespace
	path := a.agentCredentialPath()
	if err = ensureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return errors.New("agent credential must be a regular file")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	if err = a.saveProjectConfig(); err != nil {
		return err
	}
	b, err := json.Marshal(issued)
	if err != nil {
		return err
	}
	if err = store.AtomicWrite(path, b, 0600); err != nil {
		return err
	}
	stored = true
	a.AgentToken, a.storedAgentPath, a.kind = issued.Token, path, credentialAgent
	return a.print(map[string]any{"id": issued.ID, "role": issued.Role, "expires": issued.Expires, "namespace": issued.Namespace})
}
