package server

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func refreshForm(client, token string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {token}}
}
func assertConnectorAccess(t *testing.T, f *fixture, token string, want bool) {
	t.Helper()
	if err := f.db.View(func(st model.State) error {
		_, ok := connectorGrant(st, auth.Hash(token))
		if ok != want {
			t.Errorf("access valid = %v, want %v", ok, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestConnectorRefreshReplayRevokesFamily(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, project.ID)
	access, refresh := connectorTokenPair(t, f, form)
	independent := connectorAccessToken(t, f, human, project.ID)
	access2, refresh2 := connectorTokenPair(t, f, refreshForm(client, refresh))
	assertConnectorAccess(t, f, access, false)
	access3, refresh3 := connectorTokenPair(t, f, refreshForm(client, refresh2))
	assertConnectorAccess(t, f, access2, false)
	assertConnectorAccess(t, f, access3, true)
	if got := connectorForm(f, "POST", "/oauth/token", "", refreshForm(client, refresh)); got.Code != 400 {
		t.Fatal(got.Code, got.Body.String())
	}
	assertConnectorAccess(t, f, access3, false)
	if got := connectorForm(f, "POST", "/oauth/token", "", refreshForm(client, refresh3)); got.Code != 400 {
		t.Fatal(got.Code, got.Body.String())
	}
	assertConnectorAccess(t, f, independent, true)
}
func TestConnectorCodeFailuresRollbackAndReplayRevokes(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, project.ID)
	for _, change := range []struct{ key, value string }{
		{"code_verifier", strings.Repeat("x", 43)},
		{"redirect_uri", f.url + "/wrong"},
		{"client_id", connectorClient(t, f)},
		{"resource", "https://wrong.example/mcp"},
	} {
		bad := url.Values{}
		for k, v := range form {
			bad[k] = append([]string(nil), v...)
		}
		bad.Set(change.key, change.value)
		got := connectorForm(f, "POST", "/oauth/token", "", bad)
		if got.Code != 400 && got.Code != 401 {
			t.Fatalf("%s: %d %s", change.key, got.Code, got.Body.String())
		}
	}
	access, refresh := connectorTokenPair(t, f, form)
	if got := connectorForm(f, "POST", "/oauth/token", "", form); got.Code != 400 {
		t.Fatal(got.Code, got.Body.String())
	}
	assertConnectorAccess(t, f, access, false)
	if got := connectorForm(f, "POST", "/oauth/token", "", refreshForm(client, refresh)); got.Code != 400 {
		t.Fatal(got.Code, got.Body.String())
	}
}
func TestConnectorConcurrentRefreshFailsClosed(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, project.ID)
	_, refresh := connectorTokenPair(t, f, form)
	type result struct {
		status int
		body   string
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := connectorForm(f, "POST", "/oauth/token", "", refreshForm(client, refresh))
			results <- result{r.Code, r.Body.String()}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, failure := 0, 0
	for r := range results {
		switch r.status {
		case 200:
			success++
			var tokens struct {
				Access string `json:"access_token"`
			}
			if err := json.Unmarshal([]byte(r.body), &tokens); err != nil {
				t.Fatal(err)
			}
			assertConnectorAccess(t, f, tokens.Access, false)
		case 400:
			failure++
		default:
			t.Fatal(r.status, r.body)
		}
	}
	if success != 1 || failure != 1 {
		t.Fatal(success, failure)
	}
}
func TestConnectorFositePersistenceAndNoBearerSecrets(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	client := connectorClient(t, f)
	code, form := connectorAuthCode(t, f, client, human, project.ID)
	// Persist a snapshot to a separate real store, then reopen it and construct
	// a new server and cipher. Neither provider nor key derivation may rely on RAM.
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.View(func(st model.State) error { return db.Update(func(dst *model.State) error { *dst = st; return nil }) }); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cipher, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	f.s = New(f.s.Config, db, f.s.Blobs, cipher)
	f.db = db
	access, refresh := connectorTokenPair(t, f, form)
	assertConnectorAccess(t, f, access, true)
	if err = db.View(func(st model.State) error {
		data, err := json.Marshal(struct {
			Codes  map[string]model.OAuthCode
			Tokens map[string]model.OAuthGrant
		}{st.OAuthCodes, st.OAuthTokens})
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
