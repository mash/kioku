package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

func seedAppConnection(t *testing.T, f *fixture, uid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := githubapp.Config{Enabled: true, ID: 7, ClientID: "Iv.fixture", Slug: "app-fixture", ClientSecret: "client-secret", PrivateKey: key}
	f.s.Config.GitHubApp = config
	f.s.GitHubApp.Client.Config = config
	credential := githubapp.UserCredential{AccessToken: "app-user-secret", RefreshToken: "app-refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(30 * 24 * time.Hour)}
	b, _ := json.Marshal(credential)
	if err := f.db.Update(func(st *model.State) error {
		st.GitHubAppConnections[uid] = model.GitHubAppConnection{AppID: 7, GitHubUserID: 42, Login: "user", Version: 1, Status: "active", EncryptedCredential: f.s.Cipher.Seal(string(b), "github-app:"+uid), AccessExpiresAt: credential.AccessExpiresAt, RefreshExpiresAt: credential.RefreshExpiresAt, CredentialGeneration: 1, Installations: map[int64]model.GitHubAppInstallation{3: {ID: 3, AccountID: 9, Login: "acme", Status: "active"}, 4: {ID: 4, AccountID: 10, Login: "other", Status: "active"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestAppProviderUsesCurrentUserIntersectionAndIsolatesInstallationFailures(t *testing.T) {
	for _, failure := range []string{"none", "page two", "suspended", "inaccessible", "rate limit"} {
		t.Run(failure, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			uid := ownerIdentity(t, f, p.ID)
			seedAppConnection(t, f, uid)
			var issued []int64
			revokes := 0
			f.s.GitHubApp.Client.HTTP = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
				status, body := 200, ""
				h := http.Header{}
				switch r.URL.Path {
				case "/user/installations":
					body = `{"installations":[{"id":3,"app_id":7,"account":{"id":9,"login":"acme"}},{"id":4,"app_id":7,"account":{"id":10,"login":"other"}}]}`
					if failure == "inaccessible" {
						body = `{"installations":[{"id":4,"app_id":7,"account":{"id":10,"login":"other"}}]}`
					}
				case "/app/installations/3":
					body = `{"id":3,"app_id":7,"account":{"id":9,"login":"acme"},"permissions":{"contents":"read","metadata":"read","issues":"read"}}`
					if failure == "suspended" {
						body = strings.TrimSuffix(body, "}") + `,"suspended_at":"2026-01-01T00:00:00Z"}`
					}
				case "/app/installations/4":
					body = `{"id":4,"app_id":7,"account":{"id":10,"login":"other"},"permissions":{"contents":"read","metadata":"read","issues":"read"}}`
				case "/user/installations/3/repositories":
					body = `{"repositories":[{"id":101,"full_name":"Acme/Docs"},{"id":999,"full_name":"acme/unsubmitted"}]}`
					if failure == "page two" {
						if r.URL.Query().Get("page") == "1" {
							h.Set("Link", `<https://api.github.com/next>; rel="next"`)
						} else {
							status, body = 503, "app-user-secret"
						}
					}
					if failure == "rate limit" {
						status, body = 403, `{"message":"secondary rate limit app-user-secret"}`
					}
				case "/user/installations/4/repositories":
					body = `{"repositories":[{"id":102,"full_name":"other/notes"}]}`
				case "/app/installations/3/access_tokens", "/app/installations/4/access_tokens":
					var request GitHubTokenRequest
					_ = json.NewDecoder(r.Body).Decode(&request)
					issued = append(issued, request.RepositoryIDs...)
					if !reflect.DeepEqual(request.Permissions, githubapp.Permissions()) {
						t.Error("permissions")
					}
					body = fmt.Sprintf(`{"token":"installation-secret-%d","expires_at":%q}`, len(issued), time.Now().Add(time.Hour).Format(time.RFC3339))
				case "/installation/token":
					revokes++
					status = 204
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			for _, u := range []string{"https://github.com/acme/docs/issues/1", "https://github.com/acme/not-visible", "https://github.com/other/notes/pull/1"} {
				if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{URL: u}, nil); c != 202 {
					t.Fatal(c)
				}
			}
			j := startManaged(t, f, token, p.ID, false)
			want := []int64{102}
			if failure == "none" {
				want = []int64{101, 102}
			}
			if !reflect.DeepEqual(issued, want) {
				t.Fatal(issued, want)
			}
			_ = f.db.View(func(st model.State) error {
				a := st.GitHubAttemptAuthorizations[githubapp.Key(attempt(j).Key)]
				if len(a.Selections) != 3 {
					t.Fatal(a.Selections)
				}
				for _, v := range a.Selections {
					if v.Repository == "acme/docs" && failure != "none" && v.Mode != "blocked" {
						t.Fatal("unsafe fallback", v)
					}
					if v.Repository == "other/notes" && v.Mode != "token" {
						t.Fatal(v)
					}
				}
				b, _ := json.Marshal(st)
				if strings.Contains(string(b), "installation-secret") || strings.Contains(string(b), "app-user-secret") {
					t.Fatal("plaintext catalog")
				}
				return nil
			})
			sendOutput(t, f, ma, j, "App fetches")
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			j = reconcileManaged(t, f, j)
			if j.Status != "completed" || !j.Run.CleanupComplete || revokes != len(want) {
				t.Fatal(j.Status, j.Run.CleanupComplete, revokes)
			}
		})
	}
}
func TestGitHubOwnerChangeInvalidatesOnlyTransferredProjectAndBlocksPublication(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	memberToken := f.login("new-owner@example.com")
	uid := ownerIdentity(t, f, p.ID)
	var other model.Project
	if c := f.request("POST", "/api/projects", token, map[string]string{"name": "other-project"}, &other); c != 201 {
		t.Fatal(c)
	}
	_ = f.db.Update(func(st *model.State) error {
		v := st.Projects[p.ID]
		v.Members[st.Sessions[auth.Hash(memberToken)].UserID] = "member"
		st.Projects[p.ID] = v
		return nil
	})
	putFile(t, f, token, p.ID, "one.md", "one")
	putFile(t, f, token, other.ID, "two.md", "two")
	first := startManaged(t, f, token, p.ID, false)
	second := startManaged(t, f, token, other.ID, false)
	sendOutput(t, f, ma, first, "old-owner output")
	sendOutput(t, f, ma, second, "unchanged-owner output")
	if c := f.request("POST", "/api/projects/"+p.ID+"/owner", token, map[string]string{"target": "new-owner@example.com"}, nil); c != 200 {
		t.Fatal(c)
	}
	_ = f.db.View(func(st model.State) error {
		if validateGeneration(st, first) == nil || validateGeneration(st, second) != nil {
			t.Fatal("wrong invalidation scope")
		}
		if st.GitHubAppConnections[uid].Version != 0 {
			t.Fatal("transfer changed owner connection")
		}
		return nil
	})
	ma.SetStatus(first.Run.Ref.ID, "idle", "end_turn", 1)
	ma.SetStatus(second.Run.Ref.ID, "idle", "end_turn", 1)
	first = reconcileManaged(t, f, first)
	second = reconcileManaged(t, f, second)
	if first.Status != "failed" || second.Status != "completed" {
		t.Fatal(first.Status, second.Status)
	}
}
func TestGitHubFetchReportsCountOnceAndIgnoreOldEntries(t *testing.T) {
	st := model.NewState()
	key := runner.AttemptKey{ProjectID: "p", JobID: "j", AttemptID: "a"}
	j := model.Job{ID: "j", ProjectID: "p", Run: &model.AgentRun{Attempt: model.Attempt{ID: "a"}, InboxIDs: []string{"one", "two", "three"}}}
	for _, id := range j.Run.InboxIDs {
		st.Inbox[id] = model.InboxItem{ID: id, Status: "pending", SourceURL: "https://github.com/acme/docs"}
	}
	st.GitHubAttemptAuthorizations[githubapp.Key(key)] = model.GitHubAttemptAuthorization{Key: key, Selections: []model.GitHubRepositorySelection{{Repository: "acme/docs", Mode: "token", Reason: "authorized"}}}
	log := `KIOKU_GITHUB_FETCH {"attempt":"old","item":"one","mode":"token","reason":"403"}
KIOKU_GITHUB_FETCH {"attempt":"a","item":"one","mode":"token","reason":"success"}
KIOKU_GITHUB_FETCH {"attempt":"a","item":"two","mode":"token","reason":"404"}
KIOKU_GITHUB_FETCH {"attempt":"a","item":"two","mode":"token","reason":"404"}
KIOKU_GITHUB_FETCH {"attempt":"a","item":"three","mode":"unauthenticated","reason":"success"}`
	countGitHubFetchReports(&st, j, log)
	countGitHubFetchReports(&st, j, log)
	m := st.GitHubMetrics["cumulative"].Counters
	if m[`kioku_github_fetch_report_total{mode="token",reason="success"}`] != 1 || m[`kioku_github_fetch_report_total{mode="token",reason="unreported"}`] != 2 || len(m) != 2 {
		t.Fatal(m)
	}
}

func TestGitHubRevocationProgressesWhileRunnerStopIsBlocked(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	putFile(t, f, token, p.ID, "one.md", "one")
	j := startManaged(t, f, token, p.ID, false)
	uid := ownerIdentity(t, f, p.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{stop: func(ctx context.Context, _ runner.Attempt) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}})
	_ = f.db.Update(func(st *model.State) error {
		st.GitHubTokenAttempts["issued"] = model.GitHubTokenAttempt{Key: attempt(j).Key, OwnerID: uid, Status: "active", EncryptedToken: f.s.Cipher.Seal("revoke-fixture-secret", "github-token:issued"), ExpiresAt: time.Now().Add(time.Hour)}
		return nil
	})
	revoked := make(chan struct{}, 1)
	f.s.GitHubApp.Client.HTTP = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "DELETE" || r.URL.Path != "/installation/token" || r.Header.Get("Authorization") != "Bearer revoke-fixture-secret" {
			t.Error("unexpected cleanup request")
		}
		revoked <- struct{}{}
		return &http.Response{StatusCode: 204, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.failAgentJobLocked(ctx, j, "invalidated", nil) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("runner stop not entered")
	}
	if err := f.s.GitHubApp.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-revoked:
	case <-ctx.Done():
		t.Fatal("revocation waited for Stop")
	}
	cancel()
	<-done
}

func TestRenamedInstallationFailureDoesNotFallBack(t *testing.T) {
	f, token := appFixture(t)
	uid := loopbackSnapshot(t, f).Sessions[auth.Hash(token)].UserID
	seedAppConnection(t, f, uid)
	f.s.GitHubApp.Client.HTTP = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 503, "temporary failure"
		if r.URL.Path == "/user/installations" {
			status, body = 200, `{"installations":[{"id":3,"app_id":7,"account":{"id":9,"login":"renamed"}}]}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	credentials, selections, err := f.s.githubCredentials(t.Context(), uid, runner.AttemptKey{}, 1, []GitHubRepository{{Account: "renamed", Name: "docs"}})
	if err != nil || len(credentials) != 0 || len(selections) != 1 || selections[0].Mode != "blocked" || selections[0].Reason != "temporary" {
		t.Fatalf("renamed installation authorization failure: %v, %v", selections, err)
	}
}
