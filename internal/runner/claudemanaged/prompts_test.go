package claudemanaged

import (
	"reflect"
	"testing"

	"github.com/mash/kioku/prompts"
)

func TestSessionPromptOverrides(t *testing.T) {
	for _, model := range []string{"", "model: chosen", "model: {id: chosen, effort: high, speed: fast, inference_geo: us}"} {
		t.Run(model, func(t *testing.T) {
			r, f, spec := fixture(t)
			system := "custom {{.BaseURL}}"
			if model != "" {
				system = "---\n" + model + "\n---\n" + system
			}
			loaded := loadPrompts(t, map[string]string{
				prompts.SystemFile: system,
				prompts.UserFile:   "custom user {{.JobID}} {{.InputURL}} {{.OutputURL}}",
			})
			r.system, r.model = loaded.System(), loaded.Model()
			input, output := r.FilePaths()
			spec.FirstMessage = loaded.User(prompts.JobVariables{JobID: spec.Key.JobID, InputURL: input, OutputURL: output})
			// Both sessions must carry all overrides, including when sharing an agent.
			for range 2 {
				start(t, r, spec)
			}
			count := 0
			for _, req := range f.Requests() {
				if req.Path != "/v1/sessions" || req.Method != "POST" {
					continue
				}
				count++
				a := req.Body["agent"].(map[string]any)
				if a["type"] != "agent_with_overrides" || a["id"] != "agent" || a["version"] != float64(3) || a["system"] != "custom https://kioku.test\n\n"+prompts.WikiInstructionsParagraph || len(a["tools"].([]any)) != 1 || !reflect.DeepEqual(a["mcp_servers"], []any{}) || !reflect.DeepEqual(a["skills"], []any{}) {
					t.Fatal(a)
				}
				switch model {
				case "":
					if _, ok := a["model"]; ok {
						t.Fatal("model should be omitted", a)
					}
				case "model: chosen":
					if !reflect.DeepEqual(a["model"], map[string]any{"id": "chosen"}) {
						t.Fatal(a)
					}
				default:
					if !reflect.DeepEqual(a["model"], map[string]any{"id": "chosen", "effort": "high", "speed": "fast", "inference_geo": "us"}) {
						t.Fatal(a)
					}
				}
				event := req.Body["initial_events"].([]any)[0].(map[string]any)
				if event["content"].([]any)[0].(map[string]any)["text"] != "custom user job /mnt/session/uploads/input.tar.gz /mnt/session/outputs/output.tar.gz" {
					t.Fatal(event)
				}
			}
			if count != 2 {
				t.Fatal(count)
			}
		})
	}
}
func defaultPrompts(t *testing.T) *prompts.Rendered {
	t.Helper()
	r, err := prompts.Load(prompts.Variables{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func loadPrompts(t *testing.T, files map[string]string) *prompts.Rendered {
	t.Helper()
	r, err := prompts.LoadWith(files, prompts.Variables{BaseURL: "https://kioku.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
