package agentgen

import (
	"fmt"
)

// JobMetadata is the fixed JOB.md content, independent of prompt customization.
func JobMetadata(spec InputSpec) string {
	return fmt.Sprintf("job: %s\nnamespace: %s\ninput: %s\noutput: %s\n", spec.JobID, spec.Namespace, spec.InputURL, spec.OutputURL)
}
