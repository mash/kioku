package model

import (
	"encoding/json"
	"testing"
)

func TestStorageKeysKeepExistingJSONRepresentation(t *testing.T) {
	// Existing records use ordinary JSON strings, not a structured key object.
	raw := []byte(`{"revision":{"files":{"wiki/index.md":"projects/p/wiki/index.md"}},"run":{"output_key":"projects/p/.kioku/jobs/j/output.tar.gz"}}`)
	var record struct {
		Revision Revision `json:"revision"`
		Run      AgentRun `json:"run"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Revision.Files["wiki/index.md"] != "projects/p/wiki/index.md" || record.Run.OutputKey != "projects/p/.kioku/jobs/j/output.tar.gz" {
		t.Fatal("existing keys not restored")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]map[string]any
	if err = json.Unmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	for section, fields := range map[string][]string{"run": {"output_key"}} {
		for _, field := range fields {
			if _, ok := generic[section][field].(string); !ok {
				t.Fatalf("%s.%s is no longer a JSON string", section, field)
			}
		}
	}
	files := generic["revision"]["files"].(map[string]any)
	if files["wiki/index.md"] != "projects/p/wiki/index.md" {
		t.Fatal("revision key representation changed")
	}
}
