package openai

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mash/kioku/internal/runner"
)

func TestFileSessionUsesOnlyCompletedTurnArtifacts(t *testing.T) {
	key := runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}
	var downloads, deletes atomic.Int32
	var duplicate atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.Method + " " + req.URL.Path {
		case "POST /files":
			f, header, err := req.FormFile("file")
			if err != nil {
				t.Error(err)
				http.Error(w, "bad input", 400)
				return
			}
			defer f.Close()
			body, err := io.ReadAll(f)
			if err != nil || string(body) != "input" || header.Filename != runner.InputArchiveName(key) ||
				req.FormValue("purpose") != "user_data" || req.FormValue("expires_after[seconds]") == "" {
				t.Error("unexpected attachment")
			}
			io.WriteString(w, `{"id":"input"}`)
		case "POST /agents/sessions":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			env := string(body["environment"])
			if body["input"] != nil || body["vault_ids"] != nil || !strings.Contains(env, `"access":"disabled"`) ||
				!strings.Contains(env, `"file_id":"input"`) {
				t.Error("session was not configured offline")
			}
			io.WriteString(w, `{"id":"session"}`)
		case "GET /agents/sessions/session":
			json.NewEncoder(w).Encode(map[string]any{"id": "session", "metadata": metadata(key), "status": "idle",
				"environment": map[string]any{"type": "openai_hosted", "network": map[string]string{"access": "disabled"}}})
		case "POST /agents/sessions/session/events":
			io.WriteString(w, `{}`)
		case "GET /agents/sessions/session/turns":
			io.WriteString(w, `{"data":[{"id":"completed","status":"completed"}]}`)
		case "GET /agents/sessions/session/artifacts":
			if req.URL.Query().Get("after") == "" {
				io.WriteString(w, `{"data":[{"id":"old","turn_id":"previous","path":"/workspace/outputs/output.tar.gz"}],"has_more":true,"last_id":"old"}`)
				return
			}
			if duplicate.Load() {
				io.WriteString(w, `{"data":[{"id":"duplicate","turn_id":"completed","path":"/workspace/outputs/output.tar.gz"},{"id":"output","turn_id":"completed","path":"/workspace/outputs/output.tar.gz"}]}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"output","turn_id":"completed","path":"/workspace/outputs/output.tar.gz"}]}`)
		case "GET /agents/sessions/session/artifacts/output/content":
			downloads.Add(1)
			io.WriteString(w, "output")
		case "DELETE /agents/sessions/session/artifacts/old", "DELETE /agents/sessions/session/artifacts/output",
			"DELETE /agents/sessions/session", "DELETE /files/input":
			deletes.Add(1)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected provider call %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	r := NewFiles(Config{BaseURL: server.URL, HTTPClient: server.Client(), APIKey: "fake", Model: "test", Prompt: "test"})
	var saved []runner.RunRef
	ref, err := r.Start(t.Context(), runner.RunSpec{Key: key, BudgetCents: 50, FirstMessage: "test",
		InputArchive: []byte("input"), SaveRef: func(ref runner.RunRef) error { saved = append(saved, ref); return nil }})
	if err != nil || len(saved) != 3 || saved[0].Resources["input_file_id"] != "" ||
		saved[1].Resources["input_file_id"] != "input" || saved[2].ID != "session" {
		t.Fatal(ref, saved, err)
	}
	a := runner.Attempt{Key: key, Run: ref}
	status, err := r.Inspect(t.Context(), a)
	if err != nil || status.State != runner.StateStopped || status.TurnID != "completed" {
		t.Fatal(status, err)
	}
	duplicate.Store(true)
	if _, err := r.CollectOutput(t.Context(), a, status, 32); err == nil || downloads.Load() != 0 {
		t.Fatal("accepted ambiguous artifacts", err)
	}
	duplicate.Store(false)
	body, err := r.CollectOutput(t.Context(), a, status, 32)
	if err != nil || string(body) != "output" || downloads.Load() != 1 {
		t.Fatal(string(body), err)
	}
	if err := r.Cleanup(t.Context(), a); err != nil || deletes.Load() != 4 {
		t.Fatal(err, deletes.Load())
	}
}

func TestFileSessionDoesNotStartWithoutNetworkConfirmation(t *testing.T) {
	key := runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method + " " + req.URL.Path {
		case "POST /files":
			io.WriteString(w, `{"id":"input"}`)
		case "POST /agents/sessions", "GET /agents/sessions/session":
			io.WriteString(w, `{"id":"session","environment":{"type":"openai_hosted","network":{"access":"enabled"}}}`)
		default:
			t.Errorf("unexpected provider call %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	r := NewFiles(Config{BaseURL: server.URL, HTTPClient: server.Client(), APIKey: "fake", Model: "test", Prompt: "test"})
	ref, err := r.Start(t.Context(), runner.RunSpec{Key: key, BudgetCents: 50, FirstMessage: "test",
		InputArchive: []byte("input"), SaveRef: func(runner.RunRef) error { return nil }})
	if err == nil || errors.Is(err, runner.ErrTemporary) || ref.ID != "session" {
		t.Fatal(ref, err)
	}
}
