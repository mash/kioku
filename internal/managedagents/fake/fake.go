// Package fake provides an HTTP Managed Agents service for local tests only.
package fake

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Request struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         map[string]any
}
type Session struct {
	ID, Status, CostAmount, Currency string
	Metadata                         map[string]string
	VaultIDs                         []string
	CreatedAt                        time.Time
	Archived, Interrupted            bool
	Events                           []map[string]any
}
type Server struct {
	HTTP          *httptest.Server
	mu            sync.Mutex
	requests      []Request
	sessions      map[string]Session
	credentials   map[string]string
	vaults        map[string]bool
	vaultMetadata map[string]map[string]any
	errors        map[string]int
	pageSize      int
	onSession     func(string)
}

func New() *Server {
	f := &Server{sessions: map[string]Session{}, credentials: map[string]string{}, vaults: map[string]bool{}, vaultMetadata: map[string]map[string]any{}, errors: map[string]int{}, pageSize: 100}
	f.HTTP = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}
func (f *Server) Close()      { f.HTTP.Close() }
func (f *Server) URL() string { return f.HTTP.URL }
func (f *Server) SetError(method, path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors[method+" "+path] = status
}
func (f *Server) SetPageSize(size int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size > 0 {
		f.pageSize = size
	}
}
func (f *Server) OnSession(fn func(string)) { f.mu.Lock(); defer f.mu.Unlock(); f.onSession = fn }
func (f *Server) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}
func (f *Server) Credential(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credentials[id]
}
func (f *Server) VaultArchived(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.vaults[id] }
func (f *Server) Session(id string) Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneSession(f.sessions[id])
}
func cloneSession(s Session) Session {
	s.Metadata = maps.Clone(s.Metadata)
	s.VaultIDs = append([]string(nil), s.VaultIDs...)
	s.Events = append([]map[string]any(nil), s.Events...)
	return s
}
func (f *Server) SetSession(s Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = cloneSession(s)
}
func (f *Server) SetStatus(id, status, reason string, cents int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	s.Status = status
	s.CostAmount = strconv.FormatInt(cents, 10)
	s.Currency = "USD"
	if status == "idle" {
		s.Events = append(s.Events, map[string]any{"id": fmt.Sprintf("event_%06d", len(s.Events)+1), "type": "session.status_idle", "processed_at": time.Now().UTC().Format(time.RFC3339Nano), "stop_reason": map[string]string{"type": reason}})
	}
	f.sessions[id] = s
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func wireSession(s Session) map[string]any {
	v := map[string]any{"id": s.ID, "type": "session", "status": s.Status, "metadata": s.Metadata, "vault_ids": s.VaultIDs, "created_at": s.CreatedAt.Format(time.RFC3339Nano), "usage": map[string]any{"list_cost": map[string]string{"amount": s.CostAmount, "currency": s.Currency}}}
	if s.Archived {
		v["archived_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return v
}
func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, Request{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
	if code := f.errors[r.Method+" "+r.URL.Path]; code != 0 {
		f.mu.Unlock()
		jsonResponse(w, code, map[string]any{"error": map[string]string{"type": "api_error", "message": "fake provider error must not leak secrets"}})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		f.mu.Unlock()
		jsonResponse(w, 404, map[string]string{"error": "not found"})
		return
	}
	if parts[1] == "vaults" {
		var response any = map[string]string{"id": "credential"}
		status := 200
		switch {
		case len(parts) == 2 && r.Method == "POST":
			id := fmt.Sprintf("vault_%d", len(f.vaults)+1)
			f.vaults[id] = false
			f.vaultMetadata[id], _ = body["metadata"].(map[string]any)
			response = map[string]string{"id": id}
		case len(parts) == 2 && r.Method == "GET":
			var ids []string
			for id, archived := range f.vaults {
				if !archived {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			data := make([]any, 0, len(ids))
			for _, id := range ids {
				data = append(data, map[string]any{"id": id, "metadata": f.vaultMetadata[id]})
			}
			response = f.page(data, r.URL.Query().Get("page"))
		case len(parts) == 4 && parts[3] == "credentials" && r.Method == "POST":
			auth, _ := body["auth"].(map[string]any)
			secret, _ := auth["secret_value"].(string)
			f.credentials[parts[2]] = secret
		case len(parts) == 4 && parts[3] == "archive" && r.Method == "POST":
			f.vaults[parts[2]] = true
			response = map[string]string{"id": parts[2]}
		default:
			status = 404
		}
		f.mu.Unlock()
		jsonResponse(w, status, response)
		return
	}
	if parts[1] != "sessions" {
		f.mu.Unlock()
		jsonResponse(w, 404, nil)
		return
	}
	if len(parts) == 2 && r.Method == "POST" {
		id := fmt.Sprintf("session_%d", len(f.sessions)+1)
		metadata := map[string]string{}
		if m, ok := body["metadata"].(map[string]any); ok {
			for k, v := range m {
				metadata[k], _ = v.(string)
			}
		}
		var vaults []string
		if v, ok := body["vault_ids"].([]any); ok {
			for _, id := range v {
				if s, ok := id.(string); ok {
					vaults = append(vaults, s)
				}
			}
		}
		s := Session{ID: id, Status: "running", CostAmount: "0", Currency: "USD", Metadata: metadata, VaultIDs: vaults, CreatedAt: time.Now().UTC()}
		f.sessions[id] = s
		hook := f.onSession
		f.mu.Unlock()
		if hook != nil {
			hook(id)
		}
		jsonResponse(w, 200, wireSession(s))
		return
	}
	if len(parts) == 2 && r.Method == "GET" {
		ids := make([]string, 0, len(f.sessions))
		for id, s := range f.sessions {
			if !s.Archived {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		data := make([]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, wireSession(f.sessions[id]))
		}
		page := f.page(data, r.URL.Query().Get("page"))
		f.mu.Unlock()
		jsonResponse(w, 200, page)
		return
	}
	if len(parts) < 3 {
		f.mu.Unlock()
		jsonResponse(w, 404, nil)
		return
	}
	s, ok := f.sessions[parts[2]]
	if !ok {
		f.mu.Unlock()
		jsonResponse(w, 404, nil)
		return
	}
	var result any
	status := 200
	switch {
	case len(parts) == 3 && r.Method == "GET":
		result = wireSession(s)
	case len(parts) == 4 && parts[3] == "archive" && r.Method == "POST":
		s.Archived = true
		f.sessions[s.ID] = s
		result = wireSession(s)
	case len(parts) == 4 && parts[3] == "events" && r.Method == "GET":
		data := make([]any, 0, len(s.Events))
		for _, e := range s.Events {
			data = append(data, e)
		}
		result = f.page(data, r.URL.Query().Get("page"))
	case len(parts) == 4 && parts[3] == "events" && r.Method == "POST":
		s.Interrupted = true
		s.Status = "terminated"
		f.sessions[s.ID] = s
		result = map[string]any{"data": []any{}}
	default:
		status = 404
	}
	f.mu.Unlock()
	jsonResponse(w, status, result)
}
func (f *Server) page(data []any, cursor string) map[string]any {
	start, _ := strconv.Atoi(cursor)
	if start < 0 || start > len(data) {
		start = len(data)
	}
	end := min(start+f.pageSize, len(data))
	var next any
	if end < len(data) {
		next = strconv.Itoa(end)
	}
	return map[string]any{"data": data[start:end], "next_page": next}
}
