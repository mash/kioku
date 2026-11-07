// Package prompts embeds the prompts used for server-side wiki generation.
// The hook-*.txt files in this directory are not embedded: the Claude plugin
// reads them through the plugin/prompts symlink.
package prompts

import (
	_ "embed"
	"errors"
	"strings"
)

// WikiDefinition contains the generation agent manifest and system prompt.
//
//go:embed kioku-wiki.md
var WikiDefinition string

// SplitWikiDefinition returns the YAML frontmatter and the system prompt of
// WikiDefinition.
func SplitWikiDefinition() (frontmatter, system string, err error) {
	parts := strings.SplitN(WikiDefinition, "---", 3)
	if len(parts) != 3 || parts[0] != "" {
		return "", "", errors.New("invalid embedded agent definition")
	}
	return parts[1], parts[2], nil
}

// OutputRules is shared verbatim with the deployed agent's system prompt.
//
//go:embed output-rules.txt
var OutputRules string

//go:embed first-message.txt
var FirstMessage string

// Templates for the generation agent's input archive.
var (
	//go:embed job.txt
	Job string

	//go:embed changes-header.txt
	ChangesHeader string

	//go:embed changes-source.txt
	ChangesSource string

	//go:embed changes-updated-header.txt
	ChangesUpdatedHeader string

	//go:embed changes-updated-source.txt
	ChangesUpdatedSource string

	//go:embed changes-removed-header.txt
	ChangesRemovedHeader string
)
