package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

// GitHubTokenProvider issues only authorized, nonempty repo scopes. Known tokens
// are revoked durably; lost issuance responses must wait for conservative expiry.
type GitHubTokenProvider interface {
	Installations(context.Context, string, []GitHubRepository) ([]GitHubInstallation, error)
	Issue(context.Context, runner.AttemptKey, githubapp.AuthorizedRepositorySet) (runner.Secret, error)
	Revoke(context.Context, runner.AttemptKey) error
}
type GitHubRepository struct {
	Account, Name string
	ID            int64
}
type GitHubInstallation struct {
	ID                       int64
	Account, OwnerID, Reason string
	ConnectionVersion        uint64
	CheckedAt                time.Time
	Repositories             []GitHubRepository
}

var githubAccount = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
var githubRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func repositoryName(repo GitHubRepository) string {
	return strings.ToLower(repo.Account + "/" + repo.Name)
}
func pendingProviders(items []model.InboxItem) ([]GitHubRepository, bool) {
	repos := map[string]GitHubRepository{}
	google := false
	for _, item := range items {
		if item.Status != "pending" {
			continue
		}
		u, err := url.Parse(item.SourceURL)
		if err != nil {
			continue
		}
		switch strings.ToLower(u.Hostname()) {
		case "github.com":
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 2 && githubAccount.MatchString(parts[0]) && githubRepository.MatchString(parts[1]) && parts[1] != "." && parts[1] != ".." {
				repo := GitHubRepository{Account: parts[0], Name: parts[1]}
				repos[repositoryName(repo)] = repo
			}
		case "docs.google.com":
			google = true
		}
	}
	out := make([]GitHubRepository, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return repositoryName(out[i]) < repositoryName(out[j]) })
	return out, google
}

