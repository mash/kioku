package publicapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestPublicValuesExcludeProjectStorageAndExecutionState(t *testing.T) {
	p := model.Project{ID: "private-project", Namespace: "acme/platform", StorageRoot: "projects/private-project/", StorageBucket: "private-bucket"}
	job := model.Job{ID: "job", ProjectID: p.ID, Status: "failed", Error: "projects/" + p.ID + "/input.tar.gz", Run: &model.AgentRun{}}
	for _, v := range []any{p, &p, model.ProjectEntry{Project: p}, []model.Project{p}, []model.ProjectEntry{{Project: p}}, job, &job, []model.Job{job}, model.Audit{ProjectID: p.ID}, []model.Audit{{ProjectID: p.ID}}} {
		b, err := json.Marshal(Value(v))
		if err != nil {
			t.Fatal(err)
		}
		for _, denied := range []string{p.ID, p.StorageBucket, `"project_id"`, `"storage_root"`, `"storage_bucket"`, `"ref"`, `"output_key"`} {
			if strings.Contains(string(b), denied) {
				t.Fatalf("%T leaked %s: %s", v, denied, b)
			}
		}
	}
	b, _ := json.Marshal(job)
	if !strings.Contains(string(b), p.ID) {
		t.Fatal("internal persistence changed")
	}
}
