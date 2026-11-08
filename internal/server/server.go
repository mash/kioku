package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/publicapi"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

type Config struct {
	Prompts                    *prompts.Rendered
	TrustedProxy               *net.IPNet
	AnthropicWebhookSigningKey string
	AgentMaxSessions           int
	AgentJobTimeout            time.Duration
	AgentMaxSessionCents       int64
	GenerateEvery              time.Duration
	BaseURL                    string
	Providers                  map[string]auth.Provider
	MaxWikiBytes               int64
	MaxQueued                  int
	RatePerMinute              int
	MetricsToken               string
}

// Runners supplies configured runners.
type Runners interface {
	New(model.Job) (runner.Runner, error)
}

// SetRunners injects generation services before serving requests or starting workers.
func (s *Server) SetRunners(g Runners) { s.runners = g }

type Server struct {
	mcpTools     map[string]any
	runners      Runners
	Config       Config
	DB           *store.Store
	Blobs        blob.Store
	Cipher       *auth.Cipher
	wake         chan struct{}
	mux          *http.ServeMux
	projectLocks sync.Map
	prepareOnce  sync.Once
	prepareErr   error
	rateMu       sync.Mutex
	rates        map[string]rateWindow
	requests     atomic.Uint64
	failures     atomic.Uint64
}
type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string            { return e.message }
func fail(status int, message string) error { return apiError{status, message} }
func New(c Config, db *store.Store, b blob.Store, key *auth.Cipher) *Server {
	if c.AgentMaxSessions == 0 {
		c.AgentMaxSessions = 4
	}
	if c.AgentJobTimeout == 0 {
		c.AgentJobTimeout = time.Hour
	}
	if c.AgentMaxSessionCents == 0 {
		c.AgentMaxSessionCents = 1000
	}
	if c.MaxWikiBytes <= 0 {
		c.MaxWikiBytes = 64 << 20
	}
	if c.MaxQueued <= 0 {
		c.MaxQueued = 20
	}
	if c.RatePerMinute <= 0 {
		c.RatePerMinute = 600
	}
	s := &Server{Config: c, DB: db, Blobs: b, Cipher: key, wake: make(chan struct{}, 1), mux: http.NewServeMux()}
	if c.Prompts != nil {
		s.mcpTools = mcpTools(c.Prompts)
	}
	s.routes()
	return s
}

