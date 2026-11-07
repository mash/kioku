package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

func (s *Server) githubAvailable() error {
	if !s.Config.GitHubApp.Configured() || !s.Config.GitHubApp.Enabled {
		return fail(503, "GitHub App is not configured or enabled; contact the operator")
	}
	return nil
}
func (s *Server) connectGitHub(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.githubAvailable(); err != nil {
		return err
	}
	key, session := s.session(r)
	if uid == "" || session.AgentID != "" || session.JobID != "" {
		return fail(401, "human login required")
	}
	var req struct {
		Provider  string `json:"provider"`
		Challenge string `json:"code_challenge"`
		Method    string `json:"code_challenge_method"`
		Redirect  string `json:"redirect_uri"`
		State     string `json:"state"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Provider != "github" || req.Method != "S256" || !validChallenge(req.Challenge) || !loopbackRedirect(req.Redirect) || len(req.State) < 1 || len(req.State) > 128 || !unreserved(req.State) {
		return fail(400, "connect requires provider, PKCE S256, loopback redirect_uri and state")
	}
	id := auth.Token()
	if err := s.DB.Update(func(st *model.State) error {
		prune(st)
		if len(st.Devices) >= 10000 {
			return fail(429, "too many connection attempts")
		}
		current := st.Sessions[key]
		if current.UserID != uid || !current.Expires.After(time.Now()) {
			return fail(401, "login required")
		}
		st.Devices[id] = model.Device{Purpose: "github-app", UserID: uid, IdentityProvider: "github", LoopbackChallenge: req.Challenge, LoopbackRedirectURI: req.Redirect, LoopbackState: req.State, Expires: time.Now().Add(10 * time.Minute)}
		st.GitHubAppConnectAttempts[id] = model.GitHubAppConnectAttempt{UserID: uid, SessionHash: key, Version: st.GitHubAppConnections[uid].Version, Status: "authorizing"}
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]string{"device": id, "url": s.Config.BaseURL + "/auth/github-app/start?connect=" + id})
	return nil
}
func (s *Server) githubStart(w http.ResponseWriter, r *http.Request, _ string) error {
	if err := s.githubAvailable(); err != nil {
		return err
	}
	id := r.URL.Query().Get("connect")
	nonce, state := auth.Token(), auth.Token()
	if err := s.DB.Update(func(st *model.State) error {
		a, ok := st.GitHubAppConnectAttempts[id]
		d := st.Devices[id]
		if !ok || d.Purpose != "github-app" {
			return fail(403, "invalid connection request")
		}
		if !d.Expires.After(time.Now()) {
			return fail(410, "connection expired; run kioku connect github again")
		}
		if a.Status != "authorizing" || a.BrowserHash != "" {
			return fail(403, "connection already opened; restart connect")
		}
		a.BrowserHash = auth.Hash(nonce)
		a.StateHash = auth.Hash(state)
		st.GitHubAppConnectAttempts[id] = a
		return nil
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "kioku_github_app", Value: id + "." + nonce, Path: "/auth/github-app/", HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	target := "https://github.com/login/oauth/authorize?" + url.Values{"client_id": {s.Config.GitHubApp.ClientID}, "redirect_uri": {s.Config.BaseURL + "/auth/github-app/callback"}, "state": {state}}.Encode()
	http.Redirect(w, r, target, 302)
	return nil
}
func (s *Server) githubBrowser(r *http.Request) (string, model.GitHubAppConnectAttempt, error) {
	cookie, err := r.Cookie("kioku_github_app")
	if err != nil {
		return "", model.GitHubAppConnectAttempt{}, fail(403, "GitHub App browser binding missing")
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return "", model.GitHubAppConnectAttempt{}, fail(403, "GitHub App browser binding invalid")
	}
	var a model.GitHubAppConnectAttempt
	err = s.DB.View(func(st model.State) error {
		var ok bool
		a, ok = st.GitHubAppConnectAttempts[parts[0]]
		if !ok || !st.Devices[parts[0]].Expires.After(time.Now()) {
			return fail(410, "connection expired; run kioku connect github again")
		}
		if a.BrowserHash == "" || a.BrowserHash != auth.Hash(parts[1]) {
			return fail(403, "GitHub App browser binding mismatch")
		}
		session := st.Sessions[a.SessionHash]
		if session.UserID != a.UserID || !session.Expires.After(time.Now()) {
			return fail(401, "starting session expired; log in and reconnect")
		}
		return nil
	})
	return parts[0], a, err
}
func (s *Server) githubFailure(w http.ResponseWriter, r *http.Request, id, reason string) error {
	var target string
	if err := s.DB.Update(func(st *model.State) error {
		d, ok := st.Devices[id]
		if ok && loopbackRedirect(d.LoopbackRedirectURI) {
			target = d.LoopbackRedirectURI + "?" + url.Values{"error": {reason}, "state": {d.LoopbackState}}.Encode()
		}
		delete(st.Devices, id)
		delete(st.GitHubAppConnectAttempts, id)
		githubapp.Count(st, fmt.Sprintf(`kioku_github_connect_total{result=%q}`, reason), 1)
		return nil
	}); err != nil {
		return err
	}
	if target == "" {
		return fail(410, "connection expired; run kioku connect github again")
	}
	http.Redirect(w, r, target, 302)
	return nil
}
func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request, _ string) error {
	if err := s.githubAvailable(); err != nil {
		return err
	}
	id, a, err := s.githubBrowser(r)
	if err != nil {
		return err
	}
	state := r.URL.Query().Get("state")
	if a.Status != "authorizing" || a.StateHash == "" || state == "" || a.StateHash != auth.Hash(state) {
		return fail(403, "GitHub App state mismatch")
	}
	if err = s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnectAttempts[id]
		if v.Status != "authorizing" || v.StateHash != a.StateHash {
			return fail(403, "GitHub App state already consumed")
		}
		v.StateHash = ""
		v.Status = "exchanging"
		st.GitHubAppConnectAttempts[id] = v
		return nil
	}); err != nil {
		return err
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		return s.githubFailure(w, r, id, "denied")
	}
	credential, err := s.GitHubApp.Client.Exchange(r.Context(), r.URL.Query().Get("code"), "")
	if err != nil {
		return s.githubFailure(w, r, id, "failed")
	}
	user, err := s.GitHubApp.Client.User(r.Context(), runner.Secret(credential.AccessToken))
	if err != nil {
		return s.githubFailure(w, r, id, "failed")
	}
	b, _ := json.Marshal(credential)
	if err = s.DB.Update(func(st *model.State) error {
		v, ok := st.GitHubAppConnectAttempts[id]
		if !ok || v.Status != "exchanging" || !st.Devices[id].Expires.After(time.Now()) {
			return fail(410, "connection expired")
		}
		v.EncryptedCredential = s.Cipher.Seal(string(b), "github-connect:"+id)
		v.GitHubUserID = user.ID
		v.Login = user.Login
		v.AccessExpiresAt = credential.AccessExpiresAt
		v.RefreshExpiresAt = credential.RefreshExpiresAt
		v.Status = "selecting"
		st.GitHubAppConnectAttempts[id] = v
		return nil
	}); err != nil {
		return err
	}
	http.Redirect(w, r, "/auth/github-app/installations", 302)
	return nil
}
func (s *Server) githubStagedToken(id string, a model.GitHubAppConnectAttempt) (runner.Secret, error) {
	raw, err := s.Cipher.Open(a.EncryptedCredential, "github-connect:"+id)
	var c githubapp.UserCredential
	if err != nil || json.Unmarshal([]byte(raw), &c) != nil || c.AccessToken == "" || !c.AccessExpiresAt.After(time.Now()) {
		return "", fail(410, "staged authorization expired; reconnect")
	}
	return runner.Secret(c.AccessToken), nil
}
func (s *Server) githubInstallations(w http.ResponseWriter, r *http.Request, _ string) error {
	if err := s.githubAvailable(); err != nil {
		return err
	}
	id, a, err := s.githubBrowser(r)
	if err != nil {
		return err
	}
	if a.Status != "selecting" {
		return fail(410, "selection expired; restart connect")
	}
	token, err := s.githubStagedToken(id, a)
	if err != nil {
		return err
	}
	if r.Method == "POST" {
		return s.githubSelect(w, r, id, a, token)
	}
	installations, err := s.GitHubApp.Client.Installations(r.Context(), token)
	if err != nil {
		return fail(503, "GitHub installation lookup failed; retry selection")
	}
	candidates := []githubapp.Installation{}
	for _, v := range installations {
		if v.AppID == s.Config.GitHubApp.ID && v.SuspendedAt == nil {
			candidates = append(candidates, v)
		}
	}
	csrf, setup := auth.Token(), auth.Token()
	oldLogin := ""
	if err = s.DB.Update(func(st *model.State) error {
		v, ok := st.GitHubAppConnectAttempts[id]
		if !ok || v.Status != "selecting" {
			return fail(410, "selection expired")
		}
		v.CSRFHash = auth.Hash(csrf)
		v.SetupHash = auth.Hash(setup)
		st.GitHubAppConnectAttempts[id] = v
		oldLogin = st.GitHubAppConnections[a.UserID].Login
		return nil
	}); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return githubSelectionPage.Execute(w, map[string]any{"App": s.Config.GitHubApp.Slug, "Origin": s.Config.BaseURL, "Login": a.Login, "OldLogin": oldLogin, "Installations": candidates, "CSRF": csrf, "InstallURL": "https://github.com/apps/" + s.Config.GitHubApp.Slug + "/installations/new?state=" + setup, "Approval": r.URL.Query().Get("approval") == "required"})
}

var githubSelectionPage = template.Must(template.New("github-app").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Connect GitHub sources</title><h1>Connect {{.App}}</h1><p>Kioku server: {{.Origin}}. GitHub user: {{.Login}}.</p>{{if .OldLogin}}<p>This replaces the existing connection for {{.OldLogin}} after the CLI completes.</p>{{end}}<p>Select installations to read with Contents, Metadata and Issues read-only access. No installation is selected by default. Expires 10 minutes after starting; finish in the CLI within 1 minute after selection.</p>{{if .Approval}}<p>approval_required: ask your organization administrator to approve installation, then run kioku connect github again.</p>{{end}}{{if .Installations}}<form method="post" action="/auth/github-app/installations"><input type="hidden" name="csrf" value="{{.CSRF}}">{{range .Installations}}<p><label><input type="checkbox" name="installation" value="{{.ID}}">{{.Account.Login}}</label></p>{{end}}<button type="submit">Connect selected installations</button></form>{{else}}<p>needs_installation: install the App, or ask your administrator to approve it, then reconnect.</p>{{end}}<p><a href="{{.InstallURL}}">Install or update the App</a></p><form method="post" action="/auth/github-app/installations"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="cancel" value="true">Cancel</button></form></html>`))

