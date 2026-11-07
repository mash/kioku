package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
)

const anthropicTemplate = `{{define "body"}}<h1>Anthropic 接続</h1><p>生成費用は、接続した Claude Console の API 利用料金として請求されます。Claude Pro / Max の利用枠は使いません。自動生成にはこの接続を使いません。</p>{{if .State}}<p>状態: {{.State}}</p><p>Workspace: {{.WorkspaceID}}</p>{{end}}<form method="post"><p><label>Workspace ID（キーがワークスペース限定の場合は省略可）<br><input name="workspace_id" value="{{.WorkspaceID}}"></label></p><p><label>専用ワークスペースの API キー<br><input type="password" name="api_key" required autocomplete="off" size="50"></label></p><p>接続時に、このワークスペースに kioku 用の Agent と Environment を作成します。</p><button>接続・キー更新</button></form>{{if .State}}<form method="post" action="/settings/anthropic/disconnect"><p>解除すると新規生成を停止し、実行中のジョブを停止してから保存したキーを削除します。停止待ちの場合は、しばらく待ってもう一度解除してください。</p><button>接続解除</button></form>{{end}}{{end}}`

func (s *Server) anthropicPage(w http.ResponseWriter, r *http.Request, uid string) error {
	var c model.AnthropicConnection
	if err := s.DB.View(func(st model.State) error {
		if v := st.Users[uid].Anthropic; v != nil {
			c = *v
		}
		return nil
	}); err != nil {
		return err
	}
	return render(w, "Anthropic", struct{ State, WorkspaceID string }{c.State, c.WorkspaceID}, anthropicTemplate)
}
func (s *Server) saveAnthropic(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		APIKey      string `json:"api_key"`
		WorkspaceID string `json:"workspace_id"`
	}
	form := strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	if form {
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		if err := r.ParseForm(); err != nil {
			return fail(400, "invalid form")
		}
		req.APIKey = r.FormValue("api_key")
		req.WorkspaceID = r.FormValue("workspace_id")
	} else if err := decode(w, r, &req); err != nil {
		return err
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	if len(req.APIKey) < 16 || len(req.APIKey) > 4096 || strings.ContainsAny(req.APIKey, "\r\n\t ") || len(req.WorkspaceID) > 200 || strings.ContainsAny(req.WorkspaceID, "\r\n\t ") {
		return fail(400, "invalid Anthropic credentials")
	}
	origin, err := url.Parse(s.Config.BaseURL)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" {
		return fail(503, "BYOK requires a public HTTPS base URL")
	}
	unlock, err := s.lockProject(r.Context(), "anthropic:"+uid)
	if err != nil {
		return err
	}
	defer unlock()
	var c model.AnthropicConnection
	if err = s.DB.View(func(st model.State) error {
		if v := st.Users[uid].Anthropic; v != nil {
			c = *v
		}
		return nil
	}); err != nil {
		return err
	}
	existing := c.ID != ""
	if !existing {
		c.ID = auth.Token()[:20]
		c.State = "preparing"
		c.WorkspaceID = req.WorkspaceID
	}
	if c.WorkspaceID != req.WorkspaceID {
		return fail(409, "disconnect before changing workspace")
	}
	candidate := c
	candidate.EncryptedKey = s.Cipher.Seal(req.APIKey, "anthropic:"+uid+":"+c.ID)
	api, err := s.connectionClient(uid, candidate)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if c.AgentID != "" && c.EnvironmentID != "" {
		if err = api.ValidateResources(ctx, c.AgentID, c.EnvironmentID); err != nil {
			return fail(400, "key cannot access the existing Anthropic resources")
		}
	}
	if err = s.updateAccount(r, uid, func(st *model.State) error {
		u := st.Users[uid]
		u.Anthropic = &candidate
		st.Users[uid] = u
		return nil
	}); err != nil {
		return err
	}
	c = candidate
	err = api.EnsureResources(ctx, c.ID, origin.Hostname(), c.AgentID, c.EnvironmentID, func(a string, v int64, e string) error {
		return s.updateAccount(r, uid, func(st *model.State) error {
			u := st.Users[uid]
			if u.Anthropic == nil || u.Anthropic.ID != c.ID {
				return fail(409, "connection changed")
			}
			next := *u.Anthropic
			next.AgentID = a
			next.AgentVersion = v
			next.EnvironmentID = e
			u.Anthropic = &next
			st.Users[uid] = u
			return nil
		})
	})
	if err != nil {
		return fail(502, "Anthropic setup failed; verify workspace permissions and retry with the same key")
	}
	if err = s.updateAccount(r, uid, func(st *model.State) error {
		u := st.Users[uid]
		next := *u.Anthropic
		next.State = "ready"
		u.Anthropic = &next
		st.Users[uid] = u
		return nil
	}); err != nil {
		return err
	}
	if form {
		http.Redirect(w, r, "/settings/anthropic", http.StatusSeeOther)
	} else {
		jsonOut(w, 200, map[string]string{"state": "ready"})
	}
	return nil
}
func connectionBusy(st model.State, uid string) bool {
	for _, j := range st.Jobs {
		if j.UserID == uid && j.AnthropicConnectionID != "" && (j.Status == "queued" || j.Status == "running" || j.Run != nil && !j.Run.CleanupComplete) {
			return true
		}
	}
	return false
}
func (s *Server) disconnectAnthropic(w http.ResponseWriter, r *http.Request, uid string) error {
	unlock, err := s.lockProject(r.Context(), "anthropic:"+uid)
	if err != nil {
		return err
	}
	defer unlock()
	var jobs []model.Job
	if err = s.updateAccount(r, uid, func(st *model.State) error {
		u := st.Users[uid]
		if u.Anthropic == nil {
			return nil
		}
		c := *u.Anthropic
		c.State = "disconnecting"
		u.Anthropic = &c
		st.Users[uid] = u
		for id, j := range st.Jobs {
			if j.UserID != uid || j.AnthropicConnectionID != c.ID {
				continue
			}
			if j.Status == "queued" {
				j.Status = "failed"
				j.Error = "Anthropic connection disconnected"
				st.Jobs[id] = j
			}
			if j.Run != nil && !j.Run.CleanupComplete {
				jobs = append(jobs, j)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, j := range jobs {
		release, err := s.lockProject(r.Context(), "generate:"+j.ProjectID)
		if err != nil {
			return err
		}
		if j.Status == "running" {
			err = s.abortAgentJob(r.Context(), j)
		} else {
			if _, err = s.runners.New(j); err != nil {
				err = fail(503, "managed agents unavailable; retry deletion after restoring configuration")
			} else if err = s.cleanupAgentJob(r.Context(), j, nil); err != nil {
				err = fail(503, "agent cleanup pending; retry deletion")
			}
		}
		release()
		if err != nil {
			return err
		}
	}
	if err = s.updateAccount(r, uid, func(st *model.State) error {
		if connectionBusy(*st, uid) {
			return fail(503, "agent cleanup pending; retry deletion")
		}
		u := st.Users[uid]
		u.Anthropic = nil
		st.Users[uid] = u
		return nil
	}); err != nil {
		return err
	}
	if strings.HasPrefix(r.URL.Path, "/settings/") {
		http.Redirect(w, r, "/settings/anthropic", http.StatusSeeOther)
	} else {
		jsonOut(w, 200, map[string]bool{"disconnected": true})
	}
	return nil
}

func (s *Server) jobConnection(job model.Job) (model.AnthropicConnection, error) {
	var out model.AnthropicConnection
	err := s.DB.View(func(st model.State) error {
		c := st.Users[job.UserID].Anthropic
		if c == nil || c.ID != job.AnthropicConnectionID {
			return errors.New("Anthropic connection unavailable; reconnect the original workspace")
		}
		out = *c
		return nil
	})
	return out, err
}
func (s *Server) connectionClient(uid string, c model.AnthropicConnection) (*managedagents.API, error) {
	key, err := s.Cipher.Open(c.EncryptedKey, "anthropic:"+uid+":"+c.ID)
	if err != nil {
		return nil, errors.New("could not decrypt Anthropic connection")
	}
	return managedagents.New(s.Config.MABaseURL, key, nil, c.WorkspaceID), nil
}
