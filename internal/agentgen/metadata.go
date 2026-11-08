package agentgen

import (
	"fmt"
)

// JobMetadata is the fixed JOB.md content, independent of prompt customization.
func JobMetadata(spec InputSpec) string {
	return fmt.Sprintf("job: %s\nproject: %s\ninput: %s\noutput: %s\n", spec.JobID, spec.ProjectID, spec.InputURL, spec.OutputURL)
}
