package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/buildinfo"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

type connectorAccess struct {
	UserID string
	store.MCPData
}

func connectorGrant(q *store.Queries, key string) (connectorAccess, bool, error) {
	row, err := q.Session(key)
	if errors.Is(err, sql.ErrNoRows) {
		return connectorAccess{}, false, nil
	}
	if err != nil {
		return connectorAccess{}, false, err
	}
	if row.Kind != "mcp" || !row.Active(time.Now().UnixMilli()) {
		return connectorAccess{}, false, nil
	}
	data, err := store.SessionData[store.MCPData](row)
	if err != nil {
		return connectorAccess{}, false, err
	}
	if _, err := q.User(row.UserID); errors.Is(err, sql.ErrNoRows) {
		return connectorAccess{}, false, nil
	} else if err != nil {
		return connectorAccess{}, false, err
	}
	return connectorAccess{row.UserID, data}, data.TokenKind == "access" && len(data.ProjectIDs) > 0, nil
}
func connectorProject(q *store.Queries, key, namespace string) (model.Project, string, error) {
	grant, ok, err := connectorGrant(q, key)
	if err != nil {
		return model.Project{}, "", err
	}
	if !ok {
		return model.Project{}, "", fail(401, "connector login required")
	}
	parts := strings.Split(namespace, "/")
	if len(parts) != 2 {
		return model.Project{}, "", fail(404, "project not found")
	}
	project, err := namespaceProject(q, parts[0], parts[1], grant.UserID)
	var problem apiError
	if err != nil && !errors.As(err, &problem) {
		return model.Project{}, "", err
	}
	if !slices.Contains(grant.ProjectIDs, project.ID) {
		return model.Project{}, "", fail(404, "project not found")
	}
	return project, grant.UserID, err
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	key := auth.Hash(raw)
	valid := false
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error { var err error; _, valid, err = connectorGrant(q, key); return err }); err != nil {
		s.requestError(w, r, err)
		return
	}
	if !bearer || !valid {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.Config.BaseURL+`/.well-known/oauth-protected-resource"`)
		writeError(w, fail(401, "connector login required"))
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := decode(w, r, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		mcpError(w, nil, -32600, "Invalid Request")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version != "2024-11-05" && version != "2025-03-26" && version != "2025-06-18" {
			version = "2025-06-18"
		}
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "kioku", "version": buildinfo.Version}}
		if instructions := s.Config.Prompts.Instructions(); instructions != "" {
			result.(map[string]any)["instructions"] = instructions
		}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = s.mcpTools
	case "tools/call":
		value, err := s.mcpCall(r.Context(), key, req.Params)
		if err != nil {
			if e, ok := err.(apiError); ok {
				result = mcpText(e.message, true)
			} else {
				s.requestError(w, r, err)
				return
			}
			break
		}
		body, _ := json.Marshal(value)
		result = mcpText(string(body), false)
	default:
		mcpError(w, req.ID, -32601, "Method not found")
		return
	}
	jsonOut(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func mcpError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	jsonOut(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func mcpText(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": isError}
}

func (s *Server) cliLoginCode(grantKey, namespace, role string) (string, time.Time, error) {
	if role == "" {
		role = "read"
	}
	if role != "read" && role != "write" {
		return "", time.Time{}, fail(400, "role must be read or write")
	}
	raw := "kioku_code." + auth.Token()
	expires := storageDate(time.Now().Add(time.Minute).UnixMilli())
	err := s.Store.Write(context.Background(), func(q *store.Queries) error {
		project, uid, err := connectorProject(q, grantKey, namespace)
		if err != nil {
			return err
		}
		data, err := store.EncodeSessionData(store.LoginCodeData{GrantKey: grantKey, Role: &role})
		if err != nil {
			return err
		}
		err = q.InsertSession(store.SessionRow{ID: auth.Hash(raw), Kind: "login_code", Status: "pending", UserID: uid, ProjectID: &project.ID, Expires: expires.UnixMilli(), Created: time.Now().UnixMilli(), Data: data}, time.Now().UnixMilli())
		if errors.Is(err, store.ErrSessionCapacity) {
			return fail(429, "too many login codes")
		}
		return err
	})
	return raw, expires, err
}

func (s *Server) agentLogin(w http.ResponseWriter, r *http.Request, _ string) error {
	var req struct {
		Code      string `json:"code"`
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	id, secret := auth.Token()[:20], auth.Token()
	raw := "kioku_agent." + id + "." + secret
	var row store.SessionRow
	var namespace string
	if err := s.Store.Write(r.Context(), func(q *store.Queries) error {
		var err error
		row, namespace, err = exchangeAgentLogin(q, req.Code, req.Name, req.Namespace, id, secret, time.Now())
		return err
	}); err != nil {
		return s.resolveExchange(r.Context(), err, 401)
	}
	data, err := store.SessionData[store.AgentSessionData](row)
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]any{"id": id, "token": raw, "role": store.ValueString(data.Role), "expires": storageDate(row.Expires), "namespace": namespace})
	return nil
}

// mcpToolSchemas returns fresh fixed tool schemas. Descriptions come only from YAML.
func mcpToolSchemas() []any {
	return []any{map[string]any{
		"name": "cli_login_code",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"project": map[string]any{"type": "string"},
			"role":    map[string]any{"type": "string", "enum": []string{"read", "write"}, "default": "read"},
		}, "required": []string{"project"}, "additionalProperties": false},
	}, map[string]any{
		"name":        "projects_list",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	}, map[string]any{
		"name": "wiki_read",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"project": map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string", "default": "wiki/index.md"},
			"offset":  map[string]any{"type": "integer", "minimum": 0, "default": 0},
		}, "required": []string{"project"}, "additionalProperties": false},
	}, map[string]any{
		"name": "wiki_search",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"project": map[string]any{"type": "string"},
			"query":   map[string]any{"type": "string"},
		}, "required": []string{"project", "query"}, "additionalProperties": false},
	}, map[string]any{
		"name": "source_put",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"remove":     map[string]any{"type": "boolean"},
			"project":    map[string]any{"type": "string"},
			"name":       map[string]any{"type": "string"},
			"content":    map[string]any{"type": "string"},
			"source_url": map[string]any{"type": "string"},
		}, "required": []string{"project"}, "additionalProperties": false,
			"oneOf": []any{
				map[string]any{"required": []string{"name", "content"}, "properties": map[string]any{"remove": map[string]any{"const": false}}, "not": map[string]any{"required": []string{"source_url"}}},
				map[string]any{"required": []string{"remove", "source_url"}, "properties": map[string]any{"remove": map[string]any{"const": true}}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"name"}}, map[string]any{"required": []string{"content"}}}}},
			},
		},
	}, map[string]any{
		"name": "view_list",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"project": map[string]any{"type": "string"},
		}, "required": []string{"project"}, "additionalProperties": false},
	}, map[string]any{
		"name": "view_read",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"project": map[string]any{"type": "string"},
			"view":    map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string", "enum": viewTextPaths},
			"offset":  map[string]any{"type": "integer", "minimum": 0, "default": 0},
		}, "required": []string{"project", "view", "path"}, "additionalProperties": false},
	}}
}

// mcpTools builds the tools/list snapshot once, alongside the handlers' schemas.
func mcpTools(rendered *prompts.Rendered) map[string]any {
	tools := mcpToolSchemas()
	descriptions := rendered.MCP()
	for _, item := range tools {
		tool := item.(map[string]any)
		name := tool["name"].(string)
		description := descriptions.Tools[name]
		tool["description"] = description.Description
		for propertyName, property := range tool["inputSchema"].(map[string]any)["properties"].(map[string]any) {
			if text := description.Properties[propertyName].Description; text != "" {
				property.(map[string]any)["description"] = text
			}
		}
	}
	return map[string]any{"tools": tools}
}

// MCPToolNames supplies the loader with the names defined by the tool handlers.
func MCPToolNames() map[string][]string {
	names := map[string][]string{}
	for _, item := range mcpToolSchemas() {
		tool := item.(map[string]any)
		name := tool["name"].(string)
		names[name] = []string{}
		for property := range tool["inputSchema"].(map[string]any)["properties"].(map[string]any) {
			names[name] = append(names[name], property)
		}
	}
	return names
}
