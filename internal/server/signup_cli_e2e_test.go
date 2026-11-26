package server

import (
	"testing"

	"github.com/mash/kioku/internal/auth"
)

func TestE2ESignupLoopback(t *testing.T) {
	t.Parallel()
	e := newSignupE2E(t, "nature.global", map[string]oauthTestLogin{
		"allowed": {subject: "member", hd: "nature.global"},
	}, false)
	a := e.startLoopback()
	denied := e.oauthCallbackRequest(a.startURL, "outside", "")
	db := e.signupSQL()
	before := signupSnapshot(t, db)
	res := e.do(denied)
	e.expect(res, 302, nil)
	signupCookies(t, res, false)
	target, err := res.Result().Location()
	if err != nil || target.Scheme+"://"+target.Host+target.Path != a.redirect ||
		target.Query().Get("error") != "access_denied" || target.Query().Get("state") != a.state || target.Query().Has("code") {
		t.Fatalf("denial callback: %v %v", target, err)
	}
	if signupSnapshot(t, db) != before {
		t.Fatal("denial changed pending request or created credentials")
	}

	allowed := e.do(e.oauthCallbackRequest(a.startURL, "allowed", ""))
	e.expect(allowed, 302, nil)
	target, err = allowed.Result().Location()
	if err != nil || target.Query().Get("state") != a.state || target.Query().Get("code") == "" {
		t.Fatalf("allowed callback: %v %v", target, err)
	}
	e.expect(e.call("POST", "/api/auth/loopback/token", "", map[string]string{
		"device": a.device, "code": target.Query().Get("code"), "code_verifier": a.verifier,
	}), 200, nil)

	b := e.startLoopback()
	expired := e.oauthCallbackRequest(b.startURL, "outside", "")
	if _, err := db.Exec("UPDATE sessions SET expires=0 WHERE id=?", auth.Hash(b.device)); err != nil {
		t.Fatal(err)
	}
	before = signupSnapshot(t, db)
	res = e.do(expired)
	e.expect(res, 410, nil)
	signupCookies(t, res, false)
	if res.Header().Get("Content-Type") != "text/html; charset=utf-8" || res.Header().Get("Location") != "" || signupSnapshot(t, db) != before {
		t.Fatal("expired request redirected or changed the catalog")
	}
}
