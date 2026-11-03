package agentgen

import (
	_ "embed"
	"strings"
	"text/template"
)

// OutputRules is shared verbatim with the deployed agent's system prompt.
//
//go:embed prompts/output-rules.txt
var OutputRules string

//go:embed prompts/first-message.txt
var firstMessageText string

var firstMessage = template.Must(template.New("first-message").Parse(firstMessageText))

func FirstMessage(spec InputSpec) string {
	var b strings.Builder
	_ = firstMessage.Execute(&b, spec)
	return b.String()
}

// Templates for the generation agent's input archive.
var (
	//go:embed prompts/job.txt
	jobMessage string

	//go:embed prompts/changes-header.txt
	changesHeader string

	//go:embed prompts/changes-source.txt
	changesSource string

	//go:embed prompts/changes-updated-header.txt
	changesUpdatedHeader string

	//go:embed prompts/changes-updated-source.txt
	changesUpdatedSource string

	//go:embed prompts/changes-removed-header.txt
	changesRemovedHeader string
)
