package claudemanaged

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/runner"
)

func TestClaudeDiscoveryIncludesArchivedAndDeduplicatesKnownIDs(t *testing.T) {
	r, f, spec := fixture(t)
	first := start(t, r, spec)
	second := start(t, r, spec)
	f.SetStatus(first.ID, "idle", "end_turn", 412)
	f.SetStatus(second.ID, "idle", "end_turn", 88)
	archived := f.Session(second.ID)
	archived.Archived = true
	f.SetSession(archived)
	f.SetPageSize(1)
	status, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: first})
	if err != nil || status.State != runner.StateStopped || !status.Failed || status.Reason != "multiple Claude sessions for one attempt" || status.Usage.CostCents == nil || *status.Usage.CostCents != 500 {
		t.Fatalf("aggregate: %+v %v", status, err)
	}
	pages := 0
	for _, request := range f.Requests() {
		if request.Method == "GET" && request.Path == "/v1/sessions" {
			pages++
			if request.Query.Get("include_archived") != "true" {
				t.Fatal("archived sessions omitted")
			}
		}
	}
	if pages != 2 {
		t.Fatalf("pages=%d", pages)
	}
	// One unknown sample makes only its cost unknown, not a known 412 cents.
	archived.CostAmount = ""
	f.SetSession(archived)
	status, err = r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: first})
	if err != nil || status.Usage.CostCents != nil {
		t.Fatal("unknown cost was omitted", err)
	}
}
func TestUsageAggregatesOnlyAfterAllSessionsStop(t *testing.T) {
	r, f, spec := fixture(t)
	a := start(t, r, spec)
	b := start(t, r, spec)
	f.SetStatus(a.ID, "idle", "end_turn", 40)
	status, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: a})
	if err != nil || status.State != runner.StateRunning {
		t.Fatalf("published before all sessions stopped: %+v %v", status, err)
	}
	if err = r.Stop(t.Context(), runner.Attempt{Key: spec.Key, Run: a}); err != nil {
		t.Fatal(err)
	}
	if !f.Session(a.ID).Interrupted || !f.Session(b.ID).Interrupted {
		t.Fatal("Stop omitted a discovered session")
	}
}
func TestClaudeUsageFieldValidation(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		check func(runner.UsageSample) bool
	}{
		{`{"agent":{"model":{"id":"claude"}},"usage":{"input_tokens":0,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":5},"list_cost":{"amount":"9223372036854775807","currency":"USD"}}}`, func(s runner.UsageSample) bool {
			return s.Model != nil && *s.Model == "claude" && s.InputTokens != nil && *s.InputTokens == 0 && s.CacheCreationInputTokens != nil && *s.CacheCreationInputTokens == 9 && s.CostCents != nil && *s.CostCents == math.MaxInt64
		}},
		{`{"usage":{"input_tokens":9223372036854775808,"output_tokens":-1,"cache_read_input_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":9223372036854775807,"ephemeral_1h_input_tokens":1},"list_cost":{"amount":"42","currency":"EUR"}}}`, func(s runner.UsageSample) bool {
			return s.InputTokens == nil && s.OutputTokens == nil && s.CacheCreationInputTokens == nil && s.CostCents == nil && s.CacheReadInputTokens != nil && *s.CacheReadInputTokens == 2
		}},
		{`{"usage":{"input_tokens":"4","list_cost":{"amount":"9223372036854775808","currency":"USD"}}}`, func(s runner.UsageSample) bool { return s.InputTokens == nil && s.CostCents == nil }},
	} {
		s := parseUsage(tc.raw)
		if !tc.check(s) {
			t.Fatalf("invalid usage projection: %+v", s)
		}
	}
}
func TestClaudeAPIOutOfRangeUsageIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, `{"data":[],"has_more":false,"next_page":null}`)
			return
		}
		fmt.Fprint(w, `{"id":"s","status":"idle","usage":{"input_tokens":9223372036854775808,"output_tokens":0,"list_cost":{"amount":"0","currency":"USD"}}}`)
	}))
	defer srv.Close()
	a := NewAPI(srv.URL, "local-test", srv.Client())
	s, err := a.GetSession(t.Context(), "s")
	if err != nil || s.Usage.InputTokens != nil || s.Usage.OutputTokens == nil || *s.Usage.OutputTokens != 0 || s.Usage.CostCents == nil {
		t.Fatalf("invalid field failed entire session: %+v %v", s, err)
	}
}
