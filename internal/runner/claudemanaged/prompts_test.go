package claudemanaged

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
			spec.FirstMessage = loaded.User(prompts.JobVariables{JobID: spec.Key.JobID, InputURL: "input", OutputURL: spec.OutputURL})
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
				if a["type"] != "agent_with_overrides" || a["id"] != "agent" || a["version"] != float64(3) || a["system"] != "custom https://kioku.test" || len(a["tools"].([]any)) != 1 || !reflect.DeepEqual(a["mcp_servers"], []any{}) || !reflect.DeepEqual(a["skills"], []any{}) {
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
				if event["content"].([]any)[0].(map[string]any)["text"] != "custom user job input https://kioku.test:8443/output" {
					t.Fatal(event)
				}
			}
			if count != 2 {
				t.Fatal(count)
			}
		})
	}
}
func TestSessionRejectedLogsOnlyStatusAndType(t *testing.T) {
	for _, code := range []int{400, 429, 503} {
		r, f, spec := fixture(t)
		f.SetError("POST", "/v1/sessions", code)
		var b bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
		_, err := r.Start(t.Context(), spec)
		slog.SetDefault(old)
		var api *APIError
		if !errors.As(err, &api) || api.Status != code || api.Type != "api_error" {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "session create rejected") != (code < 500) {
			t.Fatal(b.String())
		}
		if code < 500 && !strings.Contains(b.String(), "type=api_error") {
			t.Fatal(b.String())
		}
		if strings.Contains(b.String(), "fake provider") || strings.Contains(b.String(), "test-job-token") {
			t.Fatal(b.String())
		}
	}
}

func defaultPrompts(t *testing.T) *prompts.Rendered {
	t.Helper()
	r, err := prompts.Load("", prompts.Variables{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func loadPrompts(t *testing.T, files map[string]string) *prompts.Rendered {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := prompts.Load(dir, prompts.Variables{BaseURL: "https://kioku.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
