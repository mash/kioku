package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/url"
	"strings"
	"time"

	"github.com/mash/kioku/internal/runner"
)

type FileRunner struct{ *Runner }

func NewFiles(c Config) *FileRunner { return &FileRunner{Runner: New(c)} }

func (*FileRunner) Provider() string { return "openai" }

const openAIOutputPath = "/workspace/outputs/" + runner.OutputArchiveName

func (*FileRunner) FilePaths() (string, string) { return "/workspace/input.tar.gz", openAIOutputPath }

func (s resource) offline() bool {
	return s.Environment.Type == "openai_hosted" && s.Environment.Network.Access == "disabled" && len(s.VaultIDs) == 0
}

func (r *FileRunner) Start(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	if len(spec.InputArchive) > 50<<20 {
		return runner.RunRef{}, errors.New("OpenAI input archive exceeds the 50 MiB attachment limit")
	}
	if spec.BudgetCents < 1 || r.config.APIKey == "" || r.config.Model == "" || strings.TrimSpace(r.config.Prompt) == "" {
		return runner.RunRef{}, errors.New("invalid OpenAI runner configuration or job")
	}
	return runner.StartFileSession(ctx, spec, r.Provider(), r.uploadInput,
		func(ctx context.Context, id string, saveSession func(string) error) (string, error) {
			if !spec.Deadline.IsZero() && time.Now().After(spec.Deadline) {
				return "", errors.New("OpenAI job deadline exceeded")
			}
			var session resource
			input, _ := r.FilePaths()
			err := r.request(ctx, "POST", "/agents/sessions", map[string]any{
				"agent": map[string]any{"model": r.config.Model, "instructions": r.config.Prompt,
					"multi_agent": map[string]bool{"enabled": false}, "tools": []any{}},
				"environment": map[string]any{"type": "openai_hosted", "container_size": "small",
					"network": map[string]string{"access": "disabled"},
					"files":   []map[string]string{{"type": "file_id", "file_id": id, "path": input}}},
				"metadata": metadata(spec.Key), "spend_control": map[string]int64{"limit": spec.BudgetCents}, "stream": false,
			}, &session)
			if err != nil {
				return "", err
			}
			if err = saveSession(session.ID); err != nil {
				return "", err
			}
			path := "/agents/sessions/" + url.PathEscape(session.ID)
			if !session.offline() {
				var effective resource
				if err = r.request(ctx, "GET", path, nil, &effective); err != nil {
					return "", err
				}
				if effective.ID != session.ID || !effective.offline() {
					return "", errors.New("OpenAI session did not confirm network access disabled")
				}
			}
			// Submit input only after the provider confirms the effective network policy.
			err = r.request(ctx, "POST", path+"/events", map[string]any{"events": []any{map[string]any{
				"type": "agent.session.input.message",
				"input": []any{map[string]any{"role": "user", "content": []any{
					map[string]string{"type": "input_text", "text": spec.FirstMessage},
				}}},
			}}}, nil)
			return session.ID, err
		})
}

func (r *FileRunner) uploadInput(ctx context.Context, name string, data []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"purpose": "user_data", "expires_after[anchor]": "created_at", "expires_after[seconds]": "86400",
	} {
		if err := w.WriteField(name, value); err != nil {
			return "", err
		}
	}
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err = part.Write(data); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	res, err := r.send(ctx, "POST", "/files", &body, w.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var file fileResource
	if err = json.NewDecoder(res.Body).Decode(&file); err != nil {
		return "", fmt.Errorf("%w: invalid OpenAI file response", runner.ErrTemporary)
	}
	return file.ID, nil
}

type fileResource struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
	TurnID   string `json:"turn_id"`
	Bytes    int64  `json:"size_bytes"`
}

func (r *FileRunner) listFiles(ctx context.Context, path string, visit func(fileResource) error) error {
	cursor := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"limit": {"100"}}
		if cursor != "" {
			q.Set("after", cursor)
		}
		var page struct {
			Data    []fileResource `json:"data"`
			HasMore bool           `json:"has_more"`
			LastID  string         `json:"last_id"`
		}
		if err := r.request(ctx, "GET", path+"?"+q.Encode(), nil, &page); err != nil {
			return err
		}
		for _, file := range page.Data {
			if err := visit(file); err != nil {
				return err
			}
		}
		if !page.HasMore {
			return nil
		}
		if page.LastID == "" || seen[page.LastID] {
			return fmt.Errorf("%w: invalid OpenAI file pagination", runner.ErrTemporary)
		}
		cursor = page.LastID
		seen[cursor] = true
	}
}

func (r *FileRunner) CollectOutput(ctx context.Context, a runner.Attempt, status runner.Status, limit int64) ([]byte, error) {
	if a.Run.ID == "" || status.Run.ID != a.Run.ID || status.TurnID == "" || status.State != runner.StateStopped || status.Reason != "" {
		return nil, errors.New("OpenAI output requires a matching completed turn")
	}
	path := "/agents/sessions/" + url.PathEscape(a.Run.ID) + "/artifacts"
	var id string
	err := r.listFiles(ctx, path, func(file fileResource) error {
		if file.Path != openAIOutputPath || file.TurnID != status.TurnID {
			return nil
		}
		if id != "" || file.ID == "" {
			return errors.New("ambiguous OpenAI output archive")
		}
		if file.Bytes > limit {
			return errors.New("OpenAI output archive exceeds the compressed size limit")
		}
		id = file.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, runner.ErrOutputPending
	}
	res, err := r.send(ctx, "GET", path+"/"+url.PathEscape(id)+"/content", nil, "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return runner.ReadOutputArchive(res.Body, limit)
}

func (r *FileRunner) Cleanup(ctx context.Context, a runner.Attempt) error {
	if !a.Run.UsesProviderFiles {
		return r.Runner.Cleanup(ctx, a)
	}
	if a.Run.Resources["upload_started"] != "true" && a.Run.ID == "" {
		return nil
	}
	sessions, err := r.sessions(ctx, a)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		path := "/agents/sessions/" + url.PathEscape(session.ID)
		var ids []string
		err = r.listFiles(ctx, path+"/artifacts", func(file fileResource) error {
			if file.ID == "" {
				return errors.New("missing OpenAI artifact ID")
			}
			ids = append(ids, file.ID)
			return nil
		})
		if err != nil && !errors.Is(err, apiError(404)) {
			return err
		}
		for _, id := range ids {
			if err := r.delete(ctx, path+"/artifacts/"+url.PathEscape(id)); err != nil {
				return err
			}
		}
		if err := r.delete(ctx, path); err != nil {
			return err
		}
	}
	var inputs []string
	if id := a.Run.Resources["input_file_id"]; id != "" {
		inputs = []string{id}
	} else if a.Run.Resources["upload_started"] == "true" {
		name := runner.InputArchiveName(a.Key)
		err = r.listFiles(ctx, "/files", func(file fileResource) error {
			if file.Filename == name && file.ID != "" {
				inputs = append(inputs, file.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, id := range inputs {
		if err := r.delete(ctx, "/files/"+url.PathEscape(id)); err != nil {
			return err
		}
	}
	return nil
}

func (r *FileRunner) delete(ctx context.Context, path string) error {
	err := r.request(ctx, "DELETE", path, nil, nil)
	if errors.Is(err, apiError(404)) {
		return nil
	}
	return err
}
