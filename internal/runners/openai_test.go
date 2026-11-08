package runners

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	openairunner "github.com/mash/kioku/internal/runner/openai"
	"github.com/mash/kioku/prompts"
)

func TestOpenAISelection(t *testing.T) {
	c := Config{Prompts: defaultPrompts(t), Type: "openai", BaseURL: "https://kioku.test", OpenAIAPIKey: "test-key", OpenAIModel: "test-model"}
	s, err := Load(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	r, err := s.New(model.Job{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*openairunner.FileRunner); !ok {
		t.Fatalf("%T", r)
	}
	for _, change := range []func(*Config){func(c *Config) { c.OpenAIAPIKey = "" }, func(c *Config) { c.OpenAIModel = "" }} {
		bad := c
		change(&bad)
		if _, err := Load(bad, nil); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestOpenAIPromptOverridesOnWire(t *testing.T) {
	rendered := loadPrompts(t, map[string]string{
		prompts.SystemFile: "---\nmodel: ignored-anthropic-model\n---\nCustom {{.BaseURL}} instructions",
		prompts.UserFile:   "Fetch {{.InputURL}} and upload {{.OutputURL}}",
	})
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	found := false
	http.DefaultTransport = promptTransport(func(req *http.Request) (*http.Response, error) {
		var response string
		switch req.URL.Path {
		case "/v1/files":
			response = `{"id":"file"}`
		case "/v1/agents/sessions":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			agent := body["agent"].(map[string]any)
			if agent["model"] != "openai-model" || !strings.HasPrefix(agent["instructions"].(string), "Custom  instructions") || body["input"] != nil || body["vault_ids"] != nil {
				t.Fatal(body)
			}
			response = `{"id":"session","environment":{"type":"openai_hosted","network":{"access":"disabled"}}}`
		case "/v1/agents/sessions/session/events":
			found = true
			data, err := io.ReadAll(req.Body)
			if err != nil || !strings.Contains(string(data), "Fetch /workspace/input.tar.gz and upload /workspace/outputs/output.tar.gz") {
				t.Fatal(string(data), err)
			}
			response = `{}`
		default:
			t.Fatalf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	set, err := Load(Config{Type: "openai", Prompts: rendered, BaseURL: "https://kioku.test", OpenAIAPIKey: "key", OpenAIModel: "openai-model"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(t.Context())
	r, err := set.New(model.Job{})
	if err != nil {
		t.Fatal(err)
	}
	job := prompts.JobVariables{JobID: "job", Namespace: "acme/project", InputURL: "/workspace/input.tar.gz", OutputURL: "/workspace/outputs/output.tar.gz"}
	_, err = r.Start(t.Context(), runner.RunSpec{Key: runner.AttemptKey{JobID: job.JobID, ProjectID: "internal-project", AttemptID: "attempt"},
		InputArchive: []byte("fixture"), SaveRef: func(runner.RunRef) error { return nil },
		FirstMessage: rendered.FileUser(job), BudgetCents: 50})
	if err != nil || !found {
		t.Fatal(err, found)
	}
}

type promptTransport func(*http.Request) (*http.Response, error)

func (f promptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
