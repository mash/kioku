// Package prompts loads and validates server prompt templates.
// Hook files are read by the plugin through plugin/prompts and are not embedded.
package prompts

import "embed"

//go:embed wiki-agent.system.md wiki-agent.user.md mcp-server.yaml
var defaults embed.FS

// OutputRules is checked against the default system prompt.
//
//go:embed output-rules.txt
var OutputRules string
