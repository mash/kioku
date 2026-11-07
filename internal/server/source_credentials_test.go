package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

type tokenIssue struct {
	key          runner.AttemptKey
	installation int64
	request      GitHubTokenRequest
}
type GitHubTokenRequest struct {
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
}

type fakeGitHubTokens struct {
	owner                              string
	repositories                       []GitHubRepository
	installations                      []GitHubInstallation
	issues                             []tokenIssue
	revoked                            []runner.AttemptKey
	failResolve, failIssue, failRevoke bool
}

func (f *fakeGitHubTokens) Installations(_ context.Context, owner string, repos []GitHubRepository) ([]GitHubInstallation, error) {
	f.owner = owner
	f.repositories = repos
	if f.failResolve {
		return nil, errors.New("provider response contains github-secret")
	}
	return f.installations, nil
}
func (f *fakeGitHubTokens) Issue(_ context.Context, key runner.AttemptKey, scope githubapp.AuthorizedRepositorySet) (runner.Secret, error) {
	f.issues = append(f.issues, tokenIssue{key, scope.InstallationID, GitHubTokenRequest{RepositoryIDs: scope.RepositoryIDs, Permissions: githubapp.Permissions()}})
	if f.failIssue {
		return "", errors.New("ambiguous issuance github-secret")
	}
	return runner.Secret(fmt.Sprintf("github-secret-%d", scope.InstallationID)), nil
}
func (f *fakeGitHubTokens) Revoke(_ context.Context, key runner.AttemptKey) error {
	f.revoked = append(f.revoked, key)
	if f.failRevoke {
		return errors.New("revocation github-secret")
	}
	return nil
}
func ownerIdentity(t *testing.T, f *fixture, pid string) string {
	t.Helper()
	var uid string
	f.db.View(func(st model.State) error {
		for id, role := range st.Projects[pid].Members {
			if role == "owner" {
				uid = id
			}
		}
		return nil
	})
	return uid
}
func connectGoogleForTest(t *testing.T, f *fixture, uid string, failRefresh bool) *int {
	t.Helper()
	calls := new(int)
	b, _ := json.Marshal(auth.Credential{AccessToken: "old-google-secret", RefreshToken: "refresh-google-secret", ExpiresAt: time.Now().Add(time.Hour)})
	if err := f.db.Update(func(st *model.State) error {
		u := st.Users[uid]
		if u.Connections == nil {
			u.Connections = map[string]string{}
		}
		u.Connections["google"] = f.s.Cipher.Seal(string(b), uid+":google")
		st.Users[uid] = u
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.Config.Providers["google"] = auth.Provider{Name: "google", ClientID: "id", ClientSecret: "client-secret", Client: &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.URL.Host != "oauth2.googleapis.com" {
			t.Errorf("unexpected host %s", r.URL.Host)
		}
		r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh-google-secret" || r.Form.Get("grant_type") != "refresh_token" {
			t.Error("wrong refresh request")
		}
		code, body := 200, `{"access_token":"fresh-google-secret","expires_in":3600,"token_type":"Bearer"}`
		if failRefresh {
			code = 400
			body = `{"error":"invalid_grant","error_description":"fresh-google-secret"}`
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	return calls
}
func TestOwnerSourceCredentialsStayInVaultAndRevokeByAttempt(t *testing.T) {
	f, owner, p, ma := managedFixture(t)
	uid := ownerIdentity(t, f, p.ID)
	calls := connectGoogleForTest(t, f, uid, false)
	member := f.login("member@example.com")
	f.db.Update(func(st *model.State) error {
		q := st.Projects[p.ID]
		q.Members[st.Sessions[auth.Hash(member)].UserID] = "member"
		st.Projects[p.ID] = q
		return nil
	})
	provider := &fakeGitHubTokens{installations: []GitHubInstallation{{ID: 11, Account: "Acme", Repositories: []GitHubRepository{{Account: "Acme", Name: "docs", ID: 101}, {Account: "Acme", Name: "unsubmitted", ID: 999}}}, {ID: 12, Account: "Other", Repositories: []GitHubRepository{{Account: "Other", Name: "notes", ID: 102}}}}}
	f.s.GitHubTokens = provider
	for _, u := range []string{"https://github.com/Acme/docs/blob/main/a.md", "https://github.com/Other/notes/issues/1", "https://docs.google.com/document/d/private/edit"} {
		if c := f.request("POST", "/api/projects/"+p.ID+"/put", member, model.PutRequest{URL: u}, nil); c != 202 {
			t.Fatal(c)
		}
	}
	putFile(t, f, member, p.ID, "submitted.md", "Verbatim body")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	j := startManaged(t, f, member, p.ID, false)
	if *calls != 1 || provider.owner != uid || len(provider.issues) != 2 {
		t.Fatal("did not use owner credentials", *calls, provider.owner, provider.issues)
	}
	for _, issue := range provider.issues {
		if len(issue.request.RepositoryIDs) != 1 || issue.request.RepositoryIDs[0] == 999 || !reflect.DeepEqual(issue.request.Permissions, githubapp.Permissions()) {
			t.Fatal(issue)
		}
	}
	seen := map[string]bool{}
	for _, req := range ma.Requests() {
		if strings.HasSuffix(req.Path, "/credentials") {
			a := req.Body["auth"].(map[string]any)
			name := a["secret_name"].(string)
			seen[name] = true
			if strings.Contains(fmt.Sprint(a), "refresh-google-secret") {
				t.Fatal("refresh token in vault")
			}
		}
	}
	for _, name := range []string{"KIOKU_JOB_TOKEN", "GITHUB_TOKEN_ACME", "GITHUB_TOKEN_OTHER", "GOOGLE_ACCESS_TOKEN"} {
		if !seen[name] {
			t.Fatal("missing vault credential", name)
		}
	}
	raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
	_, input := agentRequest(t, f, j, raw, "GET", "input", nil)
	files := readInput(t, input)
	var api map[string]any
	f.request("GET", "/api/projects/"+p.ID+"/jobs/"+j.ID, owner, nil, &api)
	encoded, _ := json.Marshal(api)
	for _, value := range []string{"github-secret", "fresh-google-secret", "old-google-secret", "refresh-google-secret"} {
		if strings.Contains(fmt.Sprint(files), value) || bytes.Contains(encoded, []byte(value)) || strings.Contains(logs.String(), value) {
			t.Fatal("credential leaked", value)
		}
	}
	sendOutput(t, f, ma, j, "Mixed results")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	provider.failRevoke = true
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" || j.Run.CleanupComplete || len(provider.revoked) != 1 {
		t.Fatal("failed revocation forgotten", j.Run)
	}
	restartManagedFixture(t, f)
	f.s.GitHubTokens = nil // Simulate a missing revocation dependency.
	j = reconcileManaged(t, f, j)
	if j.Run.CleanupComplete {
		t.Fatal("missing provider forgot revocation")
	}
	f.s.GitHubTokens = provider
	provider.failRevoke = false
	j = reconcileManaged(t, f, j)
	if !j.Run.CleanupComplete || len(provider.revoked) != 2 || provider.revoked[0] != attempt(j).Key {
		t.Fatal("revocation not retried", j.Run)
	}
	if strings.Contains(logs.String(), "github-secret") {
		t.Fatal("provider error leaked")
	}
	var snapshot model.Snapshot
	if c := f.request("GET", "/api/projects/"+p.ID+"/snapshot", owner, nil, &snapshot); c != 200 {
		t.Fatal(c)
	}
	if !strings.Contains(snapshot.Files["wiki/log.md"], "https://github.com/Acme/docs/blob/main/a.md") || !strings.Contains(snapshot.Files["wiki/log.md"], "submitter:") || !strings.Contains(snapshot.Files["wiki/log.md"], "reason:") {
		t.Fatal("missing per-item fetch failure")
	}
	for _, body := range snapshot.Files {
		if strings.Contains(body, "github-secret") || strings.Contains(body, "fresh-google-secret") || strings.Contains(body, "refresh-google-secret") {
			t.Fatal("credential in published wiki")
		}
	}

	f.db.View(func(st model.State) error {
		if len(st.Inbox) != 0 {
			t.Fatal(st.Inbox)
		}
		return nil
	})
}
func TestMissingAndFailedConnectionsOmitCredentials(t *testing.T) {
	for _, failure := range []string{"absent", "refresh", "resolve", "issue"} {
		t.Run(failure, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			if failure == "refresh" {
				connectGoogleForTest(t, f, ownerIdentity(t, f, p.ID), true)
			}
			var provider *fakeGitHubTokens
			if failure == "resolve" || failure == "issue" {
				provider = &fakeGitHubTokens{failResolve: failure == "resolve", failIssue: failure == "issue", installations: []GitHubInstallation{{ID: 1, Account: "acme", Repositories: []GitHubRepository{{Account: "acme", Name: "private", ID: 1}}}}}
				f.s.GitHubTokens = provider
			}
			for _, u := range []string{"https://github.com/acme/private/blob/main/a.md", "https://docs.google.com/document/d/private/edit"} {
				f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{URL: u}, nil)
			}
			j := startManaged(t, f, token, p.ID, false)
			count := 0
			for _, req := range ma.Requests() {
				if strings.HasSuffix(req.Path, "/credentials") {
					count++
				}
			}
			if count != 1 {
				t.Fatal("unexpected source credential", count)
			}
			sendOutput(t, f, ma, j, "Failed fetches")
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			j = reconcileManaged(t, f, j)
			if j.Status != "completed" || !j.Run.CleanupComplete {
				t.Fatal(j)
			}
			if provider != nil && len(provider.revoked) != 1 {
				t.Fatal("ambiguous tokens not revoked")
			}
		})
	}
}
func TestGitHubTokensAreSplitAndRestrictedToInboxRepositories(t *testing.T) {
	provider := &fakeGitHubTokens{}
	var requested []GitHubRepository
	installation := GitHubInstallation{ID: 10, Account: "acme"}
	for i := range 1001 {
		repo := GitHubRepository{Account: "acme", Name: fmt.Sprintf("repo-%d", i), ID: int64(i + 1)}
		requested = append(requested, repo)
		installation.Repositories = append(installation.Repositories, repo)
	}
	installation.Repositories = append(installation.Repositories, GitHubRepository{Account: "acme", Name: "unrequested", ID: 9999})
	provider.installations = []GitHubInstallation{installation}
	s := &Server{GitHubTokens: provider}
	credentials, _, err := s.githubCredentials(t.Context(), "owner", runner.AttemptKey{JobID: "job", AttemptID: "attempt"}, 0, requested)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 3 || len(provider.issues) != 3 {
		t.Fatal(len(credentials), provider.issues)
	}
	for i, issue := range provider.issues {
		want := 500
		if i == 2 {
			want = 1
		}
		if len(issue.request.RepositoryIDs) != want {
			t.Fatal(issue)
		}
		for _, id := range issue.request.RepositoryIDs {
			if id == 9999 {
				t.Fatal("scope expanded")
			}
		}
	}
	if credentials[1].Name != "GITHUB_TOKEN_ACME_2" || credentials[2].Name != "GITHUB_TOKEN_ACME_3" {
		t.Fatal("ambiguous placeholders")
	}
}

