// Package prompts embeds the prompts used for server-side wiki generation.
package prompts

import _ "embed"

// WikiDefinition is shared by the CLI provisioning manifest and BYOK setup.
//
//go:embed kioku-wiki.md
var WikiDefinition string

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
