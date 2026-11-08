package agentgen

import "testing"

func TestJobMetadataContainsOnlyJobMetadata(t *testing.T) {
	spec := InputSpec{
		JobID:     "job-123",
		ProjectID: "project-456",
		InputURL:  "https://kioku.test/jobs/job-123/input",
		OutputURL: "https://kioku.test/jobs/job-123/output",
		Inbox:     []InboxText{{Text: "source content"}},
		Wiki:      map[string]string{"wiki/index.md": "existing wiki"},
	}
	want := "job: job-123\n" +
		"project: project-456\n" +
		"input: https://kioku.test/jobs/job-123/input\n" +
		"output: https://kioku.test/jobs/job-123/output\n"
	if got := JobMetadata(spec); got != want {
		t.Fatalf("JobMetadata() = %q, want %q", got, want)
	}
}