// Keep form-action last so a page can add a specific allowed form destination.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Prepare(); err != nil {
			writeError(w, err)
			return
		}
		start := time.Now()
		rec := &responseLog{ResponseWriter: w, status: 200}
		w = rec
		s.requests.Add(1)
		defer func() {
			if rec.status >= 400 {
				s.failures.Add(1)
			}
			slog.Info("http", "method", r.Method, "path", r.Pattern, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		if !s.allow(r) {
			w.Header().Set("Retry-After", "60")
			writeError(w, fail(429, "rate limit exceeded"))
			return
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", pageCSP)
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.Config.BaseURL {
				writeError(w, fail(403, "cross-origin request denied"))
				return
			}
			if r.Header.Get("Authorization") == "" {
				if _, err := r.Cookie("kioku_session"); err == nil && r.Header.Get("Origin") == "" {
					writeError(w, fail(403, "browser mutations require Origin"))
					return
				}
			}
		}
		if err := checkClientVersion(r); err != nil {
			writeError(w, err)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}
func (s *Server) routes() {
	s.mux.HandleFunc("POST /mcp", s.mcp)
	s.mux.HandleFunc("GET /mcp", s.mcp)
	s.mux.HandleFunc("POST /api/agent/login", s.wrap(s.agentLogin, false))
	s.mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.wrap(s.oauthResourceMetadata, false))
	s.mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.wrap(s.oauthResourceMetadata, false))
	s.mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.wrap(s.oauthServerMetadata, false))
	s.mux.HandleFunc("POST /oauth/register", s.wrap(s.oauthRegister, false))
	s.mux.HandleFunc("GET /oauth/authorize", s.wrap(s.oauthAuthorize, false))
	s.mux.HandleFunc("POST /oauth/authorize", s.wrap(s.oauthAuthorize, false))
	s.mux.HandleFunc("POST /oauth/token", s.wrap(s.oauthToken, false))

	s.mux.HandleFunc(agentInputRoute, s.wrap(s.agentInput, true))
	s.mux.HandleFunc(agentOutputRoute, s.wrap(s.agentOutput, true))
	s.mux.HandleFunc("GET /api/account", s.wrap(s.account, true))
	s.mux.HandleFunc("DELETE /api/account", s.wrap(s.deleteAccount, true))
	s.mux.HandleFunc("DELETE /api/account/sessions/{session}", s.wrap(s.revokeSession, true))
	s.mux.HandleFunc("POST /api/account/revoke-all", s.wrap(s.revokeAllSessions, true))
	s.mux.HandleFunc("DELETE /api/account/identities/{provider}", s.wrap(s.unlinkIdentity, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/owner", s.wrap(s.transferProject, true))
	s.mux.HandleFunc("POST /api/orgs/{org}/owner", s.wrap(s.transferOrganization, true))

	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/agents", s.wrap(s.agents, true))
	s.mux.HandleFunc("DELETE /api/projects/{org}/{slug}/agents/{agent}", s.wrap(s.revokeAgent, true))

	s.mux.HandleFunc("POST /webhooks/anthropic", s.anthropicWebhook)

	s.mux.HandleFunc("POST /api/orgs", s.wrap(s.createOrganization, true))
	s.mux.HandleFunc("GET /api/orgs", s.wrap(s.organizations, true))
	s.mux.HandleFunc("GET /{org}/{slug}", s.web(s.namespacePage, true))

	s.mux.HandleFunc("GET /metrics", s.metrics)
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/usage", s.wrap(s.usage, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/audit", s.wrap(s.audit, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/maintenance", s.wrap(s.cleanup, true))
	s.mux.HandleFunc("DELETE /api/projects/{org}/{slug}", s.wrap(s.deleteProject, true))

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.View(func(model.State) error { return nil }); err != nil {
			writeError(w, err)
			return
		}
		jsonOut(w, 200, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("POST /api/auth/loopback", s.wrap(s.loopbackStart, false))
	s.mux.HandleFunc("POST /api/auth/loopback/token", s.wrap(s.loopbackToken, false))
	s.mux.HandleFunc("GET /auth/{provider}/start", s.wrap(s.oauthStart, false))
	s.mux.HandleFunc("GET /auth/{provider}/callback", s.wrap(s.oauthCallback, false))
	s.mux.HandleFunc("POST /api/logout", s.wrap(s.logout, true))
	s.mux.HandleFunc("GET /api/me", s.wrap(s.me, true))
	s.mux.HandleFunc("GET /api/projects", s.wrap(s.projects, true))
	s.mux.HandleFunc("POST /api/projects", s.wrap(s.createProject, true))
	s.mux.HandleFunc("POST /api/join", s.wrap(s.join, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/members", s.wrap(s.members, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/invites", s.wrap(s.invite, true))
	s.mux.HandleFunc("DELETE /api/projects/{org}/{slug}/members/{member}", s.wrap(s.removeMember, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/put", s.wrap(s.put, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/generate", s.wrap(s.generate, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/jobs/{job}", s.wrap(s.job, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/jobs/{job}/retry", s.wrap(s.retry, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/snapshot", s.wrap(s.snapshot, true))
	s.mux.HandleFunc("GET /", s.web(s.home, false))
	s.mux.HandleFunc("GET /invite/{invite}", s.web(s.invitePage, false))
}

type authContextKey struct{}
type requestIdentity struct {
	key       string
	principal model.Session
}

type handler func(http.ResponseWriter, *http.Request, string) error

func (s *Server) wrap(fn handler, required bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, principal := s.session(r)
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, requestIdentity{key: key, principal: principal}))
		uid := principal.UserID
		if principal.Role == "job" && r.Pattern != agentInputRoute && r.Pattern != agentOutputRoute {
			writeError(w, fail(403, "job credential permits only job input and output"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/projects/") && r.PathValue("slug") != "" && r.PathValue("org") != "" {
			if uid == "" {
				if err := s.authorizeAgent(r, principal); err != nil {
					writeError(w, err)
					return
				}
				writeError(w, fail(401, "login required"))
				return
			}
			err := s.DB.View(func(st model.State) error {
				p, err := namespaceProject(st, r.PathValue("org"), r.PathValue("slug"), uid)
				if err == nil {
					r.SetPathValue("project", p.ID)
				}
				return err
			})
			if err != nil {
				writeError(w, err)
				return
			}
		}
		if err := s.authorizeAgent(r, principal); err != nil {
			writeError(w, err)
			return
		}
		if required && uid == "" {
			writeError(w, fail(401, "login required"))
			return
		}
		if err := fn(w, r, uid); err != nil {
			writeError(w, err)
		} else if pid := r.PathValue("project"); pid != "" && r.Method != "GET" && r.Method != "HEAD" {
			detail := r.Pattern
			if principal.AgentID != "" {
				detail += " agent=" + principal.AgentID
			}
			s.recordAudit(pid, uid, r.Method, detail)
		}
	}
}
func (s *Server) recordAudit(pid, uid, action, detail string) {
	if err := s.DB.Update(func(st *model.State) error {
		if _, ok := st.Projects[pid]; ok {
			if st.Audit == nil {
				st.Audit = map[string]model.Audit{}
			}
			id := auth.Token()[:20]
			st.Audit[id] = model.Audit{ID: id, ProjectID: pid, UserID: uid, Action: action, Detail: detail, Created: time.Now().UTC()}
		}
		return nil
	}); err != nil {
		slog.Error("audit write failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	var e apiError
	if errors.As(err, &e) {
		jsonOut(w, e.status, map[string]string{"error": e.message})
		return
	}
	slog.Error("request failed", "error", err)
	jsonOut(w, 500, map[string]string{"error": "internal server error"})
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(publicapi.Value(v))
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fail(400, "invalid JSON or request exceeds 16 MiB")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fail(400, "unexpected trailing JSON")
	}
	return nil
}
func (s *Server) session(r *http.Request) (string, model.Session) {
	if identity, ok := r.Context().Value(authContextKey{}).(requestIdentity); ok {
		return identity.key, identity.principal
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		if c, err := r.Cookie("kioku_session"); err == nil {
			token = c.Value
		}
	}
	key := auth.Hash(token)
	var session model.Session
	_ = s.DB.View(func(st model.State) error {
		v, ok := st.Sessions[key]
		if _, exists := st.Users[v.UserID]; ok && exists && v.Expires.After(time.Now()) {
			session = v
		} else if r.Header.Get("Authorization") != "" {
			if scoped, valid := agentFromBearer(st, token); valid {
				session = scoped
			}
		}
		return nil
	})
	return key, session
}
func (s *Server) userID(r *http.Request) string { _, session := s.session(r); return session.UserID }

func member(st model.State, pid, uid string) (model.Project, error) {
	p, ok := st.Projects[pid]
	if !ok || p.Members[uid] == "" {
		return p, fail(404, "project not found")
	}
	if p.Deleting {
		return p, fail(409, "project deletion is in progress")
	}
	return p, nil
}
func owner(st model.State, pid, uid string) (model.Project, error) {
	p, err := member(st, pid, uid)
	if err == nil && p.Members[uid] != "owner" {
		err = fail(403, "owner permission required")
	}
	return p, err
}
func (s *Server) check(pid, uid string) error {
	return s.DB.View(func(st model.State) error { _, err := member(st, pid, uid); return err })
}
func (s *Server) me(w http.ResponseWriter, r *http.Request, uid string) error {
	if _, principal := s.session(r); principal.AgentID != "" {
		return s.DB.View(func(st model.State) error {
			jsonOut(w, 200, map[string]any{"agent_id": principal.AgentID, "namespace": st.Projects[principal.ProjectID].Namespace, "role": principal.Role, "expires": principal.Expires})
			return nil
		})
	}
	return s.DB.View(func(st model.State) error {
		u := st.Users[uid]
		jsonOut(w, 200, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email})
		return nil
	})
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request, uid string) error {
	_, principal := s.session(r)
	return s.DB.View(func(st model.State) error {
		out := []model.ProjectEntry{}
		for _, p := range st.Projects {
			if p.Members[uid] != "" && (principal.AgentID == "" || p.ID == principal.ProjectID) {
				entry := model.ProjectEntry{Project: p, Inbox: []model.InboxEntry{}}
				for _, item := range st.Inbox {
					if item.ProjectID == p.ID {
						entry.Inbox = append(entry.Inbox, model.InboxEntry{SourceURL: item.SourceURL, Remove: item.Remove})
					}
				}
				sort.Slice(entry.Inbox, func(i, j int) bool {
					if entry.Inbox[i].SourceURL == entry.Inbox[j].SourceURL {
						return !entry.Inbox[i].Remove && entry.Inbox[j].Remove
					}
					return entry.Inbox[i].SourceURL < entry.Inbox[j].SourceURL
				})
				entry.InboxCount = len(entry.Inbox)
				out = append(out, entry)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
		jsonOut(w, 200, out)
		return nil
	})
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
		Slug         string `json:"slug"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		return fail(400, "name must contain 1–100 bytes")
	}
	p := model.Project{ID: auth.Token()[:20], Name: req.Name, Members: map[string]string{uid: "owner"}, Created: time.Now().UTC()}
	if strings.Contains(req.Name, "/") {
		parts := strings.Split(req.Name, "/")
		if len(parts) != 2 || req.Organization != "" {
			return fail(400, "use organization/project")
		}
		req.Organization = parts[0]
		req.Slug = parts[1]
		p.Name = parts[1]
	}
	if req.Slug == "" {
		req.Slug = projectSlug(p.Name)
	}
	if !validSlug(req.Slug) {
		return fail(400, "invalid project slug")
	}
	err := s.DB.Update(func(st *model.State) error {
		initializeOrganizations(st)
		var org model.Organization
		if req.Organization == "" {
			org = personalOrg(st, uid)
			if org.Members[uid] != "owner" {
				return fail(409, "personal namespace ownership changed; select a named organization")
			}
		} else {
			var err error
			org, err = orgOwner(*st, req.Organization, uid)
			if err != nil {
				return err
			}
		}
		p.OrgID = org.ID
		p.Slug = req.Slug
		p.Namespace = org.Slug + "/" + p.Slug
		for _, existing := range st.Projects {
			if existing.Namespace == p.Namespace {
				return fail(409, "project name is already in use in this organization")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if manager, ok := s.Blobs.(interface {
		EnsureProject(context.Context, string) error
	}); ok {
		if err := manager.EnsureProject(r.Context(), p.ID); err != nil {
			return err
		}
	}
	if err := s.DB.Update(func(st *model.State) error {
		for _, existing := range st.Projects {
			if existing.Namespace == p.Namespace {
				return fail(409, "project namespace is already in use")
			}
		}
		p.StorageRoot = "projects/" + p.ID + "/"
		if buckets, ok := s.Blobs.(interface{ Bucket(string) string }); ok {
			p.StorageBucket = buckets.Bucket(p.ID)
		}
		st.Projects[p.ID] = p
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 201, p)
	return nil
}
func (s *Server) members(w http.ResponseWriter, r *http.Request, uid string) error {
	return s.DB.View(func(st model.State) error {
		p, err := member(st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		out := []map[string]any{}
		for id, role := range p.Members {
			u := st.Users[id]
			out = append(out, map[string]any{"id": id, "name": u.Name, "email": u.Email, "role": role, "status": "active"})
		}
		if p.Members[uid] == "owner" {
			for _, inv := range st.Invites {
				if inv.ProjectID == p.ID && inv.UsedBy == "" && inv.Expires.After(time.Now()) {
					out = append(out, map[string]any{"id": inv.ID, "email": inv.Email, "role": "member", "status": "invited", "expires": inv.Expires})
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["id"]) < fmt.Sprint(out[j]["id"]) })
		jsonOut(w, 200, out)
		return nil
	})
}
func (s *Server) invite(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Email string `json:"email"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email != "" && (!strings.Contains(req.Email, "@") || len(req.Email) > 254) {
		return fail(400, "invalid email")
	}
	token := auth.Token()
	inv := model.Invite{ID: auth.Token()[:20], ProjectID: r.PathValue("project"), Email: req.Email, CreatedBy: uid, Expires: time.Now().Add(7 * 24 * time.Hour), TokenHash: auth.Hash(token)}
	if err := s.DB.Update(func(st *model.State) error {
		if _, err := owner(*st, inv.ProjectID, uid); err != nil {
			return err
		}
		st.Invites[inv.ID] = inv
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]any{"url": s.Config.BaseURL + "/invite/" + token, "expires": inv.Expires, "email": inv.Email})
	return nil
}
func (s *Server) join(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if u, err := url.Parse(req.Token); err == nil && u.Host != "" {
		req.Token = strings.TrimPrefix(u.Path, "/invite/")
	}
	// Resolve the invite before contacting a provider; email-bound invitations
	// require current provider proof, never a cached profile email.
	var boundEmail string
	if err := s.DB.View(func(st model.State) error {
		for _, inv := range st.Invites {
			if inv.TokenHash == auth.Hash(req.Token) {
				if inv.UsedBy != "" || !inv.Expires.After(time.Now()) {
					return fail(410, "invite expired or already used")
				}
				boundEmail = inv.Email
				return nil
			}
		}
		return fail(404, "invite not found")
	}); err != nil {
		return err
	}
	verifiedEmail := ""
	if boundEmail != "" {
		var err error
		verifiedEmail, err = s.currentEmail(r, uid)
		if err != nil {
			return err
		}
	}
	var result model.Project
	err := s.DB.Update(func(st *model.State) error {
		for id, inv := range st.Invites {
			if inv.TokenHash != auth.Hash(req.Token) {
				continue
			}
			if inv.UsedBy != "" || !inv.Expires.After(time.Now()) {
				return fail(410, "invite expired or already used")
			}
			if inv.Email != "" && !strings.EqualFold(inv.Email, verifiedEmail) {
				return fail(403, "invite is restricted to a different verified email")
			}
			p, ok := st.Projects[inv.ProjectID]
			if !ok {
				return fail(404, "project not found")
			}
			if p.Deleting {
				return fail(409, "project deletion is in progress")
			}
			if p.Members[uid] == "" {
				p.Members[uid] = "member"
			}
			inv.UsedBy = uid
			st.Invites[id] = inv
			st.Projects[p.ID] = p
			result = p
			return nil
		}
		return fail(404, "invite not found")
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, result)
	return nil
}
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, uid string) error {
	err := s.DB.Update(func(st *model.State) error {
		p, err := owner(*st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		target := r.PathValue("member")
		for id := range p.Members {
			if strings.EqualFold(st.Users[id].Email, target) {
				target = id
				break
			}
		}
		if p.Members[target] == "owner" {
			return fail(400, "cannot remove the project owner")
		}
		if p.Members[target] == "" {
			return fail(404, "member not found")
		}
		delete(p.Members, target)
		revokeAgents(st, target, p.ID)
		st.Projects[p.ID] = p
		return nil
	})
	if err == nil {
		jsonOut(w, 200, map[string]bool{"removed": true})
	}
	return err
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, uid string) error {
	if _, principal := s.session(r); principal.AgentID != "" {
		if err := s.DB.Update(func(st *model.State) error {
			t := st.AgentTokens[principal.AgentID]
			t.RevokedAt = time.Now().UTC()
			st.AgentTokens[t.ID] = t
			return nil
		}); err != nil {
			return err
		}
		jsonOut(w, 200, map[string]bool{"logged_out": true})
		return nil
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		if c, err := r.Cookie("kioku_session"); err == nil {
			token = c.Value
		}
	}
	if err := s.DB.Update(func(st *model.State) error { delete(st.Sessions, auth.Hash(token)); return nil }); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "kioku_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
	jsonOut(w, 200, map[string]bool{"logged_out": true})
	return nil
}
