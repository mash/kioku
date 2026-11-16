package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func refreshForm(client, token string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {token}}
}
func assertConnectorAccess(t *testing.T, f *fixture, token string, want bool) {
	t.Helper()
	if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		_, ok, err := connectorGrant(st, auth.Hash(token))
		if err != nil {
			return err
		}
		if ok != want {
			t.Errorf("access valid = %v, want %v", ok, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestConnectorFositePersistenceAndNoBearerSecrets(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	client := connectorClient(t, f)
	code, form := connectorAuthCode(t, f, client, human, project.ID)
	// Persist a snapshot to a separate real store, then reopen it and construct
	// a new server and cipher. Neither provider nor key derivation may rely on RAM.
	// E1: copy a consistent SQL snapshot, then open it with a fresh server.
	dir := filepath.Join(t.TempDir(), "backup")
	if err := f.stateStore.Backup(dir); err != nil {
		t.Fatal(err)
	}
	stateStore, err := openFixtureDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stateStore.Close() })
	cipher, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	runners := f.s.runners
	f.s = New(f.s.Config, stateStore, f.s.Blobs, cipher)
	f.s.SetRunners(runners)
	f.stateStore = stateStore
	access, refresh := connectorTokenPair(t, f, form)
	assertConnectorAccess(t, f, access, true)
	if err = stateStore.Read(context.Background(), func(st *store.Queries) error {
		data, err := json.Marshal(struct {
			Codes  map[string]fixtureOAuthCode
			Tokens map[string]fixtureOAuthGrant
		}{fixtureOAuthCodesList(t, st, fixtureSessionsGet(t, st, auth.Hash(human)).UserID), fixtureOAuthTokensList(t, st, fixtureSessionsGet(t, st, auth.Hash(human)).UserID)})
		if err != nil {
			return err
		}
		for _, secret := range []string{code, form.Get("code_verifier"), access, refresh} {
			if strings.Contains(string(data), secret) {
				t.Error("bearer credential or PKCE verifier persisted")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	connectorTokenPair(t, f, refreshForm(client, refresh))
}

func TestConnectorFositeAuthorizationValidation(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	client := connectorClient(t, f)
	for _, tc := range []struct {
		name, key string
		values    []string
	}{
		{"missing state", "state", nil},
		{"short state", "state", []string{"short"}},
		{"wrong scope", "scope", []string{"admin"}},
		{"wrong resource", "resource", []string{"https://other.example/mcp"}},
		{"duplicate resource", "resource", []string{f.url + "/mcp", f.url + "/mcp"}},
		{"wrong audience", "audience", []string{"https://other.example/mcp"}},
		{"plain PKCE", "code_challenge_method", []string{"plain"}},
		{"missing PKCE", "code_challenge", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := connectorAuthorizeParams(f, client, auth.Token())
			form[tc.key] = tc.values
			got := connectorForm(f, "GET", "/oauth/authorize", human, form)
			if got.Code != 400 || got.Header().Get("Location") != "" {
				t.Fatal(got.Code, got.Header(), got.Body.String())
			}
		})
	}
}
