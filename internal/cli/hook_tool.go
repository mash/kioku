package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mash/kioku/internal/source"
)

// hookTool only records a source for an already selected, locally writable
// project. Hook errors must never block, change, or print the tool result.
func hookTool(ctx context.Context, input io.Reader) {
	if os.Getenv(captureModeEnv) == "1" {
		return
	}
	var in hookToolEvent
	if readHookJSON(input, &in) != nil || in.Event != "PostToolUse" {
		return
	}
	now := time.Now()
	path, err := sessionPath(in.SessionID)
	if err != nil {
		return
	}
	state, err := readSession(path, now)
	if err != nil || state == nil || state.Mode != "local" {
		return
	}
	authDir, err := defaultAuthDir()
	if err != nil {
		return
	}
	a := &App{Dir: state.Path, AuthDir: authDir, Config: Config{Server: state.Server, Namespace: state.Namespace}}
	bound, err := a.readProjectConfig()
	if err != nil || bound.Server != state.Server || bound.Namespace != state.Namespace {
		return
	}
	// Human members can submit; locally bound agents have an explicit read
	// or write role. The server still authorizes each eventual capture put.
	if err := a.selectCredential(false); err != nil ||
		(a.kind != credentialHuman && (a.kind != credentialAgent || a.agentRole != "write")) {
		return
	}
	if src := toolHookSource(ctx, in); src != "" {
		_ = a.registerQueueSource(src, now)
	}
}

func toolHookSource(ctx context.Context, in hookToolEvent) string {
	var raw string
	switch in.Name {
	case "WebFetch":
		var arg struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(in.Input, &arg) == nil {
			raw = arg.URL
		}
	case "Artifact":
		var arg artifactInput
		if json.Unmarshal(in.Input, &arg) != nil {
			return ""
		}
		switch {
		case arg.Action == "read":
			raw = arg.URL
		// The native tool also uses implicit publish with a file_path.
		case arg.Action == "publish" || arg.Action == "" && arg.FilePath != "":
			raw = publishedArtifactURL(in.Response)
			if raw != "" && arg.URL != "" {
				raw = arg.URL
			}
		}
		if !isArtifactURL(raw) {
			return ""
		}
	case "Bash":
		var arg struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(in.Input, &arg) == nil {
			raw = githubViewSource(ctx, in.Cwd, arg.Command)
		}
	default:
		got, _ := captureToolSource(in.Name, in.Input)
		return got
	}
	if source.ValidateHTTPS(raw) == nil {
		return raw
	}
	return ""
}

var publishResultLine = regexp.MustCompile(`^Published [^\r\n]+ at (https://[^ \t\r\n]+) \(Version [0-9]+, version id [^() \t\r\n]+\)`)

func publishedArtifactURL(response json.RawMessage) string {
	var line string
	if json.Unmarshal(response, &line) == nil {
		if match := publishResultLine.FindStringSubmatch(line); len(match) == 2 && isArtifactURL(match[1]) {
			return match[1]
		}
		return ""
	}
	var obj struct {
		URL     string          `json:"url"`
		Success *bool           `json:"success"`
		IsError bool            `json:"is_error"`
		Error   json.RawMessage `json:"error"`
		Status  string          `json:"status"`
	}
	if json.Unmarshal(response, &obj) == nil && !obj.IsError && (obj.Success == nil || *obj.Success) &&
		(len(obj.Error) == 0 || string(obj.Error) == "null") &&
		(obj.Status == "" || obj.Status == "published" || obj.Status == "success") && isArtifactURL(obj.URL) {
		return obj.URL
	}
	return ""
}

var githubRepoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var githubViewNumber = regexp.MustCompile(`^[1-9][0-9]*$`)

func githubViewSource(ctx context.Context, cwd, command string) string {
	args, ok := literalShellWords(command)
	if !ok || len(args) < 4 || args[0] != "gh" || (args[1] != "issue" && args[1] != "pr") || args[2] != "view" {
		return ""
	}
	target, repo, ok := githubViewArgs(args[3:])
	if !ok {
		return ""
	}
	if source.ValidateHTTPS(target) == nil {
		return target
	}
	if !githubViewNumber.MatchString(target) || !filepath.IsAbs(cwd) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	origin := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	origin.Dir = cwd
	data, err := origin.Output()
	if err != nil {
		return ""
	}
	remote := strings.TrimSpace(string(data))
	if !strings.HasPrefix(remote, "git@github.com:") && !strings.Contains(remote, "://") {
		return ""
	}
	resolved := githubOriginRepo(remote)
	if resolved == "" || repo != "" && !strings.EqualFold(githubOriginRepo(repo), resolved) {
		return ""
	}
	section := "issues"
	if args[1] == "pr" {
		section = "pull"
	}
	return "https://github.com/" + resolved + "/" + section + "/" + target
}

func githubViewArgs(args []string) (target, repo string, ok bool) {
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		flag, value, attached := strings.Cut(arg, "=")
		switch flag {
		case "-R", "--repo", "--json", "--jq", "-q", "--template", "-t":
			if !attached {
				if len(args) == 0 || strings.HasPrefix(args[0], "-") {
					return "", "", false
				}
				value, args = args[0], args[1:]
			}
			if flag == "-R" || flag == "--repo" {
				if repo != "" || value == "" {
					return "", "", false
				}
				repo = value
			}
		case "--comments", "-c":
			if attached {
				return "", "", false
			}
		default:
			if strings.HasPrefix(arg, "-") || target != "" {
				return "", "", false
			}
			target = arg
		}
	}
	return target, repo, target != ""
}

func githubOriginRepo(raw string) string {
	repo := raw
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		repo = strings.TrimPrefix(raw, "git@github.com:")
	case strings.HasPrefix(raw, "github.com/"):
		repo = strings.TrimPrefix(raw, "github.com/")
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || !strings.EqualFold(u.Host, "github.com") ||
			u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		repo = strings.TrimPrefix(u.Path, "/")
	}
	repo = strings.TrimSuffix(repo, ".git")
	owner, name, _ := strings.Cut(repo, "/")
	if !githubRepoName.MatchString(repo) || owner == "." || owner == ".." || name == "." || name == ".." {
		return ""
	}
	return repo
}
