package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/ory/fosite"
)

const connectorScope = "cli_login"

func authorizeReturn(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawPath != "" || strings.Contains(value, "#") || strings.ContainsAny(u.Path, "\\\r\n\t") {
		return "", fail(400, "invalid login return path")
	}
	allowed := u.Path == "/" || u.Path == "/oauth/authorize"
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if strings.HasPrefix(u.Path, "/") && len(parts) == 2 {
		// Personal namespaces (u-...) are valid too. Reserve service route prefixes.
		org := parts[0]
		allowed = allowed || (validSlug(parts[1]) && validSlug(org) && (!reservedNamespace(org) || strings.HasPrefix(org, "u-")))
	}
	if !allowed {
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
	client := store.OAuthClientRow{ClientID: auth.Token(), Data: store.OAuthClientData{ClientName: req.Name, RedirectURIs: req.RedirectURIs, Created: time.Now().UnixMilli()}}
	if err := s.Store.Write(r.Context(), func(q *store.Queries) error {
		if err := q.PruneSessions(time.Now().UnixMilli()); err != nil {
			return err
		}
		return q.InsertOAuthClient(client)
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]any{"client_id": client.ClientID, "client_name": client.Data.ClientName, "redirect_uris": client.Data.RedirectURIs, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": connectorScope})
	return nil
}

func (s *Server) browserSession(r *http.Request) (string, authenticatedSession, error) {
	copy := r.Clone(r.Context())
	copy.Header = copy.Header.Clone()
	copy.Header.Del("Authorization")
	copy = copy.WithContext(context.WithValue(copy.Context(), authContextKey{}, struct{}{}))
	key, session, err := s.session(copy)
	if session.AgentID != "" {
		session = authenticatedSession{}
	}
	return key, session, err
}

// Resource indicators are an MCP adapter around Fosite's audience handling.
// Kioku exposes one protected resource; never silently accept another audience.
func (s *Server) oauthResource(form url.Values) error {
	target := s.Config.BaseURL + "/mcp"
	for _, name := range []string{"resource", "audience"} {
		if values := form[name]; len(values) > 1 || len(values) == 1 && values[0] != target {
			return &fosite.RFC6749Error{ErrorField: "invalid_target", DescriptionField: "The resource must be this server's MCP endpoint.", CodeField: http.StatusBadRequest}
		}
	}
	form.Set("audience", target)
	return nil
}

func (s *Server) authorizationRequest(r *http.Request) (store.OAuthClientRow, error) {
	var client store.OAuthClientRow
	if err := s.oauthResource(r.Form); err != nil {
		return client, err
	}
	if r.Form.Get("scope") == "" {
		r.Form.Set("scope", connectorScope)
	}
	// Reject malformed PKCE inputs before showing consent. The actual verifier
	// validation and binding to the issued code belong to Fosite.
	if !validChallenge(r.Form.Get("code_challenge")) || r.Form.Get("code_challenge_method") != "S256" {
		return client, fosite.ErrInvalidRequest
	}
	err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		provider, _ := newSQLOAuth(q, s.Cipher, s.Config.BaseURL+"/mcp", time.Now())
		request, err := provider.NewAuthorizeRequest(r.Context(), r)
		if err != nil {
			return err
		}
		client, err = q.OAuthClient(request.GetClient().GetID())
		return err
	})
	return client, err
}

const connectorConsent = `{{define "body"}}<h1>Authorize Kioku connector</h1>
<p>Signed in as {{.Email}}</p>
{{if .Problem}}<p role="alert">{{.Problem}}</p>{{end}}
{{if .Projects}}{{if .Name}}<p>Connector name (provided by the app): <bdi>{{.Name}}</bdi></p>{{end}}
<p>The authorization code will be sent to <strong><code dir="ltr">{{.CallbackOrigin}}</code></strong>.
Only allow this destination if you trust it to read and search the wikis, add sources, and issue kioku CLI login codes for the projects you select.</p>
<form method="post" action="{{.Action}}"><input type="hidden" name="consent_user" value="{{.UserID}}">
<fieldset><legend>Select at least one project</legend>
{{range .Projects}}<div><label><input type="checkbox" name="namespace" value="{{.Namespace}}"> {{.Namespace}}</label></div>{{end}}
</fieldset><button type="submit">Allow</button></form>
{{else}}<p>You have no projects to share. Create or join a Kioku project, then connect again.</p>{{end}}{{end}}`

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request, _ string) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := r.ParseForm(); err != nil {
		return fail(400, "invalid authorization request")
	}
	client, err := s.authorizationRequest(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return nil
	}
	// Recheck the stored registration as older clients predate the CSP host check.
	callbackOrigin := redirectOrigin(r.Form.Get("redirect_uri"))
	if callbackOrigin == "" {
		return fail(400, "invalid redirect URI")
	}
	key, session, err := s.browserSession(r)
	if err != nil {
		return err
	}
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
	namespaces := append([]string(nil), r.PostForm["namespace"]...)
	// maxConsentProjects bounds the work done under the Store write lock.
	const maxConsentProjects = 200
	if len(namespaces) > maxConsentProjects {
		return fail(400, "too many projects selected")
	}
	accountChanged := r.Method == "POST" && r.PostForm.Get("consent_user") != session.UserID
	if r.Method == "GET" || len(namespaces) == 0 || accountChanged {
		r.Form.Del("namespace")
		r.Form.Del("consent_user")
		data := struct {
			UserID, Email, Name, Action, CallbackOrigin, Problem string
			Projects                                             []model.Project
		}{UserID: session.UserID, Name: client.Data.ClientName, Action: "/oauth/authorize?" + r.Form.Encode(), CallbackOrigin: callbackOrigin}
		if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
			u, err := q.User(session.UserID)
			if err != nil {
				return err
			}
			data.Email = u.Email
			rows, err := q.ProjectsForUser(session.UserID)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Deleting {
					continue
				}
				p, err := projectProjection(q, row)
				if err != nil {
					return err
				}
				data.Projects = append(data.Projects, p)
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
		return render(w, "Authorize connector", data, connectorConsent, headerFor(r, session.UserID))
	}
	var provider fosite.OAuth2Provider
	var request fosite.AuthorizeRequester
	var response fosite.AuthorizeResponder
	if err := s.Store.Write(r.Context(), func(q *store.Queries) error {
		if err := q.PruneSessions(time.Now().UnixMilli()); err != nil {
			return err
		}
		current, err := q.Session(key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil || current.Kind != "web" || current.Status != "active" || current.UserID != session.UserID || current.Expires <= time.Now().UnixMilli() {
			return fail(401, "browser session expired")
		}
		slices.Sort(namespaces)
		namespaces = slices.Compact(namespaces)
		projectIDs := make([]string, len(namespaces))
		for i, namespace := range namespaces {
			parts := strings.Split(namespace, "/")
			if len(parts) != 2 {
				return fail(400, "selected project is unavailable; reopen the consent page")
			}
			project, err := namespaceProject(q, parts[0], parts[1], current.UserID)
			if err != nil {
				return fail(400, "selected project is unavailable; reopen the consent page")
			}
			projectIDs[i] = project.ID
		}
		provider, _ = newSQLOAuth(q, s.Cipher, s.Config.BaseURL+"/mcp", time.Now())
		request, err = provider.NewAuthorizeRequest(r.Context(), r)
		if err != nil {
			return err
		}
		request.GrantScope(connectorScope)
		request.GrantAudience(s.Config.BaseURL + "/mcp")
		response, err = provider.NewAuthorizeResponse(r.Context(), request, &connectorSession{
			DefaultSession: fosite.DefaultSession{Subject: current.UserID}, ProjectIDs: projectIDs,
		})
		if err != nil {
			return err
		}

		return nil
	}); err != nil {
		return err
	}
	provider.WriteAuthorizeResponse(r.Context(), w, request, response)
	return nil
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request, _ string) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := r.ParseForm(); err != nil {
		jsonOut(w, 400, map[string]string{"error": "invalid_request"})
		return nil
	}
	var provider fosite.OAuth2Provider
	var request fosite.AccessRequester
	var response fosite.AccessResponder
	var problem error
	err := s.Store.Write(r.Context(), func(q *store.Queries) error {
		if err := q.PruneSessions(time.Now().UnixMilli()); err != nil {
			return err
		}
		var adapter *oauthSQL
		provider, adapter = newSQLOAuth(q, s.Cipher, s.Config.BaseURL+"/mcp", time.Now())
		problem = s.oauthResource(r.PostForm)
		if problem == nil {
			request, problem = provider.NewAccessRequest(r.Context(), r, &connectorSession{})
		}
		if problem == nil {
			response, problem = provider.NewAccessResponse(r.Context(), request)
		}
		if adapter.failure != nil {
			return adapter.failure
		}
		if problem != nil && adapter.replay {
			return store.RejectAfterCommit(problem)
		}
		return problem
	})
	if err != nil && (problem == nil || !errors.Is(err, problem)) {
		return s.resolveExchange(r.Context(), err, 400)
	}
	if errors.Is(problem, store.ErrSessionCapacity) {
		problem = fosite.ErrTemporarilyUnavailable
	}
	if problem != nil {
		provider.WriteAccessError(r.Context(), w, request, problem)
		return nil
	}
	provider.WriteAccessResponse(r.Context(), w, request, response)
	return nil
}

func validChallenge(value string) bool {
	challenge, err := base64.RawURLEncoding.DecodeString(value)
	return len(value) == 43 && err == nil && len(challenge) == 32
}

func unreserved(value string) bool {
	return strings.IndexFunc(value, func(c rune) bool {
		return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c))
	}) < 0
}

func validVerifier(value string) bool {
	return len(value) >= 43 && len(value) <= 128 && unreserved(value)
}
