package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestViewGuideIsAvailableWithoutProjectOrCredentials(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"-C", "/nonexistent-kioku-project", "view", "rules"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "summary.md", "data.json", "Allowed HTML elements", "Global attributes", "Wiki page", "180 bytes per archive path"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rules missing %q", want)
		}
	}
}

func TestViewCLISavesPromptOverExistingAPI(t *testing.T) {
	const prompt = "進捗を表示する\n優先順に並べる。\n"
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner" {
			t.Error("missing owner credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requests <- r.Method + " " + r.URL.Path
		if r.Method == http.MethodPatch {
			var patch map[string]string
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil || len(patch) != 1 || patch["prompt"] != prompt {
				t.Errorf("unexpected PATCH: %v, %v", patch, err)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "view prompt.md")
	if err := os.WriteFile(path, []byte(prompt), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	app := &App{
		ctx: t.Context(), Config: Config{Server: server.URL, Namespace: "acme/platform", Token: "owner"},
		kind: credentialHuman, Client: server.Client(), Out: &out, Err: &stderr,
	}
	for _, args := range [][]string{{"view", "list"}, {"view", "show", "project-map"}, {"view", "set", "--prompt-file", path, "project-map"}} {
		if err := app.dispatch(args); err != nil {
			t.Fatal(err)
		}
	}
	const base = "/api/projects/acme/platform/views"
	for _, want := range []string{"GET " + base, "GET " + base + "/project-map", "PATCH " + base + "/project-map"} {
		select {
		case got := <-requests:
			if got != want {
				t.Errorf("request %q, want %q", got, want)
			}
		default:
			t.Fatalf("missing request %q", want)
		}
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'"'}, model.MaxJSONRequestBytes/2), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.dispatch([]string{"view", "set", "--prompt-file", path, "project-map"}); err == nil ||
		!strings.Contains(err.Error(), "encoded View settings") || len(requests) != 0 {
		t.Fatalf("oversized JSON: got error %v and %d extra requests", err, len(requests))
	}
	if err := app.dispatch([]string{"view", "set", "--prompt-file", path, "../generate"}); err == nil || len(requests) != 0 {
		t.Fatalf("invalid slug: got error %v and %d extra requests", err, len(requests))
	}
}
