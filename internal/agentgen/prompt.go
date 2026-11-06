package agentgen

import (
	"strings"
	"text/template"

	"github.com/mash/kioku/prompts"
)

var firstMessage = template.Must(template.New("first-message").Parse(prompts.FirstMessage))

func FirstMessage(spec InputSpec) string {
	var b strings.Builder
	_ = firstMessage.Execute(&b, spec)
	return b.String()
}