func TestExecDoesNotAcquireSourceCredentialsAndPublishesFetchFailures(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	calls := connectGoogleForTest(t, f, ownerIdentity(t, f, p.ID), false)
	provider := &fakeGitHubTokens{}
	f.s.GitHubTokens = provider
	var captured runner.RunSpec
	f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{
		start: func(_ context.Context, spec runner.RunSpec) (runner.RunRef, error) {
			captured = spec
			return runner.RunRef{ID: "exec-fixture"}, nil
		},
		inspect: func(context.Context, runner.Attempt) (runner.Status, error) {
			return runner.Status{State: runner.StateStopped}, nil
		},
		cleanup: func(context.Context, runner.Attempt) error { return nil },
	}})
	for _, u := range []string{"https://github.com/acme/private/blob/main/a.md", "https://docs.google.com/document/d/private/edit"} {
		f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{URL: u}, nil)
	}
	putFile(t, f, token, p.ID, "raw.md", "Original body")
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/removed"}, nil)
	j := startManaged(t, f, token, p.ID, false)
	if len(captured.SourceCredentials) != 0 || *calls != 0 || provider.owner != "" || len(ma.Requests()) != 0 {
		t.Fatal("exec acquired provider credentials")
	}
	_, input := agentRequest(t, f, j, string(captured.Token), "GET", "input", nil)
	files := readInput(t, input)
	if !strings.Contains(files["JOB.md"], "fetch_allowed: false") {
		t.Fatal(files["JOB.md"])
	}
	pages := map[string]string{"wiki/index.md": "# Wiki", "wiki/source-raw.md": "---\nsource_url: " + testSourceURL("raw.md", "Original body") + "\n---\nOriginal body", "wiki/log.md": "# Log\nExec has no vault; pending fetches failed.\n"}
	if code, body := agentRequest(t, f, j, string(captured.Token), "PUT", "output", outputArchive(t, pages)); code != 200 {
		t.Fatal(code, string(body))
	}
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal(j)
	}
	f.db.View(func(st model.State) error {
		if len(st.Inbox) != 0 {
			t.Fatal(st.Inbox)
		}
		return nil
	})
}
