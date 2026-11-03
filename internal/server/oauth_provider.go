package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

const connectorScope = "cli_login"

func authorizeReturn(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.Path != "/oauth/authorize" || u.RawPath != "" || u.Fragment != "" {
		return "", fail(400, "invalid login return path")
	}
	return u.String(), nil
}

func (s *Server) oauthResourceMetadata(w http.ResponseWriter, r *http.Request, _ string) error {
	jsonOut(w, 200, map[string]any{"resource": s.Config.BaseURL + "/mcp", "authorization_servers": []string{s.Config.BaseURL}, "scopes_supported": []string{connectorScope}})
	return nil
}
func (s *Server) oauthServerMetadata(w http.ResponseWriter, r *http.Request, _ string) error {
	base := s.Config.BaseURL
	jsonOut(w, 200, map[string]any{
		"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token", "registration_endpoint": base + "/oauth/register",
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{connectorScope},
	})
	return nil
}

// Only a literal host and port can be embedded as a CSP source. In particular,
// a registered URI must never inject a directive, a second source or a wildcard.
var literalRedirectHost = regexp.MustCompile(`^[a-zA-Z0-9.\[\]:-]+$`)

func redirectOrigin(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || !literalRedirectHost.MatchString(u.Host) || u.User != nil || u.Fragment != "" {
		return ""
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request, _ string) error {
	var req struct {
		Name          string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		Scope         string   `json:"scope"`
	}
	// RFC 7591 requires ignoring metadata this server does not understand.
	// Keep the shared decoder's body-size and single-JSON-value checks.
	var body json.RawMessage
	if err := decode(w, r, &body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fail(400, "invalid client metadata")
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 || req.AuthMethod != "" && req.AuthMethod != "none" || req.Scope != "" && req.Scope != connectorScope {
		return fail(400, "invalid client metadata")
	}
	for _, uri := range req.RedirectURIs {
		if redirectOrigin(uri) == "" {
			return fail(400, "invalid redirect URI")
		}
	}
	for _, grant := range req.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			return fail(400, "unsupported grant type")
		}
	}
	for _, response := range req.ResponseTypes {
		if response != "code" {
			return fail(400, "unsupported response type")
		}
	}
	client := model.OAuthClient{ID: auth.Token(), Name: req.Name, RedirectURIs: req.RedirectURIs, Created: time.Now().UTC()}
	if err := s.DB.Update(func(st *model.State) error {
		prune(st)
		if len(st.OAuthClients) >= 10000 {
			return fail(429, "too many OAuth clients")
		}
		st.OAuthClients[client.ID] = client
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]any{"client_id": client.ID, "client_name": client.Name, "redirect_uris": client.RedirectURIs, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": connectorScope})
	return nil
}

func (s *Server) browserSession(r *http.Request) (string, model.Session) {
	cookie, err := r.Cookie("kioku_session")
	if err != nil {
		return "", model.Session{}
	}
	key := auth.Hash(cookie.Value)
	var session model.Session
	_ = s.DB.View(func(st model.State) error {
		value, ok := st.Sessions[key]
		if _, exists := st.Users[value.UserID]; ok && exists && value.AgentID == "" && value.Expires.After(time.Now()) {
			session = value
		}
		return nil
	})
	return key, session
}
func (s *Server) authorizationRequest(q url.Values) (model.OAuthClient, error) {
	var client model.OAuthClient
	err := s.DB.View(func(st model.State) error {
		var ok bool
		client, ok = st.OAuthClients[q.Get("client_id")]
		if !ok || !slices.Contains(client.RedirectURIs, q.Get("redirect_uri")) {
			return fail(400, "invalid client or redirect URI")
		}
		return nil
	})
	if err != nil {
		return client, err
	}
	challenge, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if err != nil || len(challenge) != 32 || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
		return client, fail(400, "authorization code with PKCE S256 required")
	}
	if scope := q.Get("scope"); scope != "" && scope != connectorScope {
		return client, fail(400, "invalid scope")
	}
	if resource := q.Get("resource"); resource != "" && resource != s.Config.BaseURL+"/mcp" {
		return client, fail(400, "invalid resource")
	}
	return client, nil
}

const connectorConsent = `{{define "body"}}<h1>Authorize Kioku connector</h1>
<p>Signed in as {{.Email}}</p>
{{if .Problem}}<p role="alert">{{.Problem}}</p>{{end}}
{{if .Projects}}{{if .Name}}<p>Connector name (provided by the app): <bdi>{{.Name}}</bdi></p>{{end}}
<p>The authorization code will be sent to <strong><code dir="ltr">{{.CallbackOrigin}}</code></strong>.
Only allow this destination if you trust it to read and search the wikis, add sources, and issue kioku CLI login codes for the projects you select.</p>
<form method="post" action="{{.Action}}"><input type="hidden" name="consent_user" value="{{.UserID}}">
<fieldset><legend>Select at least one project</legend>
{{range .Projects}}<div><label><input type="checkbox" name="project_id" value="{{.ID}}"> {{.Namespace}}</label></div>{{end}}
</fieldset><button type="submit">Allow</button></form>
{{else}}<p>You have no projects to share. Create or join a Kioku project, then connect again.</p>{{end}}{{end}}`

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request, _ string) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := r.ParseForm(); err != nil {
		return fail(400, "invalid authorization request")
	}
	client, err := s.authorizationRequest(r.Form)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return nil
	}
	// Recheck the stored registration as older clients predate the CSP host check.
	callbackOrigin := redirectOrigin(r.Form.Get("redirect_uri"))
	if callbackOrigin == "" {
		return fail(400, "invalid redirect URI")
	}
	key, session := s.browserSession(r)
	if session.UserID == "" {
		if r.Method == "POST" {
			return fail(401, "browser login required")
		}
		target := "/oauth/authorize?" + r.Form.Encode()
		http.Redirect(w, r, "/?return="+url.QueryEscape(target), http.StatusFound)
		return nil
	}
	if r.Method == "POST" && r.Header.Get("Origin") != s.Config.BaseURL {
		return fail(403, "consent requires same-origin POST")
	}
	// A client controls the authorization URL. Only checked boxes from the
	// user's POST body can authorize a project, never query parameters.
	projectIDs := r.PostForm["project_id"]
	// maxConsentProjects bounds the work done under the DB write lock.
	const maxConsentProjects = 200
	if len(projectIDs) > maxConsentProjects {
		return fail(400, "too many projects selected")
	}
	accountChanged := r.Method == "POST" && r.PostForm.Get("consent_user") != session.UserID
	if r.Method == "GET" || len(projectIDs) == 0 || accountChanged {
		r.Form.Del("project_id")
		r.Form.Del("consent_user")
		data := struct {
			UserID, Email, Name, Action, CallbackOrigin, Problem string
			Projects                                             []model.Project
		}{UserID: session.UserID, Name: client.Name, Action: "/oauth/authorize?" + r.Form.Encode(), CallbackOrigin: callbackOrigin}
		if err := s.DB.View(func(st model.State) error {
			data.Email = st.Users[session.UserID].Email
			for _, p := range st.Projects {
				if p.Members[session.UserID] != "" && !p.Deleting {
					data.Projects = append(data.Projects, p)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		slices.SortFunc(data.Projects, func(a, b model.Project) int { return strings.Compare(a.Namespace, b.Namespace) })
		// The consent form needs its Origin on POST; no-referrer makes browsers send null.
		w.Header().Set("Referrer-Policy", "same-origin")
		// Browsers also check the cross-origin redirect after the same-origin form POST.
		w.Header().Set("Content-Security-Policy", pageCSP+" "+callbackOrigin)
		if r.Method == "POST" {
			status := http.StatusBadRequest
			data.Problem = "No project was selected. Check a project to continue."
			if accountChanged {
				status = http.StatusConflict
				data.Problem = "The signed-in account changed. Review this account's projects before allowing access."
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
		}
		return render(w, "Authorize connector", data, connectorConsent)
	}
	raw := "kioku_auth." + auth.Token()
	if err := s.DB.Update(func(st *model.State) error {
		prune(st)
		current, ok := st.Sessions[key]
		if !ok || current.UserID != session.UserID || !current.Expires.After(time.Now()) {
			return fail(401, "browser session expired")
		}
		slices.Sort(projectIDs)
		projectIDs = slices.Compact(projectIDs)
		for _, id := range projectIDs {
			if _, err := member(*st, id, current.UserID); err != nil {
				return fail(400, "selected project is unavailable; reopen the consent page")
			}
		}
		if len(st.OAuthCodes) >= 10000 {
			return fail(429, "too many authorization attempts")
		}
		st.OAuthCodes[auth.Hash(raw)] = model.OAuthCode{ClientID: client.ID, UserID: current.UserID, ProjectIDs: projectIDs, RedirectURI: r.Form.Get("redirect_uri"), Challenge: r.Form.Get("code_challenge"), Expires: time.Now().Add(time.Minute)}
		return nil
	}); err != nil {
		return err
	}
	target, _ := url.Parse(r.Form.Get("redirect_uri"))
	q := target.Query()
	q.Set("code", raw)
	q.Set("state", r.Form.Get("state"))
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
	return nil
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request, _ string) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := r.ParseForm(); err != nil {
		jsonOut(w, 400, map[string]string{"error": "invalid_request"})
		return nil
	}
	access, refresh := "kioku_oauth."+auth.Token(), "kioku_oauth."+auth.Token()
	problem := "invalid_grant"
	err := s.DB.Update(func(st *model.State) error {
		prune(st)
		clientID := r.Form.Get("client_id")
		if _, ok := st.OAuthClients[clientID]; !ok {
			return nil
		}
		if resource := r.Form.Get("resource"); resource != "" && resource != s.Config.BaseURL+"/mcp" {
			problem = "invalid_target"
			return nil
		}
		if scope := r.Form.Get("scope"); scope != "" && scope != connectorScope {
			problem = "invalid_scope"
			return nil
		}
		var uid, refreshKey string
		var projectIDs []string
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			key := auth.Hash(r.Form.Get("code"))
			code, ok := st.OAuthCodes[key]
			delete(st.OAuthCodes, key)
			verifier := r.Form.Get("code_verifier")
			if !ok || !code.Expires.After(time.Now()) || code.ClientID != clientID || code.RedirectURI != r.Form.Get("redirect_uri") || len(verifier) < 43 || len(verifier) > 128 || strings.IndexFunc(verifier, func(c rune) bool {
				return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c))
			}) >= 0 || auth.Challenge(verifier) != code.Challenge {
				return nil
			}
			uid = code.UserID
			projectIDs = code.ProjectIDs
		case "refresh_token":
			key := auth.Hash(r.Form.Get("refresh_token"))
			grant, ok := st.OAuthTokens[key]
			if !ok || grant.Kind != "refresh" || grant.ClientID != clientID || !grant.Expires.After(time.Now()) {
				return nil
			}
			refreshKey = key
			uid = grant.UserID
			projectIDs = grant.ProjectIDs
		default:
			problem = "unsupported_grant_type"
			return nil
		}
		if _, ok := st.Users[uid]; !ok || len(projectIDs) == 0 {
			return nil
		}
		count := len(st.OAuthTokens)
		if refreshKey != "" {
			count--
		}
		if count > 19998 {
			problem = "temporarily_unavailable"
			return nil
		}
		if refreshKey != "" {
			delete(st.OAuthTokens, refreshKey)
		}
		st.OAuthTokens[auth.Hash(access)] = model.OAuthGrant{Kind: "access", ClientID: clientID, UserID: uid, ProjectIDs: projectIDs, Expires: time.Now().Add(time.Hour)}
		st.OAuthTokens[auth.Hash(refresh)] = model.OAuthGrant{Kind: "refresh", ClientID: clientID, UserID: uid, ProjectIDs: projectIDs, Expires: time.Now().Add(30 * 24 * time.Hour)}
		problem = ""
		return nil
	})
	if err != nil {
		return err
	}
	if problem != "" {
		jsonOut(w, 400, map[string]string{"error": problem})
		return nil
	}
	jsonOut(w, 200, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": refresh, "scope": connectorScope})
	return nil
}
