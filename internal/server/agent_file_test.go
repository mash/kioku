package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/runners"
)

func TestClaudeFileGenerationRecoversAndValidatesSessionOutput(t *testing.T) {
	for _, tc := range []struct {
		name, scope, network, want string
	}{
		{name: "delayed file", scope: "session", network: "limited", want: "completed"},
		{name: "wrong session", scope: "other", network: "limited", want: "failed"},
		{name: "open network", network: "unrestricted", want: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, project := orgProject(t)
			var available atomic.Bool
			var uploads, sessions, downloads, cleanup atomic.Int32
			var metadata atomic.Value
			output := outputArchive(t, map[string]string{"wiki/index.md": "# Offline", "wiki/log.md": "# Log"})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch req.Method + " " + req.URL.Path {
				case "GET /v1/agents/agent":
					io.WriteString(w, `{"id":"agent","version":2,"execution_identity":{"type":"service_account"}}`)
				case "GET /v1/environments/env":
					fmt.Fprintf(w, `{"id":"env","config":{"type":"cloud","networking":{"type":%q,"allowed_hosts":[]}}}`, tc.network)
				case "POST /v1/files":
					uploads.Add(1)
					file, header, err := req.FormFile("file")
					if err != nil {
						t.Error(err)
						http.Error(w, "bad upload", 400)
						return
					}
					defer file.Close()
					raw, err := io.ReadAll(file)
					if err != nil || !strings.HasPrefix(header.Filename, "kioku-input-") ||
						req.FormValue("expires_in_seconds") == "" ||
						!strings.Contains(req.Header.Get("Anthropic-Beta"), "files-api-2025-04-14") {
						t.Error("missing file data or upload policy", err)
					}
					if job := readInput(t, raw)["JOB.md"]; strings.Contains(job, "http") || strings.Contains(job, "input:") ||
						!strings.Contains(job, "namespace: "+project.Namespace) || strings.Contains(job, project.ID) {
						t.Error("job metadata exposed an HTTP endpoint")
					}
					io.WriteString(w, `{"id":"input"}`)
				case "POST /v1/sessions":
					sessions.Add(1)
					var body map[string]json.RawMessage
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					metadata.Store(body["metadata"])
					if strings.Contains(string(body["metadata"]), project.ID) || body["vault_ids"] != nil || !strings.Contains(string(body["resources"]), `"file_id":"input"`) ||
						!strings.Contains(string(body["resources"]), `"mount_path":"/input.tar.gz"`) ||
						!strings.Contains(string(body["initial_events"]), "/mnt/session/uploads/input.tar.gz") {
						t.Error("wrong Claude file session payload")
					}
					io.WriteString(w, `{"id":"session"}`)
				case "GET /v1/sessions":
					io.WriteString(w, `{"data":[]}`)
				case "GET /v1/sessions/session":
					fmt.Fprintf(w, `{"id":"session","status":"idle","metadata":%s}`, metadata.Load())
				case "GET /v1/sessions/session/events":
					io.WriteString(w, `{"data":[{"id":"event","type":"session.status_idle","stop_reason":{"type":"end_turn"}}]}`)
				case "GET /v1/files":
					if req.URL.Query().Get("scope_id") != "session" {
						t.Error("output listing is not session scoped")
					}
					if !available.Load() {
						io.WriteString(w, `{"data":[]}`)
						return
					}
					fmt.Fprintf(w, `{"data":[{"id":"output","filename":"output.tar.gz","downloadable":true,"scope":{"id":%q,"type":"session"}}]}`, tc.scope)
				case "GET /v1/files/output/content":
					downloads.Add(1)
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Write(output)
				case "DELETE /v1/sessions/session", "DELETE /v1/files/input":
					cleanup.Add(1)
					io.WriteString(w, `{}`)
				default:
					t.Errorf("unexpected provider request %s %s", req.Method, req.URL.Path)
					http.NotFound(w, req)
				}
			}))
			defer remote.Close()
			f.runnerConfig = runners.Config{AnthropicAPIKey: "synthetic", MAAgentID: "agent", MAEnvironmentID: "env",
				ManagedAgents: claudemanaged.NewAPI(remote.URL, "synthetic", remote.Client()), BaseURL: f.s.Config.BaseURL}
			f.setRunners()
			j := managedJob(t, f, token, project.ID, true)
			if err := f.s.startAgentJob(t.Context(), j); err != nil {
				t.Fatal(err)
			}
			j = storedJob(t, f, j.ID)
			if tc.network == "unrestricted" {
				if j.Status != tc.want || uploads.Load() != 0 || sessions.Load() != 0 {
					t.Fatal(j, uploads.Load(), sessions.Load())
				}
				return
			}
			if j.Run.TokenID != "" || !j.Run.Ref.UsesProviderFiles || j.Run.Ref.Resources["input_file_id"] != "input" {
				t.Fatal(j.Run)
			}
			// Recover the saved session ID as if kiokud stopped before recording
			// the transition to agent_running.
			if err := f.db.Update(func(st *model.State) error {
				current := st.Jobs[j.ID]
				current.Run.Phase = model.PhasePreparing
				st.Jobs[j.ID] = current
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			j = reconcileManaged(t, f, j)
			if j.Status != "running" || j.Run.OutputWaitSince == nil || j.Run.Phase != model.PhaseAgentRunning {
				t.Fatal(j)
			}
			available.Store(true)
			j = reconcileManaged(t, f, j)
			if j.Status != tc.want || !j.Run.CleanupComplete || cleanup.Load() != 2 {
				t.Fatal(j, cleanup.Load())
			}
			if (j.Revision != "") != (tc.want == "completed") || (downloads.Load() == 1) != (tc.want == "completed") {
				t.Fatal("published or downloaded output from the wrong session")
			}
		})
	}
}
