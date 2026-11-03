package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request, _ string) error {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	p, ok := s.Config.Providers[req.Provider]
	if !ok || p.ClientID == "" {
		return fail(400, "provider is not configured")
	}
	id, secret := auth.Token(), auth.Token()
	err := s.DB.Update(func(st *model.State) error {
		prune(st)
		if len(st.Devices) > 10000 {
			return fail(429, "too many login attempts")
		}
		st.Devices[id] = model.Device{SecretHash: auth.Hash(secret), Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 201, map[string]string{"device": id, "secret": secret, "url": s.Config.BaseURL + "/auth/" + req.Provider + "/start?device=" + id})
	return nil
}
func (s *Server) devicePoll(w http.ResponseWriter, r *http.Request, _ string) error {
	var req struct {
		Device string `json:"device"`
		Secret string `json:"secret"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	token := auth.Token()
	ready := false
	err := s.DB.Update(func(st *model.State) error {
		d, ok := st.Devices[req.Device]
		if !ok || d.SecretHash != auth.Hash(req.Secret) || d.Used || d.Expires.Before(time.Now()) {
			return fail(410, "login request expired")
		}
		if d.UserID == "" {
			return nil
		}
		d.Used = true
		st.Devices[req.Device] = d
		st.Sessions[auth.Hash(token)] = model.Session{Created: time.Now().UTC(), UserID: d.UserID, Expires: time.Now().Add(30 * 24 * time.Hour), IdentityProvider: d.IdentityProvider, IdentityID: d.IdentityID, IdentityCredential: d.IdentityCredential}
		ready = true
		return nil
	})
	if err != nil {
		return err
	}
	if !ready {
		jsonOut(w, 202, map[string]string{"status": "pending"})
	} else {
		jsonOut(w, 200, map[string]string{"token": token})
	}
	return nil
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request, uid string) error {
	provider := r.PathValue("provider")
	p, ok := s.Config.Providers[provider]
	if !ok || p.ClientID == "" {
		return fail(400, "provider is not configured")
	}
	// A bearer-only CLI cannot share its credential with the browser. A one-use
	// device ticket authenticates the connect flow without exposing the API token.
	id, secret := auth.Token(), auth.Token()
	err := s.DB.Update(func(st *model.State) error {
		prune(st)
		st.Devices[id] = model.Device{UserID: uid, SecretHash: auth.Hash(secret), Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 201, map[string]string{"url": s.Config.BaseURL + "/auth/" + provider + "/start?connect=" + id + "&ticket=" + secret})
	return nil
}
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request, uid string) error {
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
			if !ok || d.Used || d.Expires.Before(time.Now()) || d.UserID == "" || d.SecretHash != auth.Hash(r.URL.Query().Get("ticket")) {
				return fail(403, "invalid connection ticket")
			}
			if uid != "" && uid != d.UserID {
				return fail(403, "browser is logged in as a different user")
			}
			pending.Purpose = "connect"
			pending.UserID = d.UserID
			pending.Device = ""
			d.Used = true
			st.Devices[id] = d
		}

		if pending.Device != "" {
			d, ok := st.Devices[pending.Device]
			if !ok || d.Used || d.Expires.Before(time.Now()) || d.UserID != "" {
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
	p := s.Config.Providers[pending.Provider]
	credential, err := p.Exchange(r.Context(), r.URL.Query().Get("code"), pending.Verifier)
	if err != nil {
		return fail(502, "OAuth exchange failed; restart login")
	}
	identity, err := p.Identity(r.Context(), credential.AccessToken)
	if err != nil {
		return fail(502, "could not verify provider identity and primary email")
	}
	var uid string
	token := auth.Token()
	err = s.DB.Update(func(st *model.State) error {
		for id, u := range st.Users {
			if u.Identities[pending.Provider] == identity.ID {
				uid = id
				break
			}
		}
		if pending.Purpose == "connect" {
			if uid != "" && uid != pending.UserID {
				return fail(409, "provider account belongs to another user")
			}
			uid = pending.UserID
			u, ok := st.Users[uid]
			if !ok {
				return fail(401, "user no longer exists")
			}
			if existing := u.Identities[pending.Provider]; existing != "" && existing != identity.ID {
				return fail(409, "disconnecting or replacing a linked identity requires account recovery")
			}
			if u.Identities == nil {
				u.Identities = map[string]string{}
			}
			if u.Connections == nil {
				u.Connections = map[string]string{}
			}
			u.Identities[pending.Provider] = identity.ID
			b, _ := json.Marshal(credential)
			u.Connections[pending.Provider] = s.Cipher.Seal(string(b), uid+":"+pending.Provider)
			st.Users[uid] = u
		} else if uid == "" {
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
			d, ok := st.Devices[pending.Device]
			if !ok || d.Used || d.UserID != "" || d.Expires.Before(time.Now()) {
				return fail(410, "CLI login expired; restart login")
			}
			d.UserID = uid
			d.IdentityProvider = pending.Provider
			d.IdentityID = identity.ID
			d.IdentityCredential = sealedIdentity
			st.Devices[pending.Device] = d
		}
		st.Sessions[auth.Hash(token)] = model.Session{Created: time.Now().UTC(), UserID: uid, Expires: time.Now().Add(30 * 24 * time.Hour), IdentityProvider: pending.Provider, IdentityID: identity.ID, IdentityCredential: sealedIdentity}
		return nil
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "kioku_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	http.SetCookie(w, &http.Cookie{Name: "kioku_oauth", Value: "", Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
	if pending.Device != "" {
		return render(w, "CLI login complete", struct{ Message string }{"認証が完了しました。CLIに戻ってください。"}, messageTemplate)
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
		if v.Expires.Before(now) {
			delete(st.Devices, k)
		}
	}
	for k, v := range st.Sessions {
		if v.Expires.Before(now) {
			delete(st.Sessions, k)
		}
	}
}
func (s *Server) credential(r *http.Request, uid, provider string) (string, error) {
	if _, p := s.session(r); p.AgentID != "" {
		return "", fail(403, "agent credentials cannot use personal provider connections")
	}
	return s.providerCredential(r.Context(), uid, provider)
}

func (s *Server) providerCredential(ctx context.Context, uid, provider string) (string, error) {
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
	fresh, err := p.Refresh(ctx, c)
	if err != nil {
		return "", fail(400, "connection expired; run kioku connect "+provider)
	}
	if fresh.AccessToken != c.AccessToken {
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
	return fresh.AccessToken, nil
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
