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
	CreatedAt                        time.Time
	Archived, Deleted, Interrupted   bool
	Events                           []map[string]any
}
type Server struct {
	HTTP      *httptest.Server
	mu        sync.Mutex
	requests  []Request
	sessions  map[string]Session
	files     map[string]file
	outputs   map[string][]byte
	errors    map[string]int
	pageSize  int
	onSession func(string)
}

func New() *Server {
	f := &Server{sessions: map[string]Session{}, files: map[string]file{}, outputs: map[string][]byte{}, errors: map[string]int{}, pageSize: 100}
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
func (f *Server) Session(id string) Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneSession(f.sessions[id])
}
func cloneSession(s Session) Session {
	s.Metadata = maps.Clone(s.Metadata)
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
	v := map[string]any{"id": s.ID, "type": "session", "status": s.Status, "metadata": s.Metadata, "created_at": s.CreatedAt.Format(time.RFC3339Nano), "usage": map[string]any{"list_cost": map[string]string{"amount": s.CostAmount, "currency": s.Currency}}}
	if s.Archived {
		v["archived_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return v
}
func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Body != nil && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
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
	if parts[1] == "agents" && len(parts) == 3 {
		f.mu.Unlock()
		jsonResponse(w, 200, map[string]any{"id": parts[2], "version": 3, "execution_identity": map[string]string{"type": "service_account"}})
		return
	}
	if parts[1] == "environments" && len(parts) == 3 {
		f.mu.Unlock()
		jsonResponse(w, 200, map[string]any{"id": parts[2], "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []string{}}}})
		return
	}
	if parts[1] == "files" {
		f.serveFiles(w, r, parts)
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
		s := Session{ID: id, Status: "running", CostAmount: "0", Currency: "USD", Metadata: metadata, CreatedAt: time.Now().UTC()}
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
			if !s.Deleted && (!s.Archived || r.URL.Query().Get("include_archived") == "true") {
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
	if !ok || s.Deleted {
		f.mu.Unlock()
		jsonResponse(w, 404, nil)
		return
	}
	var result any
	status := 200
	switch {
	case len(parts) == 3 && r.Method == "GET":
		result = wireSession(s)
	case len(parts) == 3 && r.Method == "DELETE":
		s.Deleted = true
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

// file contents and session output stand in for the external LLM provider only.
type file struct {
	name string
	body []byte
}

func (f *Server) SetOutput(session string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs[session] = append([]byte(nil), body...)
}
func (f *Server) Input(id string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.files[id].body...)
}
func (f *Server) FileExists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[id]
	return ok
}
func (f *Server) serveFiles(w http.ResponseWriter, r *http.Request, parts []string) {
	defer f.mu.Unlock()
	switch {
	case len(parts) == 2 && r.Method == "POST":
		part, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "invalid file", 400)
			return
		}
		defer part.Close()
		body, err := io.ReadAll(part)
		if err != nil {
			http.Error(w, "invalid file", 400)
			return
		}
		id := fmt.Sprintf("file_%d", len(f.files)+1)
		f.files[id] = file{header.Filename, body}
		jsonResponse(w, 200, map[string]string{"id": id})
	case len(parts) == 2 && r.Method == "GET":
		data := []any{}
		if scope := r.URL.Query().Get("scope_id"); scope != "" {
			if _, ok := f.outputs[scope]; ok {
				data = append(data, map[string]any{"id": "output-" + scope, "filename": "output.tar.gz", "downloadable": true, "scope": map[string]string{"type": "session", "id": scope}})
			}
		} else {
			for id, file := range f.files {
				data = append(data, map[string]any{"id": id, "filename": file.name})
			}
		}
		jsonResponse(w, 200, map[string]any{"data": data})
	case len(parts) == 4 && parts[3] == "content" && r.Method == "GET":
		body, ok := f.outputs[strings.TrimPrefix(parts[2], "output-")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	case len(parts) == 3 && r.Method == "DELETE":
		delete(f.files, parts[2])
		jsonResponse(w, 200, map[string]string{"id": parts[2]})
	default:
		http.NotFound(w, r)
	}
}
