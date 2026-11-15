package server

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/publicapi"
	"github.com/mash/kioku/internal/runner"
)

func TestStorageContractExamplesAreComplete(t *testing.T) {
	b, err := os.ReadFile("testdata/storage/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Examples []struct{ ID, Input, Before, After string }
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Examples) != 25 {
		t.Fatalf("contract has %d examples", len(fixture.Examples))
	}
	for i, e := range fixture.Examples {
		if e.ID != strconv.Itoa(i+1) || e.Input == "" || e.Before == "" || e.After == "" {
			t.Fatalf("invalid example %d", i+1)
		}
	}
}
func TestJobResponseDoesNotExposeStorageFields(t *testing.T) {
	job := model.Job{ID: "j1", ProjectID: "private-project", Run: &model.AgentRun{
		TokenID: "private-token", Attempt: model.Attempt{ID: "private-attempt"}, OutputHash: "private-hash",
		Ref: runner.RunRef{ID: "private-session", Resources: map[string]string{"provider": "private-provider"}},
	}}
	b, err := json.Marshal(publicapi.Value(job))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-") {
		t.Fatalf("storage fields exposed: %s", b)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "kind", "status", "error", "revision", "created", "unchanged", "automatic", "agent_budget_cents", "user_id", "agent_id", "run"} {
		if _, ok := out[key]; !ok {
			t.Errorf("public key missing: %s", key)
		}
	}
}
