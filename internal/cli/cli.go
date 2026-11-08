package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mash/kioku/internal/buildinfo"
	"github.com/mash/kioku/internal/config"
)

type App struct {
	AuthDir          string
	Dir              string
	Config           Config
	Client           *http.Client
	Out, Err         io.Writer
	ctx              context.Context
	AgentToken       string
	storedAgentPath  string
	projectNamespace string
	description      *string
	kind             credentialKind
}

// credentialKind names the credential that Run selects for every request.
type credentialKind int

const (
	credentialNone credentialKind = iota
	credentialHuman
	credentialAgent
)

func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		return json.NewEncoder(out).Encode(buildinfo.Info())
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	global := flag.NewFlagSet("kioku", flag.ContinueOnError)
	global.SetOutput(errOut)
	dir := global.String("C", ".", "working directory (before command)")
	project := global.String("n", "", "project namespace ORG/PROJECT (before command)")
	server := global.String("server", config.Env("KIOKU_SERVER", ""), "server origin (before command)")
	if err = global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprint(out, help)
			return err
		}
		return err
	}
	commands := global.Args()
	if len(commands) == 0 || commands[0] == "help" {
		_, err = fmt.Fprint(out, help)
		return err
	}
	if os.Getenv("KIOKU_AGENT_TOKEN") != "" {
		return errors.New("KIOKU_AGENT_TOKEN is no longer supported; unset it and use kioku agent login")
	}
	a := &App{
		AuthDir: filepath.Join(home, ".config", "kioku", "auth"),
		Client:  &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Out:     out, Err: errOut, ctx: ctx,
	}
	var projectSet, dirSet bool
	global.Visit(func(f *flag.Flag) {
		projectSet = projectSet || f.Name == "n"
		dirSet = dirSet || f.Name == "C"
	})
	var origin string
	if projectSet {
		if !validNamespace(*project) {
			return errors.New("-n namespace must be organization/project")
		}
		a.projectNamespace = *project
		origin, err = resolveServer(*server)
		if err != nil {
			return err
		}
		if !dirSet {
			*dir, err = a.resolveProjectDir(origin, *project)
			if err != nil {
				return err
			}
		}
	}
	a.Dir, err = filepath.Abs(*dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(a.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("working directory must be a directory")
	}
	local, err := a.readProjectConfig()
	if err != nil {
		return err
	}
	if projectSet {
		if local.Server != "" && (local.Server != origin || local.Namespace != *project) {
			return errors.New("-n conflicts with .kioku/config; working directory is already bound to a different project")
		}
		if local.Server == "" && !allowsUnboundProject(commands) {
			return fmt.Errorf("no project configured; run kioku -n %s pull first", *project)
		}
	}
	a.Config = local
	if origin == "" && (*server != "" || local.Server == "") {
		origin, err = resolveServer(*server)
		if err != nil {
			return err
		}
	}
	if origin != "" {
		if local.Server != "" && local.Server != origin {
			return errors.New("server conflicts with .kioku/config")
		}
		a.Config.Server = origin
	}
	// Project configuration may come from an untrusted checkout. It cannot
	// authorize sending login codes or establishing credentials on another server.
	authenticating := commands[0] == "login" ||
		(len(commands) >= 2 && commands[0] == "agent" && commands[1] == "login")
	if authenticating {
		if err := a.trustAuthenticationServer(); err != nil {
			return err
		}
	}
	switch {
	case commands[0] == "login", len(commands) >= 2 && commands[0] == "agent" && commands[1] == "login":
		// Login commands select no credential, so a damaged file cannot block them.
	default:
		if commands[0] == "logout" && a.Config.Namespace != "" {
			// logout must not call localLock again: a second flock in one process blocks.
			unlock, err := a.localLock()
			if err != nil {
				return err
			}
			defer unlock()
		}
		if err := a.selectCredential(commands[0] == "logout"); err != nil {
			return err
		}
	}
	return a.dispatch(global.Args())
}

// The flag value includes KIOKU_SERVER's default from global flag parsing.
func resolveServer(flag string) (string, error) {
	if flag == "" {
		flag = defaultServer
	}
	return normalizeOrigin(flag)
}

// selectCredential prefers the human credential. Only without one does a bound
// directory select its agent credential.
func (a *App) selectCredential(allowExpiredAgent bool) error {
	if err := a.loadCredentials(); err != nil {
		return err
	}
	if a.Config.Token != "" {
		a.kind = credentialHuman
		return nil
	}
	if err := a.loadAgentCredentials(allowExpiredAgent); err != nil {
		return err
	}
	if a.AgentToken != "" {
		a.kind = credentialAgent
	}
	return nil
}

func (a *App) dispatch(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprint(a.Out, help)
		return err
	}
	switch args[0] {
	case "version":
		return a.versionCommand()
	case "agent":
		return a.agent(args[1:])
	case "account":
		return a.account(args[1:])
	case "login":
		return a.login(args[1:])
	case "logout":
		return a.logout()
	case "whoami":
		return a.whoami()
	case "org":
		return a.organization(args[1:])
	case "project":
		return a.project(args[1:])
	case "delete":
		return a.deleteSources(args[1:])
	case "put":
		return a.put(args[1:])
	case "generate":
		return a.generate(args[1:])
	case "pull":
		return a.pullCommand(args[1:])
	case "status":
		return a.status()
	case "job":
		return a.job(args[1:])
	case "open":
		return a.open()
	default:
		return fmt.Errorf("unknown command %q; run kioku help", args[0])
	}
}

// Creation establishes a new project; other project operations require pull.
func allowsUnboundProject(commands []string) bool {
	switch commands[0] {
	case "pull", "whoami", "login", "logout":
		return true
	case "agent":
		return len(commands) >= 2 && commands[1] == "login"
	case "project":
		return len(commands) >= 2 && (commands[1] == "create" || commands[1] == "join")
	}
	return false
}
