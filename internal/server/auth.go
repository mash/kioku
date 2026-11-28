package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func loopbackRedirect(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "/callback" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") || u.Opaque != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535 && u.Host == "127.0.0.1:"+u.Port()
}

func loopbackExpired(w http.ResponseWriter) error {
	return renderStatus(w, http.StatusGone, "CLI login expired", struct{ Message string }{"CLI login expired; run kioku login again"}, messageTemplate)
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
	data, err := store.EncodeSessionData(store.CLILoginData{LoopbackChallenge: &req.Challenge, LoopbackRedirectURI: &req.RedirectURI, LoopbackState: &req.State})
	if err != nil {
		return err
	}
	err = s.Store.Write(r.Context(), func(q *store.Queries) error {
		return q.InsertSession(store.SessionRow{ID: auth.Hash(id), Kind: "cli_login", Status: "pending", Expires: time.Now().Add(10 * time.Minute).UnixMilli(), Data: data})
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
	var account store.UserRow
	var provider string
	err := writeConsuming(r.Context(), s.Store, func(q *store.Queries) error {
		verifier := req.Verifier
		if !validVerifier(verifier) {
			verifier = ""
		}
		issued, e := exchangeCLILogin(q, req.Device, req.Code, verifier, token, time.Now())
		if e != nil {
			return e
		}
		data, e := store.SessionData[store.IdentitySessionData](issued)
		if e != nil {
			return e
		}
		provider = store.ValueString(data.IdentityProvider)
		account, e = q.User(issued.UserID)
		return e
	})
	if err != nil {
		var api apiError
		if errors.As(err, &api) && api.status == 410 {
			return fail(410, "login request expired; run kioku login again")
		}
		return err
	}
	jsonOut(w, 200, map[string]string{"token": token, "provider": provider, "name": account.Data.Name, "email": account.Email})
	return nil
}
func pendingLoopback(q *store.Queries, id string) (store.SessionRow, store.CLILoginData, bool, error) {
	row, err := q.Session(auth.Hash(id))
	if errors.Is(err, sql.ErrNoRows) {
		return row, store.CLILoginData{}, false, nil
	}
	if err != nil {
		return row, store.CLILoginData{}, false, err
	}
	if row.Kind != "cli_login" {
		return row, store.CLILoginData{}, false, nil
	}
	data, err := store.SessionData[store.CLILoginData](row)
	if err != nil {
		return row, data, false, err
	}
	return row, data, row.Status == "pending" && row.UserID == "" && row.Expires > time.Now().UnixMilli() && store.ValueString(data.LoopbackChallenge) != "", nil
}
func (s *Server) loopbackFailure(w http.ResponseWriter, r *http.Request, id, reason string) error {
	target := ""
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		_, d, ok, e := pendingLoopback(q, id)
		if e != nil {
			return e
		}
		if ok {
			target = store.ValueString(d.LoopbackRedirectURI) + "?" + url.Values{"error": {reason}, "state": {store.ValueString(d.LoopbackState)}}.Encode()
		}
		return nil
	}); err != nil {
		return err
	}
	if target == "" {
		return loopbackExpired(w)
	}
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request, _ string) error {
	provider := r.PathValue("provider")
	p, ok := s.Config.Providers[provider]
	if !ok || p.ClientID == "" {
		return fail(400, "provider is not configured")
	}
	returnPath, err := authorizeReturn(r.URL.Query().Get("return"))
	if err != nil {
		return err
	}
	payload := auth.OAuthCookie{Provider: provider, Verifier: auth.Token(), ReturnPath: store.NullableString(returnPath), CLILogin: store.NullableString(r.URL.Query().Get("device")), Expires: time.Now().Add(10 * time.Minute).UnixMilli()}
	if payload.CLILogin != nil {
		if err = s.Store.Read(r.Context(), func(q *store.Queries) error {
			_, _, ok, e := pendingLoopback(q, *payload.CLILogin)
			if e != nil {
				return e
			}
			if !ok {
				return fail(410, "login request expired")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	cookie, err := s.Cipher.SealOAuthCookie(payload)
	if err != nil {
		return err
	}
	s.setOAuthCookie(w, cookie, 600)
	http.Redirect(w, r, p.URL(auth.Hash(cookie), payload.Verifier), http.StatusFound)
	return nil
}
func identityData(cipher *auth.Cipher, credential auth.Credential, uid, provider, id string) store.IdentitySessionData {
	tokens := cipher.SealIdentityTokens(credential, uid, provider, id)
	return store.IdentitySessionData{IdentityProvider: &provider, IdentityID: &id, IdentityAccessToken: tokens.AccessToken, IdentityRefreshToken: tokens.RefreshToken, IdentityExpiresAt: tokens.ExpiresAt}
}
func (s *Server) setOAuthCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "kioku_oauth", Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request, _ string) error {
	cookie, err := r.Cookie("kioku_oauth")
	if err != nil {
		return fail(403, "OAuth state mismatch; restart login")
	}
	pending, err := s.Cipher.OpenOAuthCookie(cookie.Value, r.URL.Query().Get("state"), r.PathValue("provider"), time.Now().UnixMilli())
	if err != nil {
		return fail(403, "OAuth request expired")
	}
	if _, err := authorizeReturn(store.ValueString(pending.ReturnPath)); err != nil {
		return err
	}
	device := store.ValueString(pending.CLILogin)
	if device != "" && r.URL.Query().Has("error") {
		return s.loopbackFailure(w, r, device, "access_denied")
	}
	p := s.Config.Providers[pending.Provider]
	credential, identity, err := p.Exchange(r.Context(), r.URL.Query().Get("code"), pending.Verifier)
	if err != nil {
		if device != "" {
			return s.loopbackFailure(w, r, device, "server_error")
		}
		return fail(502, "OAuth exchange failed; restart login")
	}
	var completed oauthCompletion
	var signup, loopbackOrigin string
	err = s.Store.Write(r.Context(), func(q *store.Queries) error {
		var login store.SessionRow
		var loginData store.CLILoginData
		if device != "" {
			row, data, ok, e := pendingLoopback(q, device)
			if e != nil {
				return e
			}
			if !ok {
				return errLoopbackExpired
			}
			login, loginData = row, data
			loopbackOrigin = redirectOrigin(store.ValueString(data.LoopbackRedirectURI))
		}
		user, e := q.UserByIdentity(pending.Provider, identity.ID)
		if errors.Is(e, sql.ErrNoRows) {
			if !s.Config.SignupAllowlist.allows(pending.Provider, identity.HostedDomain) {
				return errSignupDenied
			}
			signup = auth.Token()
			return s.beginSignup(q, signup, pending, identity.Identity, credential)
		}
		if e != nil {
			return e
		}
		user.Email = identity.Email
		user.Data.Name = identity.Name
		if e = q.PutUser(user); e != nil {
			return e
		}
		data := identityData(s.Cipher, credential, user.ID, pending.Provider, identity.ID)
		completed, e = completeOAuth(q, user.ID, data, login, loginData)
		return e
	})
	if errors.Is(err, errSignupDenied) {
		s.setOAuthCookie(w, "", -1)
		if device != "" {
			return s.loopbackFailure(w, r, device, "access_denied")
		}
		return renderStatus(w, http.StatusForbidden, "アカウントを作成できません", struct{ Message string }{signupDeniedMessage}, messageTemplate)
	}
	if errors.Is(err, errLoopbackExpired) {
		s.setOAuthCookie(w, "", -1)
		return loopbackExpired(w)
	}
	if err != nil {
		return err
	}
	s.setOAuthCookie(w, "", -1)
	if signup != "" {
		s.setSignupCookie(w, signup, 600)
		return signupPage(w, http.StatusOK, auth.Hash(signup), defaultUserSlug(identity.Email), "", loopbackOrigin)
	}
	return s.finishOAuth(w, r, completed, store.ValueString(pending.ReturnPath))
}

var errLoopbackExpired = errors.New("CLI login expired")

type oauthCompletion struct {
	token, loopbackURL string
}

func completeOAuth(q *store.Queries, uid string, data store.IdentitySessionData, login store.SessionRow, loginData store.CLILoginData) (oauthCompletion, error) {
	var result oauthCompletion
	now := time.Now()
	if login.ID != "" {
		if login.Expires <= now.UnixMilli() {
			return result, errLoopbackExpired
		}
		code := "kioku_loopback." + auth.Token()
		login.UserID = uid
		login.Expires = now.Add(time.Minute).UnixMilli()
		loginData.IdentitySessionData = data
		loginData.LoopbackCodeHash = store.NullableString(auth.Hash(code))
		var err error
		login.Data, err = store.EncodeSessionData(loginData)
		if err != nil {
			return result, err
		}
		if err = q.PutSession(login); err != nil {
			return result, err
		}
		result.loopbackURL = store.ValueString(loginData.LoopbackRedirectURI) + "?" + url.Values{"code": {code}, "state": {store.ValueString(loginData.LoopbackState)}}.Encode()
	}
	raw, err := store.EncodeSessionData(data)
	if err != nil {
		return result, err
	}
	result.token = auth.Token()
	return result, q.InsertSession(store.SessionRow{ID: auth.Hash(result.token), Kind: "user", Status: "active", UserID: uid, Created: now.UnixMilli(), Expires: now.Add(30 * 24 * time.Hour).UnixMilli(), Data: raw})
}

func (s *Server) finishOAuth(w http.ResponseWriter, r *http.Request, result oauthCompletion, returnPath string) error {
	target, err := authorizeReturn(returnPath)
	if err != nil {
		return err
	}
	if target == "" {
		target = "/"
	}
	http.SetCookie(w, &http.Cookie{Name: "kioku_session", Value: result.token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	if result.loopbackURL != "" {
		http.Redirect(w, r, result.loopbackURL, http.StatusFound)
	} else {
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
	return nil
}

func (s *Server) currentEmail(r *http.Request, uid string) (string, error) {
	key, principal, err := s.session(r)
	if err != nil {
		return "", err
	}
	saved := principal.Identity
	providerName, id := store.ValueString(saved.IdentityProvider), store.ValueString(saved.IdentityID)
	if principal.UserID != uid || saved.IdentityAccessToken == nil || providerName == "" || id == "" {
		return "", fail(401, "sign in again to verify your email before accepting this invitation")
	}
	provider, ok := s.Config.Providers[providerName]
	if !ok {
		return "", fail(401, "sign in again with an available identity provider")
	}
	credential, err := s.Cipher.OpenIdentityTokens(auth.IdentityTokens{AccessToken: saved.IdentityAccessToken, RefreshToken: saved.IdentityRefreshToken, ExpiresAt: saved.IdentityExpiresAt}, uid, providerName, id)
	if err != nil {
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
	if identity.ID != id || identity.Email == "" {
		return "", fail(401, "identity no longer matches this session; sign in again")
	}
	err = s.Store.Write(r.Context(), func(q *store.Queries) error {
		row, e := q.Session(key)
		if errors.Is(e, sql.ErrNoRows) {
			return fail(401, "session changed; sign in again")
		}
		if e != nil {
			return e
		}
		current, e := store.SessionData[store.IdentitySessionData](row)
		if e != nil {
			return e
		}
		if row.UserID != uid || row.Expires <= time.Now().UnixMilli() || !equalOptional(current.IdentityAccessToken, saved.IdentityAccessToken) || !equalOptional(current.IdentityRefreshToken, saved.IdentityRefreshToken) {
			return fail(401, "session changed; sign in again")
		}
		identities, e := q.Identities(uid)
		if e != nil {
			return e
		}
		if identities[providerName] != id {
			return fail(401, "identity no longer linked to this account")
		}
		next := identityData(s.Cipher, fresh, uid, providerName, id)
		row.Data, e = store.EncodeSessionData(next)
		if e != nil {
			return e
		}
		if e = q.PutSession(row); e != nil {
			return e
		}
		user, e := q.User(uid)
		if e != nil {
			return e
		}
		user.Email = identity.Email
		user.Data.Name = identity.Name
		return q.PutUser(user)
	})
	if err != nil {
		return "", err
	}
	return identity.Email, nil
}
