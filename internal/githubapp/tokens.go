package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

var ErrPending = errors.New("GitHub token cleanup pending")
var ErrAuthorization = errors.New("GitHub attempt authorization changed")

type PersistenceError struct{}

func (PersistenceError) Error() string { return "GitHub credential persistence failed" }

type Service struct {
	Client       *Client
	DB           *store.Store
	Cipher       *auth.Cipher
	issueMu      sync.Mutex
	refreshLocks sync.Map
}

func Key(key runner.AttemptKey) string { return key.JobID + ":" + key.AttemptID }
func Count(st *model.State, key string, n uint64) {
	m := st.GitHubMetrics["cumulative"]
	if m.Counters == nil {
		m.Counters = map[string]uint64{}
	}
	m.Counters[key] += n
	st.GitHubMetrics["cumulative"] = m
}
func NewService(client *Client, db *store.Store, cipher *auth.Cipher) *Service {
	s := &Service{Client: client, DB: db, Cipher: cipher}
	client.Observe = func(o Observation) {
		_ = db.Update(func(st *model.State) error {
			Count(st, fmt.Sprintf(`kioku_github_api_total{operation=%q,status_class=%q,reason=%q}`, o.Operation, o.Class, o.Reason), 1)
			if o.Operation == "installations" || o.Operation == "repositories" {
				Count(st, fmt.Sprintf(`kioku_github_list_pages_total{operation=%q}`, o.Operation), 1)
			}
			if o.Remaining >= 0 {
				m := st.GitHubMetrics["cumulative"]
				if m.RateRemaining == nil {
					m.RateRemaining = map[string]int64{}
				}
				old, ok := m.RateRemaining[o.Resource]
				if !ok || o.Remaining < old {
					m.RateRemaining[o.Resource] = o.Remaining
				}
				st.GitHubMetrics["cumulative"] = m
			}
			return nil
		})
	}
	return s
}
func Authorized(st model.State, key runner.AttemptKey, owner string, version uint64) bool {
	a, ok := st.GitHubAttemptAuthorizations[Key(key)]
	if !ok || a.Key != key || a.OwnerID != owner || a.ConnectionVersion != version || !a.InvalidatedAt.IsZero() {
		return false
	}
	j, ok := st.Jobs[key.JobID]
	if !ok || j.ProjectID != key.ProjectID || j.Status != "running" || j.Run == nil || j.Run.Attempt.ID != key.AttemptID || j.Run.AbortReason != "" {
		return false
	}
	return st.Projects[key.ProjectID].Members[owner] == "owner" && st.GitHubAppConnections[owner].Version == version
}
func (s *Service) Recover() error {
	return s.DB.Update(func(st *model.State) error {
		for uid, c := range st.GitHubAppConnections {
			if c.EncryptedCredential != "" {
				if _, e := s.Cipher.Open(c.EncryptedCredential, "github-app:"+uid); e != nil {
					return errors.New("GitHub App credential cannot be decrypted; restore the database encryption key")
				}
			}
			if c.RefreshState == "refreshing" {
				c.Status = "reauthorization_required"
				c.EncryptedCredential = ""
				c.RefreshState = ""
				st.GitHubAppConnections[uid] = c
			}
		}
		for id, a := range st.GitHubAppConnectAttempts {
			if a.EncryptedCredential != "" {
				if _, e := s.Cipher.Open(a.EncryptedCredential, "github-connect:"+id); e != nil {
					return errors.New("GitHub App staging cannot be decrypted")
				}
			}
		}
		for id, t := range st.GitHubTokenAttempts {
			if t.EncryptedToken != "" {
				if _, e := s.Cipher.Open(t.EncryptedToken, "github-token:"+id); e != nil {
					return errors.New("GitHub token cannot be decrypted; restore the database encryption key")
				}
			}
			if t.Status == "issuing" {
				t.Status = "issuance_unknown"
				t.ExpiresAt = t.JWTExpiresAt.Add(time.Hour + time.Minute)
				st.GitHubTokenAttempts[id] = t
				Count(st, `kioku_github_token_total{operation="issue",result="issuance_unknown"}`, 1)
			}
		}
		return nil
	})
}
func (s *Service) Issue(ctx context.Context, key runner.AttemptKey, scope AuthorizedRepositorySet) (runner.Secret, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	if !s.Client.Config.Enabled {
		return "", &Failure{Reason: "disabled"}
	}
	ids := append([]int64(nil), scope.RepositoryIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b, _ := json.Marshal(struct {
		IDs         []int64
		Permissions map[string]string
	}{ids, Permissions()})
	digest := auth.Hash(string(b))
	id := auth.Hash(Key(key) + ":" + strconv.FormatInt(scope.InstallationID, 10) + ":" + digest)
	var old model.GitHubTokenAttempt
	if err := s.DB.View(func(st model.State) error {
		if !Authorized(st, key, scope.OwnerID, scope.ConnectionVersion) {
			return ErrAuthorization
		}
		old = st.GitHubTokenAttempts[id]
		return nil
	}); err != nil {
		if errors.Is(err, ErrAuthorization) {
			return "", err
		}
		return "", PersistenceError{}
	}
	if old.Status != "" {
		if old.Status != "active" || !old.ExpiresAt.After(s.Client.now()) {
			return "", &Failure{Reason: "issuance_unknown"}
		}
		raw, err := s.Cipher.Open(old.EncryptedToken, "github-token:"+id)
		if err != nil {
			return "", PersistenceError{}
		}
		return runner.Secret(raw), nil
	}
	jwt, expiry, err := s.Client.Config.JWT(s.Client.now())
	if err != nil {
		return "", err
	}
	intent := model.GitHubTokenAttempt{Key: key, OwnerID: scope.OwnerID, ConnectionVersion: scope.ConnectionVersion, InstallationID: scope.InstallationID, RepositoryIDs: ids, Permissions: Permissions(), ScopeDigest: digest, Status: "issuing", JWTExpiresAt: expiry, AuthorizationCheckedAt: scope.CheckedAt, IssuanceStartedAt: s.Client.now()}
	if err = s.DB.Update(func(st *model.State) error {
		if !Authorized(*st, key, scope.OwnerID, scope.ConnectionVersion) {
			return ErrAuthorization
		}
		connection := st.GitHubAppConnections[scope.OwnerID]
		if connection.Status != "active" || connection.RefreshState != "" {
			return &Failure{Reason: "reauthorization_required"}
		}
		st.GitHubTokenAttempts[id] = intent
		return nil
	}); err != nil {
		var failure *Failure
		if errors.Is(err, ErrAuthorization) || errors.As(err, &failure) {
			return "", err
		}
		return "", PersistenceError{}
	}
	token, expires, issueErr := s.Client.Issue(ctx, jwt, scope)
	intent.Status = "active"
	intent.ExpiresAt = expires
	if issueErr != nil {
		var f *Failure
		intent.Status = "rejected"
		if errors.As(issueErr, &f) && f.Unknown {
			intent.Status = "issuance_unknown"
			intent.ExpiresAt = expiry.Add(time.Hour + time.Minute)
		}
	} else {
		intent.EncryptedToken = s.Cipher.Seal(string(token), "github-token:"+id)
	}
	valid, connected := false, false
	err = s.DB.Update(func(st *model.State) error {
		valid = Authorized(*st, key, scope.OwnerID, scope.ConnectionVersion)
		connection := st.GitHubAppConnections[scope.OwnerID]
		connected = connection.Status == "active" && connection.RefreshState == ""
		if intent.Status == "active" && (!valid || !connected) {
			intent.Status = "revocation_pending"
			intent.RevocationRequestedAt = s.Client.now()
		}
		st.GitHubTokenAttempts[id] = intent
		Count(st, fmt.Sprintf(`kioku_github_token_total{operation="issue",result=%q}`, intent.Status), 1)
		if token != "" {
			Count(st, "kioku_github_scope_repositories", uint64(len(ids)))
		}
		return nil
	})
	if err != nil {
		if token != "" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = s.Client.Revoke(cleanup, token)
		}
		return "", PersistenceError{}
	}
	if !valid {
		return "", ErrAuthorization
	}
	if !connected {
		return "", &Failure{Reason: "reauthorization_required"}
	}
	if issueErr != nil {
		if intent.Status == "issuance_unknown" {
			return "", &Failure{Reason: "issuance_unknown"}
		}
		return "", issueErr
	}
	return token, nil
}
func terminal(status string) bool {
	switch status {
	case "rejected", "revoked", "unusable", "expired", "expired_unobserved":
		return true
	}
	return false
}

