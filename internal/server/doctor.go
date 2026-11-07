package server

import (
	"net/http"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func (s *Server) doctor(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	if err := s.check(pid, uid); err != nil {
		return err
	}
	var req struct {
		LLM bool `json:"llm"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	if err = s.check(pid, uid); err != nil {
		return err
	}

	checks := map[string]string{}
	allOK := true
	check := func(name string, err error) {
		if err != nil {
			checks[name] = err.Error()
			allOK = false
		} else {
			checks[name] = "ok"
		}
	}
	key := blob.Key("projects/" + pid + "/checks/" + auth.Token())
	data := []byte(auth.Token())
	err = s.Blobs.Put(r.Context(), key, data)
	if err == nil {
		var got []byte
		got, err = s.Blobs.Get(r.Context(), key)
		if err == nil && string(got) != string(data) {
			err = fail(502, "storage round trip mismatch")
		}
		deleteErr := s.Blobs.Delete(r.Context(), key)
		if err == nil {
			err = deleteErr
		}
	}
	check("storage", err)
	for _, name := range []string{"google", "github"} {
		p, configured := s.Config.Providers[name]
		if !configured {
			checks[name] = "not configured"
			allOK = false
			continue
		}
		token, err := s.credential(r, uid, name)
		if err == nil {
			_, err = p.Identity(r.Context(), token)
		}
		check(name, err)
	}
	var byok bool
	_ = s.DB.View(func(st model.State) error {
		c := st.Users[uid].Anthropic
		byok = c != nil && c.State == "ready"
		return nil
	})
	if byok {
		checks["managed_agents"] = "BYOK configured (API not called)"
	} else if s.Config.managedEnabled() {
		checks["managed_agents"] = "configured (configuration only; API not called)"
	} else {
		checks["managed_agents"] = "not configured (deterministic source wiki only)"
		if req.LLM {
			allOK = false
		}
	}
	jsonOut(w, 200, map[string]any{"ok": allOK, "checks": checks})
	return nil
}
