package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
)

func (s *Server) runGitHubCleanup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.GitHubApp.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("GitHub cleanup deferred")
		}
		if err := s.DB.Update(func(st *model.State) error {
			prune(st)
			for uid, c := range st.GitHubAppConnections {
				if _, ok := st.Users[uid]; ok || c.Status != "inactive" {
					continue
				}
				used := false
				for _, a := range st.GitHubAppConnectAttempts {
					if a.UserID == uid {
						used = true
					}
				}
				for _, a := range st.GitHubAttemptAuthorizations {
					if a.OwnerID == uid {
						used = true
					}
				}
				for _, a := range st.GitHubTokenAttempts {
					if a.OwnerID == uid {
						used = true
					}
				}
				if !used {
					delete(st.GitHubAppConnections, uid)
				}
			}
			return nil
		}); err != nil && ctx.Err() == nil {
			slog.Warn("GitHub retention cleanup deferred")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) githubMetrics(w io.Writer) {
	_ = s.DB.View(func(st model.State) error {
		m := st.GitHubMetrics["cumulative"]
		keys := make([]string, 0, len(m.Counters))
		for k := range m.Counters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s %d\n", k, m.Counters[k])
		}
		for _, resource := range []string{"user", "installation"} {
			if n, ok := m.RateRemaining[resource]; ok {
				fmt.Fprintf(w, "kioku_github_rate_remaining_min{resource=%q} %d\n", resource, n)
			}
		}
		pending, unknown := 0, 0
		oldest, unknownAge, authorizationAge := 0.0, 0.0, 0.0
		now := time.Now()
		for _, v := range st.GitHubTokenAttempts {
			switch v.Status {
			case "revocation_pending":
				pending++
				oldest = max(oldest, now.Sub(v.RevocationRequestedAt).Seconds())
			case "issuance_unknown", "expiry_wait", "issuing":
				unknown++
				unknownAge = max(unknownAge, now.Sub(v.IssuanceStartedAt).Seconds())
			case "active":
				if !v.AuthorizationCheckedAt.IsZero() {
					authorizationAge = max(authorizationAge, now.Sub(v.AuthorizationCheckedAt).Seconds())
				}
			}
		}
		fmt.Fprintf(w, "kioku_github_cleanup_pending %d\nkioku_github_issuance_unknown %d\nkioku_github_cleanup_oldest_seconds %g\nkioku_github_issuance_unknown_age_seconds %g\nkioku_github_authorization_age_seconds %g\n", pending, unknown, oldest, unknownAge, authorizationAge)
		return nil
	})
}

// Fetch reports are model reports, not direct HTTP observations. Count only the
// current attempt's pinned item IDs, once in the publication transaction.
func countGitHubFetchReports(st *model.State, job model.Job, log string) {
	id := githubapp.Key(attempt(job).Key)
	a, ok := st.GitHubAttemptAuthorizations[id]
	if !ok || a.FetchCounted {
		return
	}
	type report struct{ Attempt, Item, Mode, Reason string }
	reports := map[string][]report{}
	for _, line := range strings.Split(log, "\n") {
		if !strings.HasPrefix(line, "KIOKU_GITHUB_FETCH ") {
			continue
		}
		var r report
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "KIOKU_GITHUB_FETCH ")), &r) != nil || r.Attempt != job.Run.Attempt.ID {
			continue
		}
		reports[r.Item] = append(reports[r.Item], r)
	}
	selection := map[string]model.GitHubRepositorySelection{}
	for _, v := range a.Selections {
		selection[v.Repository] = v
	}
	for _, itemID := range job.Run.InboxIDs {
		item := st.Inbox[itemID]
		repos, _ := pendingProviders([]model.InboxItem{item})
		if len(repos) != 1 {
			continue
		}
		v, ok := selection[repositoryName(repos[0])]
		if !ok {
			continue
		}
		reason := "unreported"
		r := reports[itemID]
		if len(r) == 1 && r[0].Mode == v.Mode {
			switch r[0].Reason {
			case "success", "403", "404", "expired", "other":
				reason = r[0].Reason
			}
		}
		githubapp.Count(st, fmt.Sprintf(`kioku_github_fetch_report_total{mode=%q,reason=%q}`, v.Mode, reason), 1)
	}
	a.FetchCounted = true
	st.GitHubAttemptAuthorizations[id] = a
}
