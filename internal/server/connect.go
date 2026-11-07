package server

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

// A connect request is owned by an authenticated CLI. Provider authorization is
// only staged; it cannot change the account until that CLI redeems the code.
func (s *Server) connect(w http.ResponseWriter, r *http.Request, uid string) error {
	provider := r.PathValue("provider")
	if provider == "github" {
		return s.connectGitHub(w, r, uid)
	}
	p, ok := s.Config.Providers[provider]
	if !ok || p.ClientID == "" {
		return fail(400, "provider is not configured")
	}
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
	if req.Provider != provider || req.Method != "S256" || !validChallenge(req.Challenge) || !loopbackRedirect(req.RedirectURI) || len(req.State) < 1 || len(req.State) > 128 || !unreserved(req.State) {
		return fail(400, "connect requires provider, PKCE S256, loopback redirect_uri and state; update the CLI")
	}
	id := auth.Token()
	err := s.DB.Update(func(st *model.State) error {
		prune(st)
		if len(st.Devices) >= 10000 {
			return fail(429, "too many connection attempts")
		}
		st.Devices[id] = model.Device{Purpose: "connect", UserID: uid, IdentityProvider: provider, LoopbackChallenge: req.Challenge, LoopbackRedirectURI: req.RedirectURI, LoopbackState: req.State, Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 201, map[string]string{"device": id, "url": s.Config.BaseURL + "/auth/" + provider + "/start?connect=" + id})
	return nil
}

func pendingConnect(d model.Device) bool {
	return d.Purpose == "connect" && d.UserID != "" && d.LoopbackChallenge != "" && d.LoopbackCodeHash == "" && !d.Used && d.Expires.After(time.Now())
}

func (s *Server) connectCallback(w http.ResponseWriter, r *http.Request, pending model.OAuthState, credential string) error {
	var target string
	code := "kioku_connect." + auth.Token()
	err := s.DB.Update(func(st *model.State) error {
		d, ok := st.Devices[pending.Device]
		if !ok || !pendingConnect(d) || d.UserID != pending.UserID || d.IdentityProvider != pending.Provider {
			return fail(410, "connection request expired; run kioku connect again")
		}
		if _, ok := st.Users[d.UserID]; !ok {
			return fail(401, "user no longer exists")
		}
		d.IdentityCredential = s.Cipher.Seal(credential, "connect:"+pending.Device)
		d.LoopbackCodeHash = auth.Hash(code)
		d.Expires = time.Now().Add(time.Minute)
		st.Devices[pending.Device] = d
		target = d.LoopbackRedirectURI + "?" + url.Values{"code": {code}, "state": {d.LoopbackState}}.Encode()
		return nil
	})
	if err != nil {
		return err
	}
	// Do not create a browser login session or change login identities/profile.
	http.SetCookie(w, &http.Cookie{Name: "kioku_oauth", Value: "", Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}

func (s *Server) completeConnect(w http.ResponseWriter, r *http.Request, uid string) error {
	var req *struct {
		Device   string `json:"device"`
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
	}
	if err := decode(w, r, &req); err != nil || req == nil {
		return fail(400, "invalid request")
	}
	var problem error
	key, _ := s.session(r)
	err := s.DB.Update(func(st *model.State) error {
		d, ok := st.Devices[req.Device]
		session := st.Sessions[key]
		if session.UserID != uid || !session.Expires.After(time.Now()) {
			return fail(401, "login required")
		}
		if !ok || d.Purpose != "connect" || d.UserID != uid || d.IdentityProvider != r.PathValue("provider") {
			return fail(403, "connection request does not belong to this user and provider")
		}
		// Consume on failure too, and discard staged credentials on every attempt.
		delete(st.Devices, req.Device)
		if d.Used || !d.Expires.After(time.Now()) || d.LoopbackCodeHash == "" || d.LoopbackCodeHash != auth.Hash(req.Code) || !validVerifier(req.Verifier) || auth.Challenge(req.Verifier) != d.LoopbackChallenge || d.IdentityCredential == "" {
			problem = fail(410, "connection request expired; run kioku connect again")
			return nil
		}
		u, ok := st.Users[uid]
		if !ok {
			problem = fail(401, "user no longer exists")
			return nil
		}
		credential, err := s.Cipher.Open(d.IdentityCredential, "connect:"+req.Device)
		if err != nil {
			problem = fail(410, "connection request expired; run kioku connect again")
			return nil
		}
		if u.Connections == nil {
			u.Connections = map[string]string{}
		}
		u.Connections[d.IdentityProvider] = s.Cipher.Seal(credential, uid+":"+d.IdentityProvider)
		st.Users[uid] = u
		return nil
	})
	if err != nil {
		return err
	}
	if problem != nil {
		return problem
	}
	jsonOut(w, 200, map[string]any{"connected": true, "provider": r.PathValue("provider")})
	return nil
}
