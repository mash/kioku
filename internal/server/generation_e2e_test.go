package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/runner/claudemanaged/fake"
	"github.com/mash/kioku/internal/runners"
)

func (e *e2e) waitJob(token, namespace, id string, tick func()) model.Job {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var job model.Job
	for time.Now().Before(deadline) {
		if tick != nil {
			tick()
		}
		e.expect(e.call("GET", "/api/projects/"+namespace+"/jobs/"+id, token, nil), 200, &job)
		if job.Status == "completed" || job.Status == "failed" {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("job did not finish: %+v", job)
	return job
}
func (e *e2e) claudeWebhook(id string) {
	e.t.Helper()
	body := []byte(fmt.Sprintf(`{"id":"event_test","type":"event","data":{"type":"session.status_idled","id":%q}}`, id))
	req, err := http.NewRequest("POST", e.url+"/webhooks/anthropic", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, webhookTestSecret)
	mac.Write([]byte("evt_test." + stamp + "."))
	mac.Write(body)
	req.Header.Set("Webhook-Id", "evt_test")
	req.Header.Set("Webhook-Timestamp", stamp)
	req.Header.Set("Webhook-Signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	e.expect(e.do(req), 204, nil)
}
func TestE2EClaudeFileGeneration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"success", "invalid output", "execution failed"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ma := fake.New()
			t.Cleanup(ma.Close)
			var sessions sync.Map
			output := outputArchive(t, map[string]string{"wiki/index.md": "# Provider files", "wiki/log.md": "# Log"})
			if mode == "invalid output" {
				output = []byte("invalid archive")
			}
			ma.OnSession(func(id string) {
				ma.SetOutput(id, output)
				reason := "end_turn"
				if mode == "execution failed" {
					reason = "retries_exhausted"
				}
				ma.SetStatus(id, "idle", reason, 40)
				sessions.Store(id, true)
			})
			e := newE2E(t, func(s *Server, c *runners.Config) {
				c.AnthropicAPIKey, c.MAAgentID, c.MAEnvironmentID = "fake-key", "agent", "environment"
				c.ManagedAgents = claudemanaged.NewAPI(ma.URL(), "fake-key", ma.HTTP.Client())
				s.Config.AnthropicWebhookSigningKey = "whsec_" + base64.StdEncoding.EncodeToString(webhookTestSecret)
			})
			token := e.login("owner")
			ns := e.project(token, "file-generation")
			base := "/api/projects/" + ns
			e.expect(e.call("DELETE", base+"/views/project-map", token, nil), 204, nil)
			e.expect(e.call("POST", base+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/source"}), 202, nil)
			var queued model.Job
			e.expect(e.call("POST", base+"/generate", token, nil), 202, &queued)
			tick := func() { sessions.Range(func(id, _ any) bool { e.claudeWebhook(id.(string)); return true }) }
			job := e.waitJob(token, ns, queued.ID, tick)
			want := "completed"
			if mode != "success" {
				want = "failed"
			}
			if job.Status != want || (job.Revision != "") != (mode == "success") {
				t.Fatalf("result: %+v", job)
			}
			// Replaying a valid webhook cannot create another run or change its result.
			tick()
			var after model.Job
			e.expect(e.call("GET", base+"/jobs/"+job.ID, token, nil), 200, &after)
			if after.Status != job.Status || after.Revision != job.Revision {
				t.Fatalf("replay changed result: %+v", after)
			}
			sessions.Range(func(id, _ any) bool {
				if !ma.Session(id.(string)).Deleted {
					t.Error("provider session not deleted")
				}
				return true
			})
			for _, req := range ma.Requests() {
				if strings.Contains(req.Path, "vault") {
					t.Fatal("vault request", req.Path)
				}
				if req.Path == "/v1/sessions" && req.Method == "POST" && req.Body["vault_ids"] != nil {
					t.Fatal("job credential sent to provider")
				}
			}
		})
	}
}
func TestE2EExecGeneration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"success", "failure", "upload-failure", "view-failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			e := newE2E(t, func(_ *Server, c *runners.Config) {
				c.Type = "exec"
				c.Command = []string{os.Args[0], "-test.run=^TestExecRunnerHelper$", "--", mode}
			})
			token := e.login("owner")
			ns := e.project(token, "exec-generation")
			base := "/api/projects/" + ns
			e.expect(e.call("POST", base+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/source"}), 202, nil)
			var queued model.Job
			e.expect(e.call("POST", base+"/generate", token, nil), 202, &queued)
			job := e.waitJob(token, ns, queued.ID, nil)
			want := "completed"
			if mode == "failure" {
				want = "failed"
			}
			if job.Status != want {
				t.Fatalf("result: %+v", job)
			}
			if mode == "view-failure" && (len(job.Views) != 1 || job.Views[0].Status != "failed" || job.Views[0].Error != "View の生成処理に失敗しました") {
				t.Fatalf("view failure: %+v", job)
			}
			wantCost := int64(137)
			if mode == "failure" || mode == "upload-failure" {
				wantCost = 23
			}
			if job.Run == nil || job.Run.CostCents == nil || *job.Run.CostCents != wantCost {
				t.Fatalf("usage: %+v", job.Run)
			}
			if strings.Contains(job.Error, "stderr-private") {
				t.Fatal("process stderr leaked")
			}
			e.expect(e.call("GET", base+"/jobs/"+job.ID+"/agent/input", token, nil), 403, nil)
		})
	}
}
