package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

func loopbackRedirect(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "/callback" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") || u.Opaque != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535 && u.Host == "127.0.0.1:"+u.Port()
}

func (s *Server) loopbackStart(w http.ResponseWriter, r *http.Request, _ string) error {
	var req struct {
		Provider    string `json:"provider"`
		Challenge   string `json:"code_challenge"`
		Method      string `json:"code_challenge_method"`
		RedirectURI string `json:"redirect_uri"`
		State       string `json:"state"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	p, ok := s.Config.Providers[req.Provider]
	if !ok || p.ClientID == "" {
		return fail(400, "provider is not configured")
	}
	if req.Method != "S256" || !validChallenge(req.Challenge) {
		return fail(400, "PKCE S256 required")
	}
	if !loopbackRedirect(req.RedirectURI) {
		return fail(400, "redirect_uri must be http://127.0.0.1:PORT/callback")
	}
	if len(req.State) < 1 || len(req.State) > 128 || !unreserved(req.State) {
		return fail(400, "invalid state")
	}
	id := auth.Token()
	err := s.DB.Update(func(st *model.State) error {
		prune(st)
		if len(st.Devices) >= 10000 {
			return fail(429, "too many login attempts")
		}
		st.Devices[id] = model.Device{LoopbackChallenge: req.Challenge, LoopbackRedirectURI: req.RedirectURI, LoopbackState: req.State, Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 201, map[string]string{"device": id, "url": s.Config.BaseURL + "/auth/" + req.Provider + "/start?device=" + id})
	return nil
}

func (s *Server) loopbackToken(w http.ResponseWriter, r *http.Request, _ string) error {
	var req *struct {
		Device   string `json:"device"`
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
	}
	if err := decode(w, r, &req); err != nil || req == nil {
		return fail(400, "invalid request")
	}
	token := auth.Token()
	var problem error
	var account model.User
	var provider string
	err := s.DB.Update(func(st *model.State) error {
		d, ok := st.Devices[req.Device]
		// Persist consumption even on failure; returning an error here rolls it back.
		if ok && d.Purpose == "" {
			used := d
			used.Used = true
			st.Devices[req.Device] = used
		}
		if !ok || d.Purpose != "" || d.Used || !d.Expires.After(time.Now()) || d.LoopbackChallenge == "" || d.UserID == "" || d.LoopbackCodeHash == "" || d.LoopbackCodeHash != auth.Hash(req.Code) || !validVerifier(req.Verifier) || auth.Challenge(req.Verifier) != d.LoopbackChallenge {
			problem = fail(410, "login request expired; run kioku login again")
			return nil
		}
		account = st.Users[d.UserID]
		provider = d.IdentityProvider
		st.Sessions[auth.Hash(token)] = model.Session{Created: time.Now().UTC(), UserID: d.UserID, Expires: time.Now().Add(30 * 24 * time.Hour), IdentityProvider: d.IdentityProvider, IdentityID: d.IdentityID, IdentityCredential: d.IdentityCredential}
		return nil
	})
	if err != nil {
		return err
	}
	if problem != nil {
		return problem
	}
	jsonOut(w, 200, map[string]string{"token": token, "provider": provider, "name": account.Name, "email": account.Email})
	return nil
}

// Only an unbound, live loopback request may receive a callback.
func pendingLoopback(d model.Device) bool {
	return d.Purpose == "" && d.LoopbackChallenge != "" && !d.Used && d.UserID == "" && d.Expires.After(time.Now())
}

func loopbackExpired(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	return render(w, "CLI login expired", struct{ Message string }{"CLI login expired; run kioku login again"}, messageTemplate)
}

func (s *Server) loopbackFailure(w http.ResponseWriter, r *http.Request, id, reason string) error {
	var target string
	err := s.DB.Update(func(st *model.State) error {
		d := st.Devices[id]
		if pendingLoopback(d) || pendingConnect(d) {
			target = d.LoopbackRedirectURI + "?" + url.Values{"error": {reason}, "state": {d.LoopbackState}}.Encode()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if target == "" {
		return loopbackExpired(w)
	}
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request, _ string) error {
	if r.PathValue("provider") == "github" && r.URL.Query().Get("connect") != "" {
		return s.legacyGitHubConnect(w, r)
	}
	provider := r.PathValue("provider")
	p, ok := s.Config.Providers[provider]
	if !ok || p.ClientID == "" {
		return fail(400, "provider is not configured")
	}
	returnPath, err := authorizeReturn(r.URL.Query().Get("return"))
	if err != nil {
		return err
	}
	state, verifier := auth.Token(), auth.Token()
	pending := model.OAuthState{ReturnPath: returnPath, Provider: provider, Purpose: "login", Verifier: verifier, Expires: time.Now().Add(10 * time.Minute), Device: r.URL.Query().Get("device")}
	err = s.DB.Update(func(st *model.State) error {
		prune(st)
		if len(st.OAuth) > 10000 {
			return fail(429, "too many login attempts")
		}
		if id := r.URL.Query().Get("connect"); id != "" {
			d, ok := st.Devices[id]
			if !ok || !pendingConnect(d) || d.IdentityProvider != provider || pending.Device != "" {
				return fail(403, "invalid connection request; run kioku connect again")
			}
			pending.Purpose = "connect"
			pending.UserID = d.UserID
			pending.Device = id
		} else if pending.Device != "" {
			d, ok := st.Devices[pending.Device]
			if !ok || !pendingLoopback(d) {
				return fail(410, "login request expired")
			}
		}

		st.OAuth[auth.Hash(state)] = pending
		return nil
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "kioku_oauth", Value: state, Path: "/auth/", HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, p.URL(state, verifier, pending.Purpose), http.StatusFound)
	return nil
}
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request, _ string) error {
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("kioku_oauth")
	if err != nil || state == "" || cookie.Value != state {
		return fail(403, "OAuth state mismatch; restart login")
	}
	var pending model.OAuthState
	err = s.DB.Update(func(st *model.State) error {
		v, ok := st.OAuth[auth.Hash(state)]
		if !ok || v.Expires.Before(time.Now()) || v.Provider != r.PathValue("provider") {
			return fail(403, "OAuth request expired")
		}
		pending = v
		delete(st.OAuth, auth.Hash(state))
		return nil
	})
	if err != nil {
		return err
	}
	if pending.Device != "" && r.URL.Query().Has("error") {
		return s.loopbackFailure(w, r, pending.Device, "access_denied")
	}
	if pending.Provider == "github" && pending.Purpose == "connect" {
		return s.githubFailure(w, r, pending.Device, "restart_required")
	}
	p := s.Config.Providers[pending.Provider]
	credential, identity, err := p.Exchange(r.Context(), r.URL.Query().Get("code"), pending.Verifier)
	if err != nil {
		if pending.Device != "" {
			return s.loopbackFailure(w, r, pending.Device, "server_error")
		}
		return fail(502, "OAuth exchange failed; restart login")
	}
	if pending.Purpose == "connect" {
		b, err := json.Marshal(credential)
		if err != nil {
			return err
		}
		return s.connectCallback(w, r, pending, string(b))
	}
	var uid, loopbackURL string
	token := auth.Token()
	code := "kioku_loopback." + auth.Token()
	var expired bool
	err = s.DB.Update(func(st *model.State) error {
		if pending.Device != "" && !pendingLoopback(st.Devices[pending.Device]) {
			expired = true
			return nil
		}
		for id, u := range st.Users {
			if u.Identities[pending.Provider] == identity.ID {
				uid = id
				break
			}
		}
		if uid == "" {
			// Never merge accounts based on email alone.
			uid = auth.Token()[:20]
			st.Users[uid] = model.User{ID: uid, Name: identity.Name, Email: identity.Email, Identities: map[string]string{pending.Provider: identity.ID}, Connections: map[string]string{}}
		}
		// Refresh displayed profile data on every verified login. Membership is
		// still keyed by immutable user ID, never merged by email.
		user := st.Users[uid]
		user.Email = identity.Email
		user.Name = identity.Name
		st.Users[uid] = user
		credentialJSON, _ := json.Marshal(credential)
		sealedIdentity := s.Cipher.Seal(string(credentialJSON), identityContext(uid, pending.Provider, identity.ID))
		if pending.Device != "" {
			d := st.Devices[pending.Device]
			d.UserID = uid
			d.IdentityProvider = pending.Provider
			d.IdentityID = identity.ID
			d.IdentityCredential = sealedIdentity
			d.LoopbackCodeHash = auth.Hash(code)
			d.Expires = time.Now().Add(time.Minute)
			st.Devices[pending.Device] = d
			loopbackURL = d.LoopbackRedirectURI + "?" + url.Values{"code": {code}, "state": {d.LoopbackState}}.Encode()
		}
		st.Sessions[auth.Hash(token)] = model.Session{Created: time.Now().UTC(), UserID: uid, Expires: time.Now().Add(30 * 24 * time.Hour), IdentityProvider: pending.Provider, IdentityID: identity.ID, IdentityCredential: sealedIdentity}
		return nil
	})
	if err != nil {
		return err
	}
	if expired {
		return loopbackExpired(w)
	}
	http.SetCookie(w, &http.Cookie{Name: "kioku_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	http.SetCookie(w, &http.Cookie{Name: "kioku_oauth", Value: "", Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
	if pending.Device != "" {
		http.Redirect(w, r, loopbackURL, http.StatusFound)
		return nil
	}
	if pending.ReturnPath != "" {
		target, err := authorizeReturn(pending.ReturnPath)
		if err != nil {
			return err
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return nil
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}
func prune(st *model.State) {
	now := time.Now()
	for k, v := range st.OAuthCodes {
		if !v.Expires.After(now) {
			delete(st.OAuthCodes, k)
		}
	}
	// Keep the issuing grant's selection available for a code's full 60s.
	// An expired access token still cannot authenticate another MCP request.
	pendingGrants := map[string]bool{}
	for k, v := range st.LoginCodes {
		if !v.Expires.After(now) {
			delete(st.LoginCodes, k)
		} else {
			pendingGrants[v.GrantKey] = true
		}
	}
	for k, v := range st.OAuthTokens {
		if !v.Expires.After(now) && !pendingGrants[k] {
			delete(st.OAuthTokens, k)
		}
	}
	for k, v := range st.OAuth {
		if v.Expires.Before(now) {
			delete(st.OAuth, k)
		}
	}
	for k, v := range st.Devices {
		if v.Used || !v.Expires.After(now) {
			if _, ok := st.GitHubAppConnectAttempts[k]; ok {
				githubapp.Count(st, `kioku_github_connect_total{result="expired"}`, 1)
				delete(st.GitHubAppConnectAttempts, k)
			}
			delete(st.Devices, k)
		}
	}
	for k, v := range st.Sessions {
		if v.Expires.Before(now) {
			delete(st.Sessions, k)
		}
	}
}

func (s *Server) providerCredential(ctx context.Context, uid, provider string) (runner.Secret, error) {
	unlock, err := s.lockProject(ctx, "provider:"+uid+":"+provider)
	if err != nil {
		return "", err
	}
	defer unlock()
	var sealed string
	if err := s.DB.View(func(st model.State) error { sealed = st.Users[uid].Connections[provider]; return nil }); err != nil {
		return "", err
	}
	if sealed == "" {
		return "", fail(400, "run kioku connect "+provider+" first")
	}
	plain, err := s.Cipher.Open(sealed, uid+":"+provider)
	if err != nil {
		return "", err
	}
	var c auth.Credential
	if err = json.Unmarshal([]byte(plain), &c); err != nil {
		return "", err
	}
	p, ok := s.Config.Providers[provider]
	if !ok {
		return "", errors.New("provider unavailable")
	}
	original := c
	c.ExpiresAt = time.Now().Add(-time.Minute)
	fresh, err := p.Refresh(ctx, c)
	if err != nil {
		return "", fail(400, "connection expired; run kioku connect "+provider)
	}
	if fresh != original {
		b, _ := json.Marshal(fresh)
		err = s.DB.Update(func(st *model.State) error {
			u := st.Users[uid]
			if u.Connections[provider] != sealed {
				return fail(409, "connection changed; retry")
			}
			u.Connections[provider] = s.Cipher.Seal(string(b), uid+":"+provider)
			st.Users[uid] = u
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return runner.Secret(fresh.AccessToken), nil
}

func identityContext(uid, provider, id string) string {
	return "identity:" + uid + ":" + provider + ":" + id
}

// currentEmail revalidates the identity attached to this exact login session.
// Old sessions without an encrypted identity proof must sign in again.
func (s *Server) currentEmail(r *http.Request, uid string) (string, error) {
	key, session := s.session(r)
	if session.UserID != uid || session.IdentityCredential == "" || session.IdentityProvider == "" || session.IdentityID == "" {
		return "", fail(401, "sign in again to verify your email before accepting this invitation")
	}
	provider, ok := s.Config.Providers[session.IdentityProvider]
	if !ok {
		return "", fail(401, "sign in again with an available identity provider")
	}
	plain, err := s.Cipher.Open(session.IdentityCredential, identityContext(uid, session.IdentityProvider, session.IdentityID))
	if err != nil {
		return "", err
	}
	var credential auth.Credential
	if err = json.Unmarshal([]byte(plain), &credential); err != nil {
		return "", err
	}
	fresh, err := provider.Refresh(r.Context(), credential)
	if err != nil {
		return "", fail(401, "sign in again to verify your current email")
	}
	identity, err := provider.Identity(r.Context(), fresh.AccessToken)
	if err != nil {
		return "", fail(502, "could not verify your current email with the identity provider")
	}
	if identity.ID != session.IdentityID || identity.Email == "" {
		return "", fail(401, "identity no longer matches this session; sign in again")
	}
	data, _ := json.Marshal(fresh)
	err = s.DB.Update(func(st *model.State) error {
		current, ok := st.Sessions[key]
		if !ok || current.UserID != uid || current.Expires.Before(time.Now()) || current.IdentityCredential != session.IdentityCredential {
			return fail(401, "session changed; sign in again")
		}
		user := st.Users[uid]
		if user.Identities[session.IdentityProvider] != identity.ID {
			return fail(401, "identity no longer linked to this account")
		}
		current.IdentityCredential = s.Cipher.Seal(string(data), identityContext(uid, session.IdentityProvider, session.IdentityID))
		st.Sessions[key] = current
		user.Email = identity.Email
		user.Name = identity.Name
		st.Users[uid] = user
		return nil
	})
	if err != nil {
		return "", err
	}
	return identity.Email, nil
}
