// Package prompts loads and validates server prompt templates.
// Hook files are not embedded; only capture-run.txt is embedded for CLI capture sessions.
package prompts

import "embed"

//go:embed wiki-agent.system.md wiki-agent.user.md wiki-agent.default-prompt.md mcp-server.yaml
var defaults embed.FS

// OutputRules is checked against the default system prompt.
//
//go:embed output-rules.txt
var OutputRules string

// DefaultWikiPrompt returns the embedded instructions for organizing a wiki.
func DefaultWikiPrompt() string {
	body, _ := defaults.ReadFile(DefaultPromptFile)
	return string(body)
}

// CaptureRun instructs the background capture session.
//
//go:embed capture-run.txt
var CaptureRun string