func (s *Server) githubSetup(w http.ResponseWriter, r *http.Request, _ string) error {
	if err := s.githubAvailable(); err != nil {
		return err
	}
	if _, err := r.Cookie("kioku_github_app"); err != nil {
		return fail(410, "no live connection ticket; run kioku connect github")
	}
	id, a, err := s.githubBrowser(r)
	if err != nil {
		return err
	}
	state := r.URL.Query().Get("state")
	if state == "" || a.SetupHash == "" || a.SetupHash != auth.Hash(state) {
		return fail(403, "GitHub App setup binding mismatch")
	}
	if err = s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnectAttempts[id]
		if v.SetupHash != a.SetupHash {
			return fail(403, "setup state consumed")
		}
		v.SetupHash = ""
		st.GitHubAppConnectAttempts[id] = v
		return nil
	}); err != nil {
		return err
	}
	token, err := s.githubStagedToken(id, a)
	if err != nil {
		return err
	}
	if _, err = s.GitHubApp.Client.Installations(r.Context(), token); err != nil {
		return fail(503, "GitHub installation lookup failed")
	}
	target := "/auth/github-app/installations"
	if r.URL.Query().Get("setup_action") == "request" {
		target += "?approval=required"
	}
	http.Redirect(w, r, target, 302)
	return nil
}
func (s *Server) githubSelect(w http.ResponseWriter, r *http.Request, id string, a model.GitHubAppConnectAttempt, token runner.Secret) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if r.ParseForm() != nil {
		return fail(400, "invalid selection")
	}
	if a.CSRFHash == "" || auth.Hash(r.PostForm.Get("csrf")) != a.CSRFHash {
		return fail(403, "selection CSRF mismatch")
	}
	if err := s.DB.Update(func(st *model.State) error {
		v := st.GitHubAppConnectAttempts[id]
		if v.Status != "selecting" || v.CSRFHash != a.CSRFHash {
			return fail(403, "selection already submitted")
		}
		v.CSRFHash = ""
		st.GitHubAppConnectAttempts[id] = v
		return nil
	}); err != nil {
		return err
	}
	if r.PostForm.Get("cancel") == "true" {
		return s.githubFailure(w, r, id, "canceled")
	}
	wanted := map[int64]bool{}
	for _, raw := range r.PostForm["installation"] {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 || wanted[v] {
			return fail(403, "invalid installation selection")
		}
		wanted[v] = true
	}
	if len(wanted) == 0 {
		return fail(403, "select at least one installation")
	}
	all, err := s.GitHubApp.Client.Installations(r.Context(), token)
	if err != nil {
		return fail(503, "GitHub installation lookup failed")
	}
	selected := map[int64]model.GitHubAppInstallation{}
	for _, v := range all {
		if !wanted[v.ID] {
			continue
		}
		if v.AppID != s.Config.GitHubApp.ID || v.SuspendedAt != nil {
			return fail(403, "installation is unavailable")
		}
		verified, e := s.GitHubApp.Client.Installation(r.Context(), v.ID)
		if e != nil {
			return fail(503, "GitHub installation verification failed")
		}
		if e = githubapp.ValidateInstallation(verified, s.Config.GitHubApp.ID, v.Account.ID); e != nil || verified.ID != v.ID {
			return fail(403, "installation permissions unavailable")
		}
		if _, e = s.GitHubApp.Client.Repositories(r.Context(), token, v.ID); e != nil {
			return fail(503, "GitHub repository lookup failed")
		}
		selected[v.ID] = model.GitHubAppInstallation{ID: v.ID, AccountID: v.Account.ID, Login: v.Account.Login, Status: "active", CheckedAt: time.Now().UTC()}
	}
	if len(selected) != len(wanted) {
		return fail(403, "installation does not belong to this GitHub user")
	}
	code := "kioku_connect." + auth.Token()
	target := ""
	if err = s.DB.Update(func(st *model.State) error {
		v, ok := st.GitHubAppConnectAttempts[id]
		d := st.Devices[id]
		if !ok || v.Status != "selecting" || !d.Expires.After(time.Now()) {
			return fail(410, "selection expired")
		}
		v.Installations = selected
		v.Status = "staged"
		st.GitHubAppConnectAttempts[id] = v
		d.LoopbackCodeHash = auth.Hash(code)
		d.Expires = time.Now().Add(time.Minute)
		st.Devices[id] = d
		target = d.LoopbackRedirectURI + "?" + url.Values{"code": {code}, "state": {d.LoopbackState}}.Encode()
		return nil
	}); err != nil {
		return err
	}
	http.Redirect(w, r, target, 302)
	return nil
}
func (s *Server) completeGitHub(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Device   string `json:"device"`
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	key, principal := s.session(r)
	var problem error
	unlock, err := s.lockGitHubOwner(r.Context(), uid)
	if err != nil {
		return err
	}
	defer unlock()
	err = s.DB.Update(func(st *model.State) error {
		a, ok := st.GitHubAppConnectAttempts[req.Device]
		d := st.Devices[req.Device]
		if !ok {
			if d.Purpose == "connect" {
				problem = fail(403, "wrong connection provider")
			} else {
				problem = fail(410, "connection expired or already used")
			}
			return nil
		}
		delete(st.GitHubAppConnectAttempts, req.Device)
		delete(st.Devices, req.Device)
		session := st.Sessions[key]
		switch {
		case uid == "" || session.UserID != uid || !session.Expires.After(time.Now()) || principal.AgentID != "" || principal.JobID != "":
			problem = fail(401, "starting session expired; log in and reconnect")
		case a.UserID != uid || a.SessionHash != key || d.IdentityProvider != "github":
			problem = fail(403, "complete must use the starting user and session")
		case a.Status != "staged" || !d.Expires.After(time.Now()) || d.LoopbackCodeHash != auth.Hash(req.Code) || !validVerifier(req.Verifier) || auth.Challenge(req.Verifier) != d.LoopbackChallenge:
			problem = fail(410, "connection proof invalid or expired; reconnect")
		case st.GitHubAppConnections[uid].Version != a.Version:
			githubapp.Count(st, "kioku_github_version_conflicts_total", 1)
			problem = fail(409, "connection changed; run kioku connect github again")
		case !s.Config.GitHubApp.Enabled:
			problem = fail(503, "GitHub App disabled")
		default:
			raw, e := s.Cipher.Open(a.EncryptedCredential, "github-connect:"+req.Device)
			if e != nil {
				problem = fail(410, "staged authorization unavailable")
				break
			}
			invalidateGitHub(st, uid, "", time.Now().UTC())
			st.GitHubAppConnections[uid] = model.GitHubAppConnection{AppID: s.Config.GitHubApp.ID, GitHubUserID: a.GitHubUserID, Login: a.Login, Version: a.Version + 1, Status: "active", EncryptedCredential: s.Cipher.Seal(raw, "github-app:"+uid), AccessExpiresAt: a.AccessExpiresAt, RefreshExpiresAt: a.RefreshExpiresAt, CredentialGeneration: 1, Installations: a.Installations}
		}
		result := "success"
		if problem != nil {
			result = "rejected"
		}
		githubapp.Count(st, fmt.Sprintf(`kioku_github_connect_total{result=%q}`, result), 1)
		return nil
	})
	if err != nil {
		return err
	}
	if problem != nil {
		return problem
	}
	s.notify()
	jsonOut(w, 200, map[string]any{"connected": true, "provider": "github"})
	return nil
}

