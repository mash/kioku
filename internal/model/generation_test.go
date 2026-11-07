package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/runner"
)

func TestJobRunJSON(t *testing.T) {
	zero, cents := int64(0), int64(137)
	for _, cost := range []*int64{nil, &zero, &cents} {
		job := Job{ID: "job", Run: &AgentRun{Ref: runner.RunRef{ID: "session", Resources: map[string]string{"vault_id": "vault"}}, Attempt: Attempt{ID: "attempt"}, CostCents: cost}}
		b, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"run":`) || strings.Contains(string(b), `"managed":`) || strings.Contains(string(b), `"cost_cents":`) != (cost != nil) {
			t.Fatal(string(b))
		}
		var got Job
		if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, job) {
			t.Fatal(got, err)
		}
	}
	var job Job
	if err := json.Unmarshal([]byte(`{"run":{"ref":{"id":"s"}},"managed":{"session_id":"legacy"}}`), &job); err != nil {
		t.Fatal(err)
	}
	if job.Run.Ref.ID != "s" {
		t.Fatal("unexpected compatibility behavior", job.Run)
	}
}
