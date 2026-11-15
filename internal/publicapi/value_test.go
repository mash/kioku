package publicapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestPublicValuesExcludeProjectStorageAndExecutionState(t *testing.T) {
	p := model.Project{ID: "private-project", Namespace: "acme/platform"}
	job := model.Job{ID: "job", ProjectID: p.ID, Status: "failed", Error: "projects/" + p.ID + "/input.tar.gz", Run: &model.AgentRun{}}
	for _, v := range []any{p, &p, model.ProjectEntry{Project: p}, []model.Project{p}, []model.ProjectEntry{{Project: p}}, job, &job, []model.Job{job}, model.Audit{ProjectID: p.ID}, []model.Audit{{ProjectID: p.ID}}} {
		b, err := json.Marshal(Value(v))
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
	if !strings.Contains(string(b), p.ID) {
		t.Fatal("internal DTO lost the project identity")
	}
}

func TestProjectValueOmitsDeadlineKeys(t *testing.T) {
	p := model.Project{Namespace: "org/project"}
	entry := model.ProjectEntry{Project: p}
	for _, value := range []any{p, &p, entry, &entry, []model.Project{p}, []model.ProjectEntry{entry}} {
		raw, err := json.Marshal(Value(value))
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
