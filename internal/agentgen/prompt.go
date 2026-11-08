package agentgen

import (
	"fmt"
)

// FirstMessage gives the agent the job's identifiers and URLs. The system
// prompt holds every instruction, so this message carries data only.
func FirstMessage(spec InputSpec) string {
	return fmt.Sprintf("job: %s\nproject: %s\ninput: %s\noutput: %s\n", spec.JobID, spec.ProjectID, spec.InputURL, spec.OutputURL)
}