// Lock the same boundary used while publishing each object. Never wait for a
// remote runner or GitHub request while holding these locks or the DB mutex.
func (s *Server) lockGitHubOwner(ctx context.Context, uid string) (func(), error) {
	var ids []string
	if err := s.DB.View(func(st model.State) error {
		for id, p := range st.Projects {
			if p.Members[uid] == "owner" {
				ids = append(ids, id)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	var unlocks []func()
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, id := range ids {
		unlock, err := s.lockProject(ctx, id)
		if err != nil {
			release()
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return release, nil
}
func invalidateGitHub(st *model.State, uid, pid string, now time.Time) {
	for id, a := range st.GitHubAttemptAuthorizations {
		if a.OwnerID != uid || (pid != "" && a.Key.ProjectID != pid) {
			continue
		}
		a.InvalidatedAt = now
		st.GitHubAttemptAuthorizations[id] = a
		j := st.Jobs[a.Key.JobID]
		if j.Run != nil && j.Run.Attempt.ID == a.Key.AttemptID && j.Status == "running" {
			j.Run.AbortReason = "GitHub source authorization changed"
			revokeJobToken(st, j.ID)
			st.Jobs[j.ID] = j
			githubapp.Count(st, "kioku_github_interrupted_attempts_total", 1)
		}
	}
}
func disconnectGitHub(st *model.State, uid string) {
	invalidateGitHub(st, uid, "", time.Now().UTC())
	c := st.GitHubAppConnections[uid]
	st.GitHubAppConnections[uid] = model.GitHubAppConnection{Version: c.Version + 1, Status: "inactive"}
	for id, a := range st.GitHubAppConnectAttempts {
		if a.UserID == uid {
			delete(st.GitHubAppConnectAttempts, id)
			delete(st.Devices, id)
		}
	}
}
func (s *Server) legacyGitHubConnect(w http.ResponseWriter, r *http.Request) error {
	id := r.URL.Query().Get("connect")
	valid := false
	if err := s.DB.Update(func(st *model.State) error {
		d := st.Devices[id]
		valid = d.Purpose == "connect" && d.IdentityProvider == "github" && d.Expires.After(time.Now())
		for k, a := range st.OAuth {
			if a.Provider == "github" && a.Purpose == "connect" && a.Device == id {
				delete(st.OAuth, k)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !valid {
		return fail(410, "old GitHub source connection retired; run kioku connect github again")
	}
	return s.githubFailure(w, r, id, "restart_required")
}
func connections(st model.State, uid string) []string {
	out := []string{}
	for p := range st.Users[uid].Connections {
		if p != "github" {
			out = append(out, p)
		}
	}
	if c := st.GitHubAppConnections[uid]; c.Status != "" && c.Status != "inactive" {
		out = append(out, "github")
	}
	sort.Strings(out)
	return out
}
func (s *Server) githubAccount(st model.State, uid string) map[string]any {
	c := st.GitHubAppConnections[uid]
	status := c.Status
	if status == "" {
		status = "not_connected"
	}
	installations := []model.GitHubAppInstallation{}
	for _, v := range c.Installations {
		installations = append(installations, v)
	}
	sort.Slice(installations, func(i, j int) bool { return installations[i].ID < installations[j].ID })
	return map[string]any{"configured": s.Config.GitHubApp.Configured(), "enabled": s.Config.GitHubApp.Enabled, "status": status, "login": c.Login, "installations": installations, "slug": s.Config.GitHubApp.Slug}
}

func (s *Server) githubCleanupPending(uid string) bool {
	pending := false
	_ = s.DB.View(func(st model.State) error {
		for _, v := range st.GitHubTokenAttempts {
			if v.OwnerID == uid && v.EncryptedToken != "" {
				pending = true
			}
			if v.OwnerID == uid && (v.Status == "issuing" || v.Status == "issuance_unknown" || v.Status == "expiry_wait") {
				pending = true
			}
		}
		return nil
	})
	return pending
}
