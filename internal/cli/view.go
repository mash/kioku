package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/viewoutput"
)

const viewHelp = `Usage: kioku [-n ORG/PROJECT] view COMMAND

  rules                         Show the output contract (offline)
  list                          List project Views and generation status
  show SLUG                     Show the saved settings and last result
  set --prompt-file FILE SLUG    Save a UTF-8 prompt as the project owner

Use generate --wait to run the project pipeline and see each View's result.
`

func (a *App) view(args []string) error {
	method := http.MethodGet
	var suffix string
	var input any
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: kioku view list")
		}
	case "show":
		if len(args) != 2 || !safeSlug(args[1]) {
			return errors.New("usage: kioku view show SLUG")
		}
		suffix = "/" + args[1]
	case "set":
		flags := flag.NewFlagSet("view set", flag.ContinueOnError)
		flags.SetOutput(a.Err)
		file := flags.String("prompt-file", "", "UTF-8 file containing the View prompt")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || !safeSlug(flags.Arg(0)) || *file == "" {
			return errors.New("usage: kioku view set --prompt-file FILE SLUG")
		}
		prompt, err := readViewPrompt(*file)
		if err != nil {
			return err
		}
		method = http.MethodPatch
		suffix = "/" + flags.Arg(0)
		payload, err := json.Marshal(map[string]string{"prompt": prompt})
		if err != nil {
			return err
		}
		if len(payload) > model.MaxJSONRequestBytes {
			return errors.New("encoded View settings exceed 16 MiB")
		}
		input = json.RawMessage(payload)
	default:
		return fmt.Errorf("unknown view command %q; run kioku view --help", args[0])
	}
	project, err := a.projectPath()
	if err != nil {
		return err
	}
	var result any
	if _, err := a.request(method, project+"/views"+suffix, input, &result); err != nil {
		return err
	}
	return a.print(result)
}

func readViewPrompt(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, model.MaxJSONRequestBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > model.MaxJSONRequestBytes {
		return "", errors.New("View prompt exceeds 16 MiB")
	}
	prompt := string(body)
	if !utf8.ValidString(prompt) || strings.TrimSpace(prompt) == "" {
		return "", errors.New("View prompt must contain UTF-8 text")
	}
	return prompt, nil
}

func viewGuide(out io.Writer, args []string) (bool, error) {
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprint(out, viewHelp)
		return true, err
	}
	if args[0] != "rules" {
		return false, nil
	}
	if len(args) != 1 {
		return true, errors.New("usage: kioku view rules")
	}
	_, err := fmt.Fprint(out, viewoutput.Rules())
	return true, err
}
