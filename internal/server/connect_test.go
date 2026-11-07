package server

import (
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func newConnectAttempt(t *testing.T, f *fixture, token string) loopbackAttempt {
	t.Helper()
	a := loopbackAttempt{verifier: auth.Token(), state: auth.Token(), redirect: "http://127.0.0.1:5000/callback"}
	var result map[string]string
	if status := f.request("POST", "/api/connect/google", token, map[string]string{"provider": "google", "code_challenge": auth.Challenge(a.verifier), "code_challenge_method": "S256", "redirect_uri": a.redirect, "state": a.state}, &result); status != 201 {
		t.Fatalf("start connect: %d", status)
	}
	a.device, a.startURL = result["device"], result["url"]
	return a
}

func stageConnect(t *testing.T, f *fixture, a loopbackAttempt) string {
	t.Helper()
	response := finishLoopbackOAuth(t, f, startLoopbackOAuth(t, f, a))
	if response.StatusCode != 302 {
		t.Fatalf("connect callback: %d", response.StatusCode)
	}
	for _, c := range response.Cookies() {
		if c.Name == "kioku_session" {
			t.Fatal("connect created a browser session")
		}
	}
	target, err := response.Location()
	if err != nil || target.Scheme+"://"+target.Host+target.Path != a.redirect || target.Query().Get("state") != a.state || target.Query().Get("code") == "" {
		t.Fatal("invalid loopback redirect", err)
	}
	return target.Query().Get("code")
}

func connectExchange(a loopbackAttempt, code string) map[string]string {
	return map[string]string{"device": a.device, "code": code, "code_verifier": a.verifier}
}

// F2: the attacker owns the request, but the victim authorizes in a browser
// with no Kioku session. Without the code delivered to the victim's localhost,
// the attacker cannot install the victim's provider credential.
func TestConnectRequiresCLICompletion(t *testing.T) {
	f := setupLoopback(t)
	token := f.login("attacker@example.com")
	before := loopbackSnapshot(t, f)
	a := newConnectAttempt(t, f, token)
	code := stageConnect(t, f, a)
	staged := loopbackSnapshot(t, f)
	if !reflect.DeepEqual(before.Users, staged.Users) || !reflect.DeepEqual(before.Sessions, staged.Sessions) {
		t.Fatal("OAuth callback changed users or sessions")
	}
	if strings.Contains(staged.Devices[a.device].IdentityCredential, "victim") {
		t.Fatal("staged provider credential is plaintext")
	}
	if got := f.request("POST", "/api/connect/google/complete", token, connectExchange(a, "unknown-code"), nil); got != 410 {
		t.Fatal(got)
	}
	if got := f.request("POST", "/api/connect/google/complete", token, connectExchange(a, code), nil); got != 403 {
		t.Fatal("failed attempt was reusable", got)
	}
	if !reflect.DeepEqual(before.Users, loopbackSnapshot(t, f).Users) {
		t.Fatal("attacker account changed")
	}
}

func TestConnectCompletionOnlyUpdatesConnection(t *testing.T) {
	f := setupLoopback(t)
	token := f.login("owner@example.com")
	before := loopbackSnapshot(t, f)
	uid := before.Sessions[auth.Hash(token)].UserID
	a := newConnectAttempt(t, f, token)
	code := stageConnect(t, f, a)
	var response map[string]any
	if got := f.request("POST", "/api/connect/google/complete", token, connectExchange(a, code), &response); got != 200 || response["connected"] != true || response["provider"] != "google" {
		t.Fatal(got, response)
	}
	after := loopbackSnapshot(t, f)
	u := after.Users[uid]
	credential, err := f.s.Cipher.Open(u.Connections["google"], uid+":google")
	if err != nil || !strings.Contains(credential, "victim") {
		t.Fatal("missing provider credential", err)
	}
	u.Connections = before.Users[uid].Connections
	if !reflect.DeepEqual(u, before.Users[uid]) || !reflect.DeepEqual(before.Sessions, after.Sessions) {
		t.Fatal("connect changed login identity, profile or sessions")
	}
	if _, ok := after.Devices[a.device]; ok {
		t.Fatal("staged credential retained")
	}
	if got := f.request("POST", "/api/connect/google/complete", token, connectExchange(a, code), nil); got != 403 {
		t.Fatal("code reused", got)
	}
}

func TestConnectRejectsWrongOwnerProviderAndLoginExchange(t *testing.T) {
	f := setupLoopback(t)
	owner, other := f.login("owner@example.com"), f.login("other@example.com")
	a := newConnectAttempt(t, f, owner)
	code := stageConnect(t, f, a)
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/api/connect/google/complete", "", 401},
		{"/api/connect/google/complete", other, 403},
		{"/api/connect/github/complete", owner, 403},
	} {
		if got := f.request("POST", tc.path, tc.token, connectExchange(a, code), nil); got != tc.status {
			t.Fatal(tc.path, got)
		}
	}
	// A connect code must never mint a Kioku login session.
	before := loopbackSnapshot(t, f)
	if got := f.request("POST", "/api/auth/loopback/token", "", connectExchange(a, code), nil); got != 410 {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(before.Sessions, loopbackSnapshot(t, f).Sessions) {
		t.Fatal("connect code minted login session")
	}
	if got := f.request("POST", "/api/connect/google/complete", owner, connectExchange(a, code), nil); got != 200 {
		t.Fatal("wrong flow consumed connect code", got)
	}
}

func TestConnectInvalidProofExpiryAndRevocation(t *testing.T) {
	for _, mode := range []string{"wrong verifier", "expired", "revoked", "account deleted"} {
		t.Run(mode, func(t *testing.T) {
			f := setupLoopback(t)
			token := f.login("owner@example.com")
			a := newConnectAttempt(t, f, token)
			code := stageConnect(t, f, a)
			req := connectExchange(a, code)
			want := 410
			if mode == "wrong verifier" {
				req["code_verifier"] = auth.Token()
			}
			if err := f.db.Update(func(st *model.State) error {
				switch mode {
				case "expired":
					d := st.Devices[a.device]
					d.Expires = time.Now().Add(-time.Second)
					st.Devices[a.device] = d
				case "revoked":
					delete(st.Sessions, auth.Hash(token))
					want = 401
				case "account deleted":
					delete(st.Users, st.Sessions[auth.Hash(token)].UserID)
					want = 401
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := loopbackSnapshot(t, f)
			if got := f.request("POST", "/api/connect/google/complete", token, req, nil); got != want {
				t.Fatal(got)
			}
			if !reflect.DeepEqual(before.Users, loopbackSnapshot(t, f).Users) {
				t.Fatal("invalid completion changed account")
			}
		})
	}
}

func TestConnectRejectsLegacyAndInvalidStarts(t *testing.T) {
	f := setupLoopback(t)
	token := f.login("owner@example.com")
	for _, field := range []string{"code_challenge", "code_challenge_method", "redirect_uri", "state", "provider"} {
		req := map[string]string{"provider": "google", "code_challenge": auth.Challenge(auth.Token()), "code_challenge_method": "S256", "redirect_uri": "http://127.0.0.1:5000/callback", "state": auth.Token()}
		req[field] = "invalid"
		if field == "state" {
			req[field] = ""
		}
		if got := f.request("POST", "/api/connect/google", token, req, nil); got != 400 {
			t.Fatal(field, got)
		}
	}
	if got := f.request("POST", "/api/connect/google", token, nil, nil); got != 400 {
		t.Fatal("legacy start accepted", got)
	}
	if err := f.db.Update(func(st *model.State) error {
		st.Devices["legacy"] = model.Device{UserID: st.Sessions[auth.Hash(token)].UserID, SecretHash: auth.Hash("secret"), Expires: time.Now().Add(time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("GET", "/auth/google/start?connect=legacy&ticket=secret", "", nil, nil); got != 403 {
		t.Fatal("legacy ticket accepted", got)
	}
}

func TestConnectConcurrentCompletion(t *testing.T) {
	f := setupLoopback(t)
	token := f.login("owner@example.com")
	a := newConnectAttempt(t, f, token)
	code := stageConnect(t, f, a)
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.request("POST", "/api/connect/google/complete", token, connectExchange(a, code), nil)
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for status := range results {
		if status == 200 {
			successes++
		} else if status != 403 {
			t.Fatal(status)
		}
	}
	if successes != 1 {
		t.Fatal("completion is not single use", successes)
	}
}

func TestConnectCallbackFailureAndDuplicate(t *testing.T) {
	f := setupLoopback(t)
	token := f.login("owner@example.com")
	a := newConnectAttempt(t, f, token)
	first, second := startLoopbackOAuth(t, f, a), startLoopbackOAuth(t, f, a)
	if response := finishLoopbackOAuth(t, f, first); response.StatusCode != 302 {
		t.Fatal(response.StatusCode)
	}
	before := loopbackSnapshot(t, f)
	if response := finishLoopbackOAuth(t, f, second); response.StatusCode != 410 {
		t.Fatal(response.StatusCode)
	}
	if !reflect.DeepEqual(before.Devices, loopbackSnapshot(t, f).Devices) {
		t.Fatal("duplicate callback overwrote staged credential")
	}
	a = newConnectAttempt(t, f, token)
	req := startLoopbackOAuth(t, f, a)
	q := req.URL.Query()
	q.Del("code")
	q.Set("error", "access_denied")
	req.URL.RawQuery = q.Encode()
	response := finishLoopbackOAuth(t, f, req)
	target, err := url.Parse(response.Header.Get("Location"))
	if err != nil || response.StatusCode != 302 || target.Query().Get("error") != "access_denied" || target.Query().Get("state") != a.state {
		t.Fatal("cancellation not returned to CLI", err)
	}
}
