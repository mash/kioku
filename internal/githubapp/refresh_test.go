package githubapp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func seedUserCredential(t *testing.T, s *Service, expires time.Time) {
	t.Helper()
	c := UserCredential{AccessToken: "old-secret", RefreshToken: "refresh-secret", AccessExpiresAt: expires, RefreshExpiresAt: expires.Add(30 * 24 * time.Hour)}
	b, _ := json.Marshal(c)
	if err := s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnections["u"]
		v.AppID = 7
		v.CredentialGeneration = 1
		v.EncryptedCredential = s.Cipher.Seal(string(b), "github-app:u")
		v.AccessExpiresAt = c.AccessExpiresAt
		v.RefreshExpiresAt = c.RefreshExpiresAt
		st.GitHubAppConnections["u"] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestRefreshSerializedAndRecoveryNeverReplays(t *testing.T) {
	s, _, _ := tokenFixture(t)
	seedUserCredential(t, s, time.Now().Add(time.Minute))
	calls := 0
	s.Client.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		_ = s.DB.View(func(st model.State) error {
			if st.GitHubAppConnections["u"].RefreshState != "refreshing" {
				t.Error("missing intent")
			}
			return nil
		})
		return response(200, `{"access_token":"new-secret","refresh_token":"new-refresh","expires_in":28800,"refresh_token_expires_in":15000000}`), nil
	})}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			token, c, err := s.UserToken(t.Context(), "u")
			if err != nil || string(token) != "new-secret" || c.Version != 1 || c.CredentialGeneration != 2 {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls != 1 {
		t.Fatal(calls)
	}
	_ = s.DB.Update(func(st *model.State) error {
		c := st.GitHubAppConnections["u"]
		c.RefreshState = "refreshing"
		st.GitHubAppConnections["u"] = c
		return nil
	})
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UserToken(t.Context(), "u"); Reason(err) != "reauthorization_required" {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("refresh replayed")
	}
	_ = s.DB.View(func(st model.State) error {
		if st.GitHubAppConnections["u"].EncryptedCredential != "" {
			t.Fatal("uncertain credential retained")
		}
		return nil
	})
}
func TestLateRefreshCannotOverwriteReconnect(t *testing.T) {
	s, _, _ := tokenFixture(t)
	seedUserCredential(t, s, time.Now())
	s.Client.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		_ = s.DB.Update(func(st *model.State) error {
			st.GitHubAppConnections["u"] = model.GitHubAppConnection{Version: 2, Status: "inactive"}
			return nil
		})
		return response(200, `{"access_token":"late-secret","refresh_token":"late-refresh","expires_in":28800,"refresh_token_expires_in":15000000}`), nil
	})}
	if _, _, err := s.UserToken(t.Context(), "u"); !errors.Is(err, ErrAuthorization) {
		t.Fatal(err)
	}
	_ = s.DB.View(func(st model.State) error {
		c := st.GitHubAppConnections["u"]
		if c.Version != 2 || c.EncryptedCredential != "" {
			t.Fatal("overwrote reconnect")
		}
		return nil
	})
}
func TestIssueInvalidatedBeforeSaveAndDisabledCleanup(t *testing.T) {
	s, key, scope := tokenFixture(t)
	revokes := 0
	s.Client.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "DELETE" {
			revokes++
			return response(204, ""), nil
		}
		_ = s.DB.Update(func(st *model.State) error {
			if len(st.GitHubTokenAttempts) != 1 {
				t.Error("POST before intent")
			}
			a := st.GitHubAttemptAuthorizations[Key(key)]
			a.InvalidatedAt = time.Now()
			st.GitHubAttemptAuthorizations[Key(key)] = a
			return nil
		})
		return response(201, fmt.Sprintf(`{"token":"late-installation-secret","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))), nil
	})}
	if token, err := s.Issue(t.Context(), key, scope); token != "" || !errors.Is(err, ErrAuthorization) {
		t.Fatal("late token released", err)
	}
	s.Client.Config = Config{}
	if err := s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if revokes != 1 {
		t.Fatal("cleanup depended on App key")
	}
}
func TestCrashIntentAndWrongDatabaseKey(t *testing.T) {
	s, key, scope := tokenFixture(t)
	now := time.Now()
	_ = s.DB.Update(func(st *model.State) error {
		st.GitHubTokenAttempts["intent"] = model.GitHubTokenAttempt{Key: key, OwnerID: scope.OwnerID, ConnectionVersion: 1, Status: "issuing", JWTExpiresAt: now.Add(9 * time.Minute)}
		return nil
	})
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.View(func(st model.State) error {
		r := st.GitHubTokenAttempts["intent"]
		if r.Status != "issuance_unknown" || !r.ExpiresAt.Equal(now.Add(70*time.Minute)) {
			t.Fatal("unsafe unknown bound")
		}
		return nil
	})
	seedUserCredential(t, s, time.Now().Add(time.Hour))
	other, _ := auth.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	s.Cipher = other
	if err := s.Recover(); err == nil {
		t.Fatal("opened encrypted catalog with wrong key")
	}
}
