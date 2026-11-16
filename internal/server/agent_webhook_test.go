package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/store"
)

var webhookTestSecret = []byte("only-a-local-test-secret-32-bytes")

func signedAgentWebhook(t *testing.T, f *fixture, body []byte, at time.Time, key []byte, mutate func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequest("POST", f.url+"/webhooks/anthropic", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	id := "evt_test"
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + ts + "."))
	_, _ = mac.Write(body)
	req.Header.Set("Webhook-Id", id)
	req.Header.Set("Webhook-Timestamp", ts)
	req.Header.Set("Webhook-Signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	if mutate != nil {
		mutate(req)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
func webhookBody(j fixtureJob) []byte {
	return []byte(fmt.Sprintf(`{"id":"event_test","type":"event","data":{"type":"session.status_idled","id":%q}}`, j.Run.Ref.ID))
}
func TestManagedWebhookSignaturesAndReplay(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	f.s.Config.AnthropicWebhookSigningKey = "whsec_" + base64.StdEncoding.EncodeToString(webhookTestSecret)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Signed completion")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	body := webhookBody(j)
	before := len(ma.Requests())
	for _, tc := range []struct {
		name   string
		at     time.Time
		secret []byte
		mutate func(*http.Request)
	}{
		{"missing signature", time.Now(), webhookTestSecret, func(r *http.Request) { r.Header.Del("Webhook-Signature") }},
		{"forged", time.Now(), webhookTestSecret, func(r *http.Request) { r.Header.Set("Webhook-Signature", "v1,AAAA") }},
		{"other secret", time.Now(), []byte("different-key"), nil},
		{"old", time.Now().Add(-6 * time.Minute), webhookTestSecret, nil},
		{"future", time.Now().Add(6 * time.Minute), webhookTestSecret, nil},
		{"changed body", time.Now(), webhookTestSecret, func(r *http.Request) {
			r.Body = io.NopCloser(bytes.NewReader(bytes.Replace(body, []byte("event_test"), []byte("event_best"), 1)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := signedAgentWebhook(t, f, body, tc.at, tc.secret, tc.mutate); c != 400 {
				t.Fatal(c)
			}
		})
	}
	if len(ma.Requests()) != before || storedJob(t, f, j.ID).Status != "running" {
		t.Fatal("invalid webhook reconciled")
	}
	if c := signedAgentWebhook(t, f, bytes.Repeat([]byte("x"), 64<<10+1), time.Now(), webhookTestSecret, nil); c != 400 {
		t.Fatal(c)
	}
	for _, b := range [][]byte{[]byte(`{"data":{"type":"session.status_idled","id":"unknown"}}`), []byte(`{"data":{"type":"session.running","id":"unknown"}}`)} {
		if c := signedAgentWebhook(t, f, b, time.Now(), webhookTestSecret, nil); c != 204 {
			t.Fatal(c)
		}
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c := signedAgentWebhook(t, f, body, time.Now(), webhookTestSecret, func(r *http.Request) {
				r.Header.Set("Webhook-Signature", "v1,invalid "+r.Header.Get("Webhook-Signature"))
			}); c != 204 {
				t.Error(c)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if got := storedJob(t, f, j.ID); got.WikiStatus != "completed" {
		t.Fatal(got)
	}
	assertAgentCleanup(t, f, ma, j)
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if cost := costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents); cost != 123 {
			t.Error("cost", cost)
		}
		return nil
	})
}
func TestManagedWebhookDisabledAndTemporaryReconciliation(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Poll completion")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	if c := signedAgentWebhook(t, f, webhookBody(j), time.Now(), webhookTestSecret, nil); c != 404 {
		t.Fatal(c)
	}
	f.s.Config.AnthropicWebhookSigningKey = "whsec_" + base64.StdEncoding.EncodeToString(webhookTestSecret)
	ma.SetError("GET", "/v1/sessions/"+j.Run.Ref.ID, 503)
	if c := signedAgentWebhook(t, f, webhookBody(j), time.Now(), webhookTestSecret, nil); c != 204 {
		t.Fatal(c)
	}
	if storedJob(t, f, j.ID).Status != "running" {
		t.Fatal("temporary error failed job")
	}
	ma.SetError("GET", "/v1/sessions/"+j.Run.Ref.ID, 0)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
}

type webhookFailedCatalog struct {
	catalog
	err error
}

func (d *webhookFailedCatalog) Read(ctx context.Context, fn func(*store.Queries) error) error {
	return d.err
}

func TestWebhookReportsFatalDatabaseFailure(t *testing.T) {
	t.Parallel()
	for _, fatal := range []bool{false, true} {
		t.Run(strconv.FormatBool(fatal), func(t *testing.T) {
			f, _, _, _ := managedFixture(t)
			f.s.Config.AnthropicWebhookSigningKey = "whsec_" + base64.StdEncoding.EncodeToString(webhookTestSecret)
			err := errors.New("injected read failure")
			var failure error = err
			if fatal {
				failure = &store.FatalDatabaseError{Cause: err}
			}
			f.s.Store = &webhookFailedCatalog{catalog: f.stateStore, err: failure}
			body := []byte(`{"data":{"type":"session.status_idled","id":"session"}}`)
			if code := signedAgentWebhook(t, f, body, time.Now(), webhookTestSecret, nil); code != 204 {
				t.Fatal(code)
			}
			if stopped := f.s.fatalError.Load() != nil; stopped != fatal {
				t.Fatalf("fatal=%t: server stopped=%t", fatal, stopped)
			}
		})
	}
}