func pinGitHubAuthorization(st *model.State, job model.Job) {
	ownerID := ""
	for uid, role := range st.Projects[job.ProjectID].Members {
		if role == "owner" {
			ownerID = uid
			break
		}
	}
	key := attempt(job).Key
	st.GitHubAttemptAuthorizations[githubapp.Key(key)] = model.GitHubAttemptAuthorization{Key: key, OwnerID: ownerID, ConnectionVersion: st.GitHubAppConnections[ownerID].Version}
}
func (s *Server) sourceCredentials(ctx context.Context, job *model.Job, items []model.InboxItem) ([]runner.Credential, []model.GitHubRepositorySelection, error) {
	startedAt := time.Now()
	var authorization model.GitHubAttemptAuthorization
	if err := s.DB.View(func(st model.State) error {
		authorization = st.GitHubAttemptAuthorizations[githubapp.Key(attempt(*job).Key)]
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if authorization.OwnerID == "" {
		return nil, nil, githubapp.ErrAuthorization
	}
	repos, google := pendingProviders(items)
	var credentials []runner.Credential
	var selections []model.GitHubRepositorySelection
	if len(repos) > 0 {
		if err := s.DB.Update(func(st *model.State) error {
			j := st.Jobs[job.ID]
			if !githubapp.Authorized(*st, authorization.Key, authorization.OwnerID, authorization.ConnectionVersion) {
				return githubapp.ErrAuthorization
			}
			j.Run.GitHubCredentials = true
			st.Jobs[j.ID] = j
			*job = j
			return nil
		}); err != nil {
			return nil, nil, err
		}
		var err error
		credentials, selections, err = s.githubCredentials(ctx, authorization.OwnerID, authorization.Key, authorization.ConnectionVersion, repos)
		if err != nil {
			return nil, nil, err
		}
	}
	if google {
		token, err := s.providerCredential(ctx, authorization.OwnerID, "google")
		if err == nil && token != "" {
			credentials = append(credentials, runner.Credential{Name: "GOOGLE_ACCESS_TOKEN", Value: token, AllowedHosts: []string{"docs.googleapis.com", "www.googleapis.com"}})
		}
	}
	if err := s.DB.Update(func(st *model.State) error {
		if !githubapp.Authorized(*st, authorization.Key, authorization.OwnerID, authorization.ConnectionVersion) {
			return githubapp.ErrAuthorization
		}
		githubapp.Count(st, "kioku_github_authorization_duration_milliseconds_total", uint64(time.Since(startedAt).Milliseconds()))
		githubapp.Count(st, "kioku_github_authorization_duration_count", 1)
		a := st.GitHubAttemptAuthorizations[githubapp.Key(authorization.Key)]
		a.Selections = selections
		if !a.SelectionCounted {
			for _, v := range selections {
				githubapp.Count(st, fmt.Sprintf(`kioku_github_credential_selection_total{mode=%q,reason=%q}`, v.Mode, v.Reason), 1)
			}
			a.SelectionCounted = true
		}
		st.GitHubAttemptAuthorizations[githubapp.Key(a.Key)] = a
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return credentials, selections, nil
}
func selectionFailure(err error) (string, string) {
	reason := githubapp.Reason(err)
	if reason == "not_connected" {
		return "unauthenticated", reason
	}
	switch reason {
	case "disabled", "reauthorization_required", "suspended", "inaccessible", "configuration_error", "forbidden", "rate_limited", "temporary", "issuance_unknown":
	default:
		reason = "temporary"
	}
	return "blocked", reason
}
func (s *Server) githubCredentials(ctx context.Context, ownerID string, key runner.AttemptKey, version uint64, repos []GitHubRepository) ([]runner.Credential, []model.GitHubRepositorySelection, error) {
	results := map[string]model.GitHubRepositorySelection{}
	for _, repo := range repos {
		name := repositoryName(repo)
		results[name] = model.GitHubRepositorySelection{Repository: name, Mode: "unauthenticated", Reason: "outside_installation"}
	}
	finish := func(credentials []runner.Credential) ([]runner.Credential, []model.GitHubRepositorySelection, error) {
		out := make([]model.GitHubRepositorySelection, 0, len(results))
		for _, v := range results {
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Repository < out[j].Repository })
		return credentials, out, nil
	}
	var installations []GitHubInstallation
	var err error
	if s.GitHubTokens == nil {
		err = &githubapp.Failure{Reason: "not_connected"}
	} else {
		installations, err = s.GitHubTokens.Installations(ctx, ownerID, repos)
	}
	if err != nil {
		var persistence githubapp.PersistenceError
		if errors.As(err, &persistence) || errors.Is(err, githubapp.ErrAuthorization) {
			return nil, nil, err
		}
		mode, reason := selectionFailure(err)
		for k, v := range results {
			v.Mode, v.Reason = mode, reason
			results[k] = v
		}
		return finish(nil)
	}
	sort.Slice(installations, func(i, j int) bool { return installations[i].ID < installations[j].ID })
	var credentials []runner.Credential
	counters := map[string]int{}
	for _, installation := range installations {
		if installation.Reason != "" {
			_, reason := selectionFailure(&githubapp.Failure{Reason: installation.Reason})
			for k, v := range results {
				if strings.EqualFold(strings.Split(k, "/")[0], installation.Account) && v.Mode != "token" {
					v.Mode, v.Reason = "blocked", reason
					results[k] = v
				}
			}
			continue
		}
		if installation.ID <= 0 || !githubAccount.MatchString(installation.Account) {
			continue
		}
		selected := map[int64]GitHubRepository{}
		for _, repo := range installation.Repositories {
			if v, ok := results[repositoryName(repo)]; ok && v.Mode != "token" && repo.ID > 0 && strings.EqualFold(repo.Account, installation.Account) {
				selected[repo.ID] = repo
			}
		}
		ids := make([]int64, 0, len(selected))
		for id := range selected {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for len(ids) > 0 {
			chunk := append([]int64(nil), ids[:min(500, len(ids))]...)
			ids = ids[len(chunk):]
			scope := githubapp.AuthorizedRepositorySet{OwnerID: ownerID, ConnectionVersion: version, InstallationID: installation.ID, RepositoryIDs: chunk, CheckedAt: installation.CheckedAt}
			if installation.OwnerID != "" && (installation.OwnerID != ownerID || installation.ConnectionVersion != version) {
				return nil, nil, githubapp.ErrAuthorization
			}
			token, err := s.GitHubTokens.Issue(ctx, key, scope)
			if err != nil || token == "" {
				var persistence githubapp.PersistenceError
				if errors.As(err, &persistence) || errors.Is(err, githubapp.ErrAuthorization) {
					return nil, nil, err
				}
				mode, reason := selectionFailure(err)
				for _, id := range chunk {
					name := repositoryName(selected[id])
					results[name] = model.GitHubRepositorySelection{Repository: name, Mode: mode, Reason: reason}
				}
				continue
			}
			account := strings.ReplaceAll(strings.ToUpper(installation.Account), "-", "_")
			counters[account]++
			name := "GITHUB_TOKEN_" + account
			if counters[account] > 1 {
				name += fmt.Sprintf("_%d", counters[account])
			}
			c := runner.Credential{Name: name, Value: token, AllowedHosts: []string{"api.github.com"}}
			for _, id := range chunk {
				repo := repositoryName(selected[id])
				c.Repositories = append(c.Repositories, repo)
				results[repo] = model.GitHubRepositorySelection{Repository: repo, Mode: "token", Reason: "authorized", Placeholder: name}
			}
			credentials = append(credentials, c)
		}
	}
	return finish(credentials)
}
func credentialHints(credentials []runner.Credential, selections []model.GitHubRepositorySelection) string {
	var b strings.Builder
	b.WriteString("\nGitHub source selection (use token mode from the first request; blocked mode must not fall back to unauthenticated):\n")
	for _, v := range selections {
		data, _ := json.Marshal(v)
		b.Write(data)
		b.WriteByte('\n')
	}
	for _, c := range credentials {
		if len(c.Repositories) > 0 {
			fmt.Fprintf(&b, "Use the vault placeholder $%s in the Authorization header for these GitHub repositories only: %s.\n", c.Name, strings.Join(c.Repositories, ", "))
		}
	}
	return b.String()
}
