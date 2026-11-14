package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type sessionHookInput struct {
	SessionID string          `json:"session_id"`
	Event     string          `json:"hook_event_name"`
	Source    string          `json:"source"`
	Cwd       string          `json:"cwd"`
	Prompt    string          `json:"prompt"`
	AgentID   json.RawMessage `json:"agent_id"`
}

// Parse hook JSON in the CLI so sh needs neither jq nor a language runtime.
func (a *App) sessionHook(args []string, input io.Reader) error {
	f := flag.NewFlagSet("session hook", flag.ContinueOnError)
	f.SetOutput(a.Err)
	event := f.String("event", "", "hook event (defaults to the input event)")
	prompts := f.String("prompts", "", "plugin prompt directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *prompts == "" || (*event != "" && *event != "SessionStart" && *event != "PostToolUse" && *event != "UserPromptSubmit") {
		return errors.New("usage: kioku session hook [--event EVENT] --prompts DIR")
	}
	const maxHookInput = 8 << 20
	b, err := io.ReadAll(io.LimitReader(input, maxHookInput+1))
	if err != nil || len(b) > maxHookInput {
		return nil
	}
	var in sessionHookInput
	if json.Unmarshal(b, &in) != nil || (*event != "" && in.Event != *event) || len(in.AgentID) != 0 {
		return nil
	}
	if in.Event != "SessionStart" && in.Event != "UserPromptSubmit" {
		return nil
	}
	path, err := sessionPath(in.SessionID)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	s, err := readSession(path, now)
	if err != nil || s.Namespace == "" {
		if in.Event == "UserPromptSubmit" {
			if line := candidateContext(*prompts, sessionCandidates(in.Prompt, in.Cwd)); line != "" {
				return writeHookContext(a.Out, in.Event, line)
			}
		}
		return nil
	}
	if in.Event == "SessionStart" {
		if in.Source != "compact" && in.Source != "resume" {
			return nil
		}
		// Use exactly the same output as `session context --session SID`.
		var context bytes.Buffer
		if err := runSession([]string{"context", "--session", in.SessionID}, &context, io.Discard); err != nil {
			return nil
		}
		if context.Len() == 0 {
			return nil
		}
		return writeHookContext(a.Out, in.Event, strings.TrimSuffix(context.String(), "\n"))
	}
	writeContext := func(line string) error {
		if line == "" {
			return nil
		}
		return writeHookContext(a.Out, in.Event, line)
	}
	urls := collectCaptureURLs(in.Prompt)
	if len(urls) == 0 {
		return writeContext(promptTurnContext(*prompts, s))
	}
	prompt, err := os.ReadFile(filepath.Join(*prompts, "hook-capture.txt"))
	if err != nil {
		return writeContext(promptTurnContext(*prompts, s))
	}
	return updateSession(in.SessionID, now, func(current *sessionState) *sessionState {
		if current == nil || current.Namespace == "" {
			return nil
		}
		seen := make(map[string]bool, len(current.NotifiedURLs))
		for _, url := range current.NotifiedURLs {
			seen[url] = true
		}
		var fresh []string
		for _, url := range urls {
			if !seen[url] {
				fresh = append(fresh, url)
				seen[url] = true
			}
		}
		if len(fresh) == 0 {
			_ = writeContext(promptTurnContext(*prompts, current))
			return nil
		}
		if err := writeContext(joinHookContext(promptTurnContext(*prompts, current), renderCapturePrompt(string(prompt), current, strings.Join(fresh, "\n")))); err != nil {
			return nil
		}
		current.NotifiedURLs = append(current.NotifiedURLs, fresh...)
		if len(current.NotifiedURLs) > 1000 {
			current.NotifiedURLs = current.NotifiedURLs[len(current.NotifiedURLs)-1000:]
		}
		return current
	})
}

func writeHookContext(out io.Writer, event, context string) error {
	return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": context}})
}

func renderCapturePrompt(prompt string, s *sessionState, target string) string {
	return strings.NewReplacer("{{namespace}}", s.Namespace, "{{server}}", s.Server, "{{mode}}", s.Mode, "{{path}}", s.Path, "{{urls}}", target).Replace(strings.TrimSpace(prompt))
}

var httpsURL = regexp.MustCompile(`https://[^\s<>"'\x60\\]+`)

// Extract URLs only from the user prompt, in their original order.
func collectCaptureURLs(prompt string) []string {
	var urls []string
	for _, match := range httpsURL.FindAllString(prompt, -1) {
		urls = append(urls, strings.TrimRight(match, ".,;:!?)〕]}"))
	}
	return urls
}
