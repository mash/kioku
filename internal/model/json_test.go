package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicValuesExcludeProjectStorageAndExecutionState(t *testing.T) {
	p := Project{ID: "private-project", Namespace: "acme/platform"}
	job := Job{ID: "job", ProjectID: p.ID, Status: "failed", Error: "projects/" + p.ID + "/input.tar.gz", Run: &AgentRun{}}
	for _, v := range []any{p, &p, ProjectEntry{Project: p}, []Project{p}, []ProjectEntry{{Project: p}}, job, &job, []Job{job}, Audit{ProjectID: p.ID}, []Audit{{ProjectID: p.ID}}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, denied := range []string{p.ID, `"project_id"`, `"storage_root"`, `"storage_bucket"`, `"ref"`, `"output_key"`} {
			if strings.Contains(string(b), denied) {
				t.Fatalf("%T leaked %s: %s", v, denied, b)
			}
		}
	}
	b, _ := json.Marshal(job)
	if strings.Contains(string(b), p.ID) {
		t.Fatal("JSON leaked the project identity")
	}
}

func TestProjectValueOmitsDeadlineKeys(t *testing.T) {
	p := Project{Namespace: "org/project"}
	entry := ProjectEntry{Project: p}
	for _, value := range []any{p, &p, entry, &entry, []Project{p}, []ProjectEntry{entry}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"next_generate_at"`, `"generate_interval_seconds"`} {
			if strings.Contains(string(raw), key) {
				t.Errorf("%T exposed %s: %s", value, key, raw)
			}
		}
		if !strings.Contains(string(raw), `"namespace":"org/project"`) {
			t.Fatal("namespace missing", string(raw))
		}
	}
}
