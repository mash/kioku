package agents

import _ "embed"

// WikiDefinition is shared by the CLI provisioning manifest and BYOK setup.
//
//go:embed kioku-wiki.md
var WikiDefinition string
