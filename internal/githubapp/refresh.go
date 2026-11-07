package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

// UserToken serializes refresh across every project of one owner. The intent is
// committed before sending; an ambiguous rotation can only be repaired by connect.
func (s *Service) UserToken(ctx context.Context, uid string) (runner.Secret, model.GitHubAppConnection, error) {
	lock, _ := s.refreshLocks.LoadOrStore(uid, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	var c model.GitHubAppConnection
	if err := s.DB.View(func(st model.State) error { c = st.GitHubAppConnections[uid]; return nil }); err != nil {
		return "", c, PersistenceError{}
	}
	if c.Status == "" || c.Status == "inactive" {
		return "", c, &Failure{Reason: "not_connected"}
	}
	if !s.Client.Config.Enabled {
		return "", c, &Failure{Reason: "disabled"}
	}
	if !s.Client.Config.Configured() || c.AppID != s.Client.Config.ID {
		return "", c, &Failure{Reason: "configuration_error"}
	}
	if c.Status != "active" || c.RefreshState != "" {
		return "", c, &Failure{Reason: "reauthorization_required"}
	}
	raw, err := s.Cipher.Open(c.EncryptedCredential, "github-app:"+uid)
	var credential UserCredential
	if err != nil || json.Unmarshal([]byte(raw), &credential) != nil {
		return "", c, PersistenceError{}
	}
	if credential.AccessToken == "" {
		return "", c, &Failure{Reason: "reauthorization_required"}
	}
	if credential.AccessExpiresAt.After(s.Client.now().Add(5 * time.Minute)) {
		return runner.Secret(credential.AccessToken), c, nil
	}
	if !credential.RefreshExpiresAt.After(s.Client.now()) || credential.RefreshToken == "" {
		if err := s.RequireAuthorization(uid, c.Version, c.CredentialGeneration); err != nil {
			return "", c, err
		}
		return "", c, &Failure{Reason: "reauthorization_required"}
	}
	if err = s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnections[uid]
		if v.Version != c.Version || v.CredentialGeneration != c.CredentialGeneration || v.Status != "active" {
			return ErrAuthorization
		}
		v.RefreshState = "refreshing"
		v.RefreshStartedAt = s.Client.now()
		st.GitHubAppConnections[uid] = v
		return nil
	}); err != nil {
		if errors.Is(err, ErrAuthorization) {
			return "", c, err
		}
		return "", c, PersistenceError{}
	}
	fresh, exchangeErr := s.Client.Exchange(ctx, "", credential.RefreshToken)
	if exchangeErr != nil {
		if err := s.RequireAuthorization(uid, c.Version, c.CredentialGeneration); err != nil {
			return "", c, err
		}
		return "", c, &Failure{Reason: "reauthorization_required"}
	}
	encoded, _ := json.Marshal(fresh)
	if err = s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnections[uid]
		if v.Version != c.Version || v.CredentialGeneration != c.CredentialGeneration || v.RefreshState != "refreshing" {
			return ErrAuthorization
		}
		v.EncryptedCredential = s.Cipher.Seal(string(encoded), "github-app:"+uid)
		v.AccessExpiresAt = fresh.AccessExpiresAt
		v.RefreshExpiresAt = fresh.RefreshExpiresAt
		v.CredentialGeneration++
		v.RefreshState = ""
		v.RefreshStartedAt = time.Time{}
		st.GitHubAppConnections[uid] = v
		c = v
		return nil
	}); err != nil {
		if errors.Is(err, ErrAuthorization) {
			return "", c, err
		}
		return "", c, PersistenceError{}
	}
	return runner.Secret(fresh.AccessToken), c, nil
}
func (s *Service) RequireAuthorization(uid string, version, generation uint64) error {
	if err := s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnections[uid]
		if v.Version == version && v.CredentialGeneration == generation {
			v.Status = "reauthorization_required"
			v.EncryptedCredential = ""
			v.RefreshState = ""
			st.GitHubAppConnections[uid] = v
		}
		return nil
	}); err != nil {
		return PersistenceError{}
	}
	return nil
}
