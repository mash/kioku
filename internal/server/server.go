package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
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
	GenerateDebounce           time.Duration
	BaseURL                    string
	Providers                  map[string]auth.Provider
	SignupAllowlist            SignupAllowlist
	MaxWikiBytes               int64
	MaxQueued                  int
	RatePerMinute              int
	MetricsToken               string
}

// SetRunner injects the runner selected at startup before serving requests or
// starting workers. A nil runner fails every generation job.
func (s *Server) SetRunner(r runner.Runner) { s.runner = r }

type Server struct {
	mcpTools map[string]any
	runner   runner.Runner
	Config   Config
	Store    catalog
	Blobs    blob.Store
	Cipher   *auth.Cipher
	wake     chan struct{}
	mux      *http.ServeMux
	storage  *storageOperations
	rateMu   sync.Mutex
	rates    map[string]rateWindow
	requests atomic.Uint64
	failures atomic.Uint64
}
type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string            { return e.message }
func fail(status int, message string) error { return apiError{status, message} }

// MaxWikiBytes is the default limit of published Wiki bytes per project.
const MaxWikiBytes = 64 << 20

func New(c Config, stateStore catalog, b blob.Store, key *auth.Cipher) *Server {
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
		c.MaxWikiBytes = MaxWikiBytes
	}
	if c.MaxQueued <= 0 {
		c.MaxQueued = 20
	}
	if c.RatePerMinute <= 0 {
		c.RatePerMinute = 600
	}
	s := &Server{Config: c, Store: stateStore, Blobs: b, Cipher: key, wake: make(chan struct{}, 1), mux: http.NewServeMux()}
	if c.Prompts != nil {
		s.mcpTools = mcpTools(c.Prompts)
	}
	s.storage = newStorageOperations(stateStore, b, &s.Config)
	s.routes()
	return s
}

// Keep form-action last so a page can add a specific allowed form destination.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	s.mux.HandleFunc("POST /api/orgs/{org}/owners", s.wrap(s.addOrganizationOwner, true))

	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/agents", s.wrap(s.agents, true))
	s.mux.HandleFunc("DELETE /api/projects/{org}/{slug}/agents/{agent}", s.wrap(s.revokeAgent, true))

	s.mux.HandleFunc("POST /webhooks/anthropic", s.anthropicWebhook)

	s.mux.HandleFunc("POST /api/orgs", s.wrap(s.createOrganization, true))
	s.mux.HandleFunc("GET /api/orgs", s.wrap(s.organizations, true))
	s.mux.HandleFunc("GET /{org}/{slug}", s.web(s.namespacePage, true))
	s.mux.HandleFunc("GET /{org}/{slug}/views/{view}", s.web(s.viewPage, true))

	s.mux.HandleFunc("GET /metrics", s.metrics)
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/usage", s.wrap(s.usage, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/audit", s.wrap(s.audit, true))
	s.mux.HandleFunc("DELETE /api/projects/{org}/{slug}", s.wrap(s.deleteProject, true))

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Read(r.Context(), func(q *store.Queries) error { return q.Healthy() }); err != nil {
			writeError(w, err)
			return
		}
		jsonOut(w, 200, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("POST /api/auth/loopback", s.wrap(s.loopbackStart, false))
	s.mux.HandleFunc("POST /api/auth/loopback/token", s.wrap(s.loopbackToken, false))
	s.mux.HandleFunc("GET /auth/{provider}/start", s.wrap(s.oauthStart, false))
	s.mux.HandleFunc("GET /auth/{provider}/callback", s.wrap(s.oauthCallback, false))
	s.mux.HandleFunc("POST /auth/signup", s.wrap(s.signup, false))
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
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/views", s.wrap(s.listViews, true))
	s.mux.HandleFunc("POST /api/projects/{org}/{slug}/views", s.wrap(s.createView, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/views/{view}", s.wrap(s.getView, true))
	s.mux.HandleFunc("PATCH /api/projects/{org}/{slug}/views/{view}", s.wrap(s.updateView, true))
	s.mux.HandleFunc("DELETE /api/projects/{org}/{slug}/views/{view}", s.wrap(s.deleteView, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/jobs/{job}", s.wrap(s.job, true))
	s.mux.HandleFunc("GET /api/projects/{org}/{slug}/snapshot", s.wrap(s.snapshot, true))
	s.mux.HandleFunc("GET /", s.web(s.home, false))
	s.mux.HandleFunc("GET /invite/{invite}", s.web(s.invitePage, false))
}

type authContextKey struct{}
type requestIdentity struct {
	key       string
	principal authenticatedSession
}

type handler func(http.ResponseWriter, *http.Request, string) error

func (s *Server) wrap(fn handler, required bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, principal, err := s.session(r)
		if err != nil {
			writeError(w, err)
			return
		}
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
			err := s.Store.Read(r.Context(), func(q *store.Queries) error {
				if principal.Role == "job" {
					org, e := q.OrganizationBySlug(r.PathValue("org"))
					if errors.Is(e, sql.ErrNoRows) {
						return fail(403, "invalid job credential")
					}
					if e != nil {
						return e
					}
					p, e := q.ProjectBySlug(org.ID, r.PathValue("slug"))
					if errors.Is(e, sql.ErrNoRows) {
						return fail(403, "invalid job credential")
					}
					if e != nil {
						return e
					}
					r.SetPathValue("project", p.ID)
					_, e = checkSQLJobCredential(q, s.Cipher, requestBearer(r), p.ID, r.PathValue("job"), time.Now())
					return e
				}
				p, err := namespaceProject(q, r.PathValue("org"), r.PathValue("slug"), uid, r.Pattern == "DELETE /api/projects/{org}/{slug}")
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
		r = r.WithContext(context.WithValue(r.Context(), mutationRequestKey{}, r))
		if err := fn(w, r, uid); err != nil {
			writeError(w, err)

		}
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
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	return readBody(w, r).decode(v)
}

// requestBody holds a JSON request body that a handler reads before its
// authorization check and decodes after it, so that an unauthorized request
// never learns which input is valid.
type requestBody struct {
	raw []byte
	err error
}

func readBody(w http.ResponseWriter, r *http.Request) requestBody {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, model.MaxJSONRequestBytes))
	return requestBody{raw, err}
}
func (b requestBody) decode(v any) error {
	if b.err != nil {
		return fail(400, "invalid JSON or request exceeds 16 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b.raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fail(400, "invalid JSON or request exceeds 16 MiB")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fail(400, "unexpected trailing JSON")
	}
	return nil
}
