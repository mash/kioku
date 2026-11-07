package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

// GitHubTokenProvider is the boundary to the separately implemented App
// connection. Installations resolves only installations connected to ownerID.
// Issue must retain revocation identity durably by attempt, including ambiguous
// issuance responses. Revoke must be idempotent and revoke every token for that
// attempt after restarts. Implementations must never persist plaintext tokens.
// A nil provider means no App is configured; OAuth user tokens are never used.
type GitHubTokenProvider interface {
	Installations(context.Context, string, []GitHubRepository) ([]GitHubInstallation, error)
	Issue(context.Context, runner.AttemptKey, int64, GitHubTokenRequest) (runner.Secret, error)
	Revoke(context.Context, runner.AttemptKey) error
}
type GitHubRepository struct {
	Account, Name string
	ID            int64
}
type GitHubInstallation struct {
	ID           int64
	Account      string
	Repositories []GitHubRepository
}
type GitHubTokenRequest struct {
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
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

func (s *Server) sourceCredentials(ctx context.Context, job *model.Job, items []model.InboxItem) ([]runner.Credential, error) {
	var ownerID string
	if err := s.DB.View(func(st model.State) error {
		for id, role := range st.Projects[job.ProjectID].Members {
			if role == "owner" {
				ownerID = id
				break
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if ownerID == "" {
		return nil, nil
	}
	repos, google := pendingProviders(items)
	var credentials []runner.Credential
	if len(repos) > 0 && s.GitHubTokens != nil {
		// Persist the cleanup obligation before any token can be issued remotely.
		if err := s.DB.Update(func(st *model.State) error {
			j := st.Jobs[job.ID]
			if !sameManagedRun(j, *job) || j.Status != "running" {
				return fail(409, "generation ended")
			}
			j.Run.GitHubCredentials = true
			st.Jobs[j.ID] = j
			*job = j
			return nil
		}); err != nil {
			return nil, err
		}
		credentials = s.githubCredentials(ctx, ownerID, attempt(*job).Key, repos)
	}
	// Refresh last, immediately before Start. No refresh token enters the vault.
	if google {
		token, err := s.providerCredential(ctx, ownerID, "google")
		if err == nil && token != "" {
			credentials = append(credentials, runner.Credential{Name: "GOOGLE_ACCESS_TOKEN", Value: token, AllowedHosts: []string{"docs.googleapis.com", "www.googleapis.com"}})
		}
	}
	return credentials, nil
}
func (s *Server) githubCredentials(ctx context.Context, ownerID string, key runner.AttemptKey, repos []GitHubRepository) []runner.Credential {
	installations, err := s.GitHubTokens.Installations(ctx, ownerID, repos)
	if err != nil {
		slog.Warn("GitHub source credentials unavailable", "job", key.JobID)
		return nil
	}
	wanted := map[string]bool{}
	for _, repo := range repos {
		wanted[repositoryName(repo)] = true
	}
	sort.Slice(installations, func(i, j int) bool { return installations[i].ID < installations[j].ID })
	var credentials []runner.Credential
	counters := map[string]int{}
	for _, installation := range installations {
		if installation.ID <= 0 || !githubAccount.MatchString(installation.Account) {
			continue
		}
		selected := map[int64]GitHubRepository{}
		for _, repo := range installation.Repositories {
			if repo.ID > 0 && strings.EqualFold(repo.Account, installation.Account) && wanted[repositoryName(repo)] {
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
			token, err := s.GitHubTokens.Issue(ctx, key, installation.ID, GitHubTokenRequest{RepositoryIDs: chunk, Permissions: map[string]string{"contents": "read"}})
			if err != nil || token == "" {
				slog.Warn("GitHub installation token unavailable", "job", key.JobID)
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
				c.Repositories = append(c.Repositories, repositoryName(selected[id]))
			}
			credentials = append(credentials, c)
		}
	}
	return credentials
}
func credentialHints(credentials []runner.Credential) string {
	var b strings.Builder
	for _, c := range credentials {
		if len(c.Repositories) > 0 {
			fmt.Fprintf(&b, "\nUse the vault placeholder $%s in the Authorization header for these GitHub repositories only: %s.\n", c.Name, strings.Join(c.Repositories, ", "))
		}
	}
	return b.String()
}