// Revoke can revoke only known tokens. Unknown issuance is retained until its
// conservative expiry; expired_unobserved is not evidence of remote revocation.
func (s *Service) Revoke(ctx context.Context, key runner.AttemptKey) error {
	return s.revoke(ctx, key, false)
}

// The sweeper only retires due tokens while an attempt remains authorized.
// Explicit attempt cleanup still revokes every scope, including live tokens.
func (s *Service) revoke(ctx context.Context, key runner.AttemptKey, onlyDue bool) error {
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	var records map[string]model.GitHubTokenAttempt
	if err := s.DB.Update(func(st *model.State) error {
		records = map[string]model.GitHubTokenAttempt{}
		for id, t := range st.GitHubTokenAttempts {
			if t.Key != key || terminal(t.Status) {
				continue
			}
			if onlyDue && !s.cleanupDue(*st, t) {
				continue
			}
			if t.RevocationRequestedAt.IsZero() {
				t.RevocationRequestedAt = s.Client.now()
			}
			if t.Status == "active" {
				t.Status = "revocation_pending"
			}
			st.GitHubTokenAttempts[id] = t
			records[id] = t
		}
		return nil
	}); err != nil {
		return PersistenceError{}
	}
	pending := false
	for id, t := range records {
		now := s.Client.now()
		if !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt) {
			t.Status = "expired"
			if t.EncryptedToken == "" {
				t.Status = "expired_unobserved"
			}
		} else if t.Status == "issuing" || t.Status == "issuance_unknown" || t.Status == "expiry_wait" {
			t.Status = "expiry_wait"
			pending = true
		} else if now.Before(t.NextAttemptAt) {
			pending = true
			continue
		} else {
			raw, err := s.Cipher.Open(t.EncryptedToken, "github-token:"+id)
			if err != nil {
				return PersistenceError{}
			}
			err = s.Client.Revoke(ctx, runner.Secret(raw))
			var f *Failure
			switch {
			case err == nil:
				t.Status = "revoked"
			case errors.As(err, &f) && f.Status == 401:
				t.Status = "unusable"
			default:
				t.Status = "revocation_pending"
				pending = true
				t.Failures++
				delay := 30 * time.Second
				if f != nil && f.Reason == "rate_limited" {
					delay = time.Minute * time.Duration(1<<min(t.Failures-1, 10))
				}
				t.NextAttemptAt = now.Add(delay)
				if f != nil && f.RetryAt.After(t.NextAttemptAt) {
					t.NextAttemptAt = f.RetryAt
				}
			}
		}
		if terminal(t.Status) {
			t.EncryptedToken = ""
		}
		if err := s.DB.Update(func(st *model.State) error {
			st.GitHubTokenAttempts[id] = t
			Count(st, fmt.Sprintf(`kioku_github_token_total{operation="revoke",result=%q}`, t.Status), 1)
			return nil
		}); err != nil {
			return PersistenceError{}
		}
	}
	if pending {
		return ErrPending
	}
	return nil
}
func (s *Service) cleanupDue(st model.State, t model.GitHubTokenAttempt) bool {
	return !s.Client.Config.Enabled || !Authorized(st, t.Key, t.OwnerID, t.ConnectionVersion) || !t.RevocationRequestedAt.IsZero() || (!t.ExpiresAt.IsZero() && !t.ExpiresAt.After(s.Client.now()))
}
func (s *Service) Sweep(ctx context.Context) error {
	keys := map[string]runner.AttemptKey{}
	if err := s.DB.View(func(st model.State) error {
		for _, t := range st.GitHubTokenAttempts {
			if !terminal(t.Status) && s.cleanupDue(st, t) {
				keys[Key(t.Key)] = t.Key
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.revoke(ctx, k, true); err != nil && !errors.Is(err, ErrPending) {
			return err
		}
	}
	return s.DB.Update(func(st *model.State) error {
		for id, a := range st.GitHubAttemptAuthorizations {
			j, ok := st.Jobs[a.Key.JobID]
			if ok && j.Run != nil && j.Run.Attempt.ID == a.Key.AttemptID && !j.Run.CleanupComplete {
				continue
			}
			complete := true
			for _, t := range st.GitHubTokenAttempts {
				if t.Key == a.Key && !terminal(t.Status) {
					complete = false
				}
			}
			if complete {
				delete(st.GitHubAttemptAuthorizations, id)
				for tid, t := range st.GitHubTokenAttempts {
					if t.Key == a.Key {
						delete(st.GitHubTokenAttempts, tid)
					}
				}
			}
		}
		for id, t := range st.GitHubTokenAttempts {
			if terminal(t.Status) {
				if _, ok := st.GitHubAttemptAuthorizations[Key(t.Key)]; !ok {
					delete(st.GitHubTokenAttempts, id)
				}
			}
		}
		return nil
	})
}
