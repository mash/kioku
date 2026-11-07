package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func testConfig(t *testing.T) Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Enabled: true, ID: 7, ClientID: "Iv.fixture", Slug: "test-app", ClientSecret: "client-secret", PrivateKey: key}
}
func TestConfigAndJWT(t *testing.T) {
	c := testConfig(t)
	now := time.Unix(1800000000, 0)
	jwt, expiry, err := c.JWT(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(jwt), ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Fatal("invalid signature")
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(b, &claims)
	if claims["iss"] != c.ClientID || int64(claims["iat"].(float64)) != now.Unix()-60 || int64(claims["exp"].(float64)) != expiry.Unix() {
		t.Fatal(claims)
	}
	path, _ := filepath.Abs(filepath.Join(t.TempDir(), "key.pem"))
	_ = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(c.PrivateKey)}), 0600)
	settings := map[string]string{"ID": "7", "CLIENT_ID": c.ClientID, "SLUG": c.Slug, "CLIENT_SECRET": "fixture", "PRIVATE_KEY_FILE": path}
	get := func(k string) string { return settings[strings.TrimPrefix(k, "KIOKU_GITHUB_APP_")] }
	if got, err := LoadConfig(get); err != nil || got.Enabled || !got.Configured() {
		t.Fatal(got.Configured(), err)
	}
	for _, key := range []string{"ID", "CLIENT_ID", "SLUG", "CLIENT_SECRET", "PRIVATE_KEY_FILE"} {
		v := settings[key]
		delete(settings, key)
		if _, err := LoadConfig(get); err == nil {
			t.Fatal("accepted partial", key)
		}
		settings[key] = v
	}
	_ = os.Chmod(path, 0644)
	if _, err := LoadConfig(get); err == nil {
		t.Fatal("accepted public key file")
	}
	if _, err := LoadConfig(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(func(k string) string {
		if strings.HasSuffix(k, "ENABLED") {
			return "true"
		}
		return ""
	}); err == nil {
		t.Fatal("enabled without config")
	}
}
func TestHTTPClassificationAndPagination(t *testing.T) {
	for _, tc := range []struct {
		status       int
		body, reason string
		unknown      bool
	}{{401, "secret", "unauthorized", false}, {403, `{"message":"secret secondary rate limit"}`, "rate_limited", false}, {403, "secret", "forbidden", false}, {404, "secret", "missing_or_inaccessible", false}, {429, "secret", "rate_limited", false}, {500, "secret", "temporary", true}, {302, "secret", "temporary", true}} {
		c := &Client{HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-GitHub-Api-Version") != APIVersion || r.GetBody != nil {
				t.Error("request contract")
			}
			return response(tc.status, tc.body), nil
		})}}
		_, err := c.request(t.Context(), "issue", "POST", "https://api.github.com/test", "secret", map[string]string{"x": "y"}, nil)
		var f *Failure
		if !errors.As(err, &f) || f.Reason != tc.reason || f.Unknown != tc.unknown || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	calls := 0
	c := &Client{HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			resp := response(200, `{"repositories":[{"id":1,"full_name":"a/b"}]}`)
			resp.Header.Set("Link", `<https://attacker.invalid/?secret>; rel="next"`)
			return resp, nil
		}
		if r.URL.Host != "api.github.com" || r.URL.Query().Get("page") != "2" {
			t.Fatal("followed untrusted next link")
		}
		return response(503, "secret"), nil
	})}}
	if repos, err := c.Repositories(t.Context(), "token", 1); err == nil || repos != nil || calls != 2 {
		t.Fatal("partial authorization accepted")
	}
}
func TestScopeBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 500, 501, 1001} {
		s := AuthorizedRepositorySet{OwnerID: "u", InstallationID: 1}
		for i := range n {
			s.RepositoryIDs = append(s.RepositoryIDs, int64(i+1))
		}
		if (s.Validate() == nil) != (n > 0 && n <= 500) {
			t.Fatal(n)
		}
	}
	s := AuthorizedRepositorySet{OwnerID: "u", InstallationID: 1, RepositoryIDs: []int64{1, 1}}
	if s.Validate() == nil {
		t.Fatal("duplicate ID")
	}
}
func tokenFixture(t *testing.T) (*Service, runner.AttemptKey, AuthorizedRepositorySet) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cipher, _ := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s := NewService(&Client{Config: testConfig(t)}, db, cipher)
	key := runner.AttemptKey{ProjectID: "p", JobID: "j", AttemptID: "a"}
	scope := AuthorizedRepositorySet{OwnerID: "u", ConnectionVersion: 1, InstallationID: 3, RepositoryIDs: []int64{10}, CheckedAt: time.Now()}
	if err := db.Update(func(st *model.State) error {
		st.Users["u"] = model.User{ID: "u"}
		st.Projects["p"] = model.Project{ID: "p", Members: map[string]string{"u": "owner"}}
		st.Jobs["j"] = model.Job{ID: "j", ProjectID: "p", Status: "running", Run: &model.AgentRun{Attempt: model.Attempt{ID: "a"}}}
		st.GitHubAppConnections["u"] = model.GitHubAppConnection{Version: 1, Status: "active"}
		st.GitHubAttemptAuthorizations[Key(key)] = model.GitHubAttemptAuthorization{Key: key, OwnerID: "u", ConnectionVersion: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, key, scope
}
func TestDurableIssueDeduplicationBackupAndRevocation(t *testing.T) {
	s, key, scope := tokenFixture(t)
	calls := 0
	s.Client.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "DELETE" {
			if r.Header.Get("Authorization") != "Bearer fixture-installation-secret" {
				t.Error("wrong revoke")
			}
			return response(204, ""), nil
		}
		calls++
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !reflect.DeepEqual(body.Permissions, Permissions()) || !reflect.DeepEqual(body.RepositoryIDs, scope.RepositoryIDs) {
			t.Error("scope")
		}
		return response(201, fmt.Sprintf(`{"token":"fixture-installation-secret","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))), nil
	})}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			token, err := s.Issue(t.Context(), key, scope)
			if err != nil || string(token) != "fixture-installation-secret" {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls != 1 {
		t.Fatal(calls)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err := s.DB.Backup(backup); err != nil {
		t.Fatal(err)
	}
	restored, err := store.OpenControl(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	_ = restored.View(func(st model.State) error {
		if len(st.GitHubTokenAttempts) != 1 || len(st.GitHubAttemptAuthorizations) != 1 {
			t.Fatal("lost backup records")
		}
		b, _ := json.Marshal(st)
		if strings.Contains(string(b), "fixture-installation-secret") {
			t.Fatal("plaintext in state")
		}
		return nil
	})
	if err := s.Revoke(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Issue(t.Context(), key, scope); err == nil {
		t.Fatal("reissued terminal scope")
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestUnknownIssuanceAndCleanupBackoff(t *testing.T) {
	s, key, scope := tokenFixture(t)
	now := time.Now()
	s.Client.Now = func() time.Time { return now }
	s.Client.HTTP = &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret transport response lost") })}
	if _, err := s.Issue(t.Context(), key, scope); Reason(err) != "issuance_unknown" {
		t.Fatal(err)
	}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), key); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	now = now.Add(71 * time.Minute)
	if err := s.Revoke(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.View(func(st model.State) error {
		for _, v := range st.GitHubTokenAttempts {
			if v.Status != "expired_unobserved" {
				t.Fatal(v.Status)
			}
		}
		return nil
	})
	s, key, scope = tokenFixture(t)
	s.Client.Now = func() time.Time { return now }
	calls := 0
	s.Client.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			return response(201, fmt.Sprintf(`{"token":"secret","expires_at":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
		}
		calls++
		res := response(429, `{"message":"secondary rate limit"}`)
		res.Header.Set("Retry-After", "120")
		return res, nil
	})}
	if _, err := s.Issue(t.Context(), key, scope); err != nil {
		t.Fatal(err)
	}
	_ = s.Revoke(t.Context(), key)
	_ = s.Recover()
	_ = s.Revoke(t.Context(), key)
	if calls != 1 {
		t.Fatal("backoff shortened")
	}
	now = now.Add(120 * time.Second)
	_ = s.Revoke(t.Context(), key)
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestIssueJWTRejectionIsConfigurationError(t *testing.T) {
	s, key, scope := tokenFixture(t)
	s.Client.HTTP = &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return response(http.StatusUnauthorized, "invalid JWT"), nil
	})}
	if _, err := s.Issue(t.Context(), key, scope); Reason(err) != "configuration_error" {
		t.Fatalf("JWT rejection classification: %v", err)
	}
}
