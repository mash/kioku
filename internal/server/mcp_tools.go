package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"github.com/mash/kioku/internal/wiki"
)

func decodeMCPArguments(raw json.RawMessage, value any) bool {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return false
	}
	for _, field := range fields {
		if string(field) == "null" {
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return false
	}
	// encoding/json matches field names case-insensitively, so compare each key
	// with the exact names that value marshals to.
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var allowed map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &allowed); err != nil {
		return false
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return false
		}
	}
	return true
}

func (s *Server) mcpCall(ctx context.Context, key string, raw json.RawMessage) (any, error) {
	invalid := fail(400, "unknown tool or invalid arguments")
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalid
	}
	switch params.Name {
	case "cli_login_code":
		var legacy struct {
			Name      string `json:"name"`
			Arguments struct {
				Project string `json:"project"`
				Role    string `json:"role"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, invalid
		}
		code, expires, err := s.cliLoginCode(key, legacy.Arguments.Project, legacy.Arguments.Role)
		return map[string]any{"code": code, "expires_at": expires, "command": "kioku agent login " + code}, err
	case "projects_list":
		if !decodeMCPArguments(params.Arguments, &struct{}{}) {
			return nil, invalid
		}
		return s.mcpProjects(key)
	case "wiki_read":
		var args struct {
			Project *string `json:"project"`
			Path    *string `json:"path"`
			Offset  *int    `json:"offset"`
		}
		if !decodeMCPArguments(params.Arguments, &args) || args.Project == nil {
			return nil, invalid
		}
		name, offset := "wiki/index.md", 0
		if args.Path != nil {
			name = *args.Path
		}
		if args.Offset != nil {
			offset = *args.Offset
		}
		return s.mcpWikiRead(ctx, key, *args.Project, name, offset)
	case "wiki_search":
		var args struct {
			Project *string `json:"project"`
			Query   *string `json:"query"`
		}
		if !decodeMCPArguments(params.Arguments, &args) || args.Project == nil || args.Query == nil {
			return nil, invalid
		}
		return s.mcpWikiSearch(ctx, key, *args.Project, *args.Query)
	case "source_put":
		var args struct {
			Project *string `json:"project"`
			Name    *string `json:"name"`
			Content *string `json:"content"`
		}
		if !decodeMCPArguments(params.Arguments, &args) || args.Project == nil || args.Name == nil || args.Content == nil {
			return nil, invalid
		}
		return s.mcpSourcePut(ctx, key, *args.Project, *args.Name, *args.Content)
	default:
		return nil, invalid
	}
}

func (s *Server) mcpProjects(key string) (any, error) {
	projects := []map[string]string{}
	err := s.DB.View(func(st model.State) error {
		grant, ok := connectorGrant(st, key)
		if !ok {
			return fail(401, "connector login required")
		}
		for _, p := range st.Projects {
			if role := p.Members[grant.UserID]; role != "" && !p.Deleting && slices.Contains(grant.ProjectIDs, p.ID) {
				projects = append(projects, map[string]string{"namespace": p.Namespace, "name": p.Name, "role": role})
			}
		}
		return nil
	})
	sort.Slice(projects, func(i, j int) bool { return projects[i]["namespace"] < projects[j]["namespace"] })
	return map[string]any{"projects": projects}, err
}

func (s *Server) connectorWikiFiles(ctx context.Context, key, namespace string) (model.Revision, map[string]string, func(), error) {
	var project model.Project
	var uid string
	if err := s.DB.View(func(st model.State) error {
		var err error
		project, uid, err = connectorProject(st, key, namespace)
		return err
	}); err != nil {
		return model.Revision{}, nil, nil, err
	}
	return s.wikiFiles(ctx, uid, project.ID)
}

func (s *Server) mcpWikiRead(ctx context.Context, key, namespace, name string, offset int) (any, error) {
	rev, views, unlock, err := s.connectorWikiFiles(ctx, key, namespace)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if !wiki.ValidFile(name) {
		return nil, fail(400, "invalid wiki path")
	}
	_, viewExists := views[name]
	_, fileExists := rev.Files[name]
	if !viewExists && !fileExists {
		switch {
		case rev.ID == "" && len(views) == 0:
			return nil, fail(404, "project has no wiki or intake yet")
		case rev.ID == "":
			return nil, fail(404, "wiki not generated yet; read wiki/inbox.md")
		default:
			return nil, fail(404, "wiki file not found; read maps/graph.json for the page list")
		}
	}
	body, err := s.wikiFile(ctx, rev, views, name)
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > len(body) || (offset < len(body) && !utf8.RuneStart(body[offset])) {
		return nil, fail(400, "invalid offset")
	}
	end := min(offset+32768, len(body))
	for end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	var next *int
	if end < len(body) {
		next = &end
	}
	return map[string]any{"project": namespace, "path": name, "file_bytes": len(body), "offset": offset, "next_offset": next, "content": body[offset:end]}, nil
}

func (s *Server) mcpWikiSearch(ctx context.Context, key, namespace, query string) (any, error) {
	rev, views, unlock, err := s.connectorWikiFiles(ctx, key, namespace)
	if err != nil {
		return nil, err
	}
	defer unlock()
	query = strings.TrimSpace(query)
	if len(query) == 0 || len(query) > 200 {
		return nil, fail(400, "query must be 1-200 bytes")
	}
	names := map[string]bool{}
	for name := range rev.Files {
		if wiki.ValidFile(name) {
			names[name] = true
		}
	}
	for name := range views {
		if wiki.ValidFile(name) {
			names[name] = true
		}
	}
	paths := make([]string, 0, len(names))
	for name := range names {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	results := []map[string]any{}
	truncated := false
	lowerQuery := strings.ToLower(query)
	for _, name := range paths {
		body, err := s.wikiFile(ctx, rev, views, name)
		if err != nil {
			return nil, err
		}
		lineNumber := 0
		for line := range strings.SplitSeq(body, "\n") {
			lineNumber++
			lower := strings.ToLower(line)
			match := strings.Index(lower, lowerQuery)
			if match < 0 {
				continue
			}
			if len(results) == 50 {
				truncated = true
				break
			}
			// strings.ToLower can change the byte length of a rune, so map the
			// match offset in lower back to an offset in line.
			origin, lowered := len(line), 0
			for i, r := range line {
				if lowered >= match {
					origin = i
					break
				}
				lowered += len(strings.ToLower(string(r)))
			}
			start := max(0, origin-120)
			end := min(start+240, len(line))
			for start < end && !utf8.RuneStart(line[start]) {
				start++
			}
			for end < len(line) && !utf8.RuneStart(line[end]) {
				end--
			}
			results = append(results, map[string]any{"path": name, "line": lineNumber, "text": line[start:end]})
		}
		if truncated {
			break
		}
	}
	return map[string]any{"project": namespace, "query": query, "results": results, "truncated": truncated}, nil
}

func (s *Server) mcpSourcePut(ctx context.Context, key, namespace, name, content string) (any, error) {
	var project model.Project
	var uid string
	if err := s.DB.View(func(st model.State) error {
		var err error
		project, uid, err = connectorProject(st, key, namespace)
		return err
	}); err != nil {
		return nil, err
	}
	doc, err := source.File(name, content)
	if err != nil {
		return nil, fail(400, fmt.Sprintf("%s: %v", name, err))
	}
	job := model.Job{ID: auth.Token()[:20], Kind: "put", ProjectID: project.ID, UserID: uid, Created: time.Now().UTC()}
	job, err = s.storeSources(ctx, []source.Document{doc}, job)
	if err != nil {
		return nil, err
	}
	s.recordAudit(project.ID, uid, "POST", "mcp source_put")
	return map[string]any{"job_id": job.ID, "source_ids": job.SourceIDs}, nil
}
