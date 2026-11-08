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
	if _, ok := r.(*openairunner.Runner); !ok {
		t.Fatalf("%T", r)
	}
	for _, change := range []func(*Config){func(c *Config) { c.OpenAIAPIKey = "" }, func(c *Config) { c.OpenAIModel = "" }, func(c *Config) { c.BaseURL = "http://kioku.test" }, func(c *Config) { c.BaseURL = "https://kioku.test:8080" }} {
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
		response := `{"id":"vault"}`
		switch req.URL.Path {
		case "/v1/vaults":
		case "/v1/vaults/vault/credentials":
			response = `{"id":"credential"}`
		case "/v1/agents/sessions":
			found = true
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			agent := body["agent"].(map[string]any)
			if agent["model"] != "openai-model" || agent["instructions"] != "Custom https://kioku.test instructions" || body["input"] != "Fetch https://kioku.test/input and upload https://kioku.test/output" {
				t.Fatal(body)
			}
			response = `{"id":"session"}`
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
	job := prompts.JobVariables{JobID: "job", ProjectID: "project", InputURL: "https://kioku.test/input", OutputURL: "https://kioku.test/output"}
	_, err = r.Start(t.Context(), runner.RunSpec{Key: runner.AttemptKey{JobID: job.JobID, ProjectID: job.ProjectID, AttemptID: "attempt"}, Token: "job-token", InputURL: job.InputURL, OutputURL: job.OutputURL, FirstMessage: rendered.User(job), BudgetCents: 50})
	if err != nil || !found {
		t.Fatal(err, found)
	}
}

type promptTransport func(*http.Request) (*http.Response, error)

func (f promptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
