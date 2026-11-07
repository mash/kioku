package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

type githubAppProvider struct{ service *githubapp.Service }

func (p *githubAppProvider) Installations(ctx context.Context, owner string, _ []GitHubRepository) ([]GitHubInstallation, error) {
	token, c, err := p.service.UserToken(ctx, owner)
	if err != nil {
		return nil, err
	}
	all, err := p.service.Client.Installations(ctx, token)
	if err != nil {
		if githubapp.Reason(err) == "unauthorized" {
			if e := p.service.RequireAuthorization(owner, c.Version, c.CredentialGeneration); e != nil {
				return nil, e
			}
			return nil, &githubapp.Failure{Reason: "reauthorization_required"}
		}
		return nil, err
	}
	available := map[int64]githubapp.Installation{}
	for _, v := range all {
		available[v.ID] = v
	}
	var result []GitHubInstallation
	for id, selected := range c.Installations {
		resolved := GitHubInstallation{ID: id, Account: selected.Login, OwnerID: owner, ConnectionVersion: c.Version, CheckedAt: time.Now().UTC()}
		current, ok := available[id]
		var lookupErr error
		if !ok || current.AppID != c.AppID || current.Account.ID != selected.AccountID {
			lookupErr = &githubapp.Failure{Reason: "inaccessible"}
		} else {
			var verified githubapp.Installation
			verified, lookupErr = p.service.Client.Installation(ctx, id)
			if lookupErr == nil {
				lookupErr = githubapp.ValidateInstallation(verified, c.AppID, selected.AccountID)
				if verified.ID != id {
					lookupErr = &githubapp.Failure{Reason: "inaccessible"}
				}
			}
			if lookupErr == nil {
				var repos []githubapp.Repository
				repos, lookupErr = p.service.Client.Repositories(ctx, token, id)
				if lookupErr == nil {
					for _, repo := range repos {
						parts := strings.Split(repo.FullName, "/")
						if len(parts) == 2 {
							resolved.Repositories = append(resolved.Repositories, GitHubRepository{Account: parts[0], Name: parts[1], ID: repo.ID})
						}
					}
					resolved.Account = current.Account.Login
				}
			}
		}
		if lookupErr != nil {
			resolved.Reason = githubapp.Reason(lookupErr)
			switch resolved.Reason {
			case "missing_or_inaccessible":
				resolved.Reason = "inaccessible"
			case "unauthorized":
				if e := p.service.RequireAuthorization(owner, c.Version, c.CredentialGeneration); e != nil {
					return nil, e
				}
				return nil, &githubapp.Failure{Reason: "reauthorization_required"}
			}
		}
		if err := p.service.DB.Update(func(st *model.State) error {
			connection := st.GitHubAppConnections[owner]
			if connection.Version != c.Version {
				return githubapp.ErrAuthorization
			}
			state := connection.Installations[id]
			if resolved.Reason == "" {
				state.Status = "active"
				state.Login = resolved.Account
				state.Reason = ""
				state.CheckedAt = resolved.CheckedAt
			} else if resolved.Reason == "suspended" || resolved.Reason == "inaccessible" {
				state.Status = resolved.Reason
				state.Reason = resolved.Reason
				state.CheckedAt = resolved.CheckedAt
			}
			connection.Installations[id] = state
			st.GitHubAppConnections[owner] = connection
			return nil
		}); err != nil {
			if errors.Is(err, githubapp.ErrAuthorization) {
				return nil, err
			}
			return nil, githubapp.PersistenceError{}
		}
		result = append(result, resolved)
	}
	return result, nil
}
func (p *githubAppProvider) Issue(ctx context.Context, key runner.AttemptKey, scope githubapp.AuthorizedRepositorySet) (runner.Secret, error) {
	return p.service.Issue(ctx, key, scope)
}
func (p *githubAppProvider) Revoke(ctx context.Context, key runner.AttemptKey) error {
	return p.service.Revoke(ctx, key)
}
