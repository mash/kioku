package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

func agentJobPrefix(job model.Job) string {
	return "projects/" + job.ProjectID + "/.kioku/jobs/" + job.ID + "/"
}
func acceptsAgentIO(job model.Job) bool {
	if job.Status != "running" || job.Run == nil {
		return false
	}
	switch job.Run.Phase {
	case model.PhasePreparing, model.PhaseAgentRunning, model.PhaseOutputReceived:
		return true
	}
	return false
}
func agentIOJob(st model.State, principal model.Session, pid, jid string) (model.Job, error) {
	if err := checkJobToken(st, principal, pid, jid); err != nil {
		return model.Job{}, err
	}
	job, ok := st.Jobs[jid]
	if !ok || job.ProjectID != pid || !acceptsAgentIO(job) {
		return model.Job{}, fail(409, "job no longer accepts agent input or output")
	}
	return job, nil
}
func (s *Server) agentInput(w http.ResponseWriter, r *http.Request, uid string) error {
	// Bound archive memory before reading either object bytes or request bodies.
	ioUnlock, gateErr := s.lockProject(r.Context(), "agent-io:"+r.PathValue("project"))
	if gateErr != nil {
		return gateErr
	}
	defer ioUnlock()

	_, principal := s.session(r)
	var job model.Job
	if err := s.DB.View(func(st model.State) error {
		var e error
		job, e = agentIOJob(st, principal, r.PathValue("project"), r.PathValue("job"))
		return e
	}); err != nil {
		return err
	}
	b, err := s.Blobs.Get(r.Context(), agentJobKey(job, "input.tar.gz"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/gzip")
	_, err = w.Write(b)
	return err
}
func (s *Server) outputContext(ctx context.Context, job model.Job) (map[string]string, []model.InboxItem, error) {
	previous := map[string]string{}
	var rev model.Revision
	var items []model.InboxItem
	err := s.DB.View(func(st model.State) error {
		if err := validateGeneration(st, job); err != nil {
			return err
		}
		rev = st.Revisions[st.Projects[job.ProjectID].Revision]
		for _, id := range job.Run.InboxIDs {
			item, ok := st.Inbox[id]
			if !ok || item.RunID != job.Run.Attempt.ID {
				return fail(409, "generation inbox missing")
			}
			items = append(items, item)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for name, key := range rev.Files {
		if agentgen.AgentPage(name) {
			b, err := s.Blobs.Get(ctx, key)
			if err != nil {
				return nil, nil, err
			}
			previous[name] = string(b)
		}
	}
	return previous, items, nil
}
func (s *Server) outputLimits() agentgen.OutputLimits {
	return agentgen.OutputLimits{MaxCompressed: 2 * s.Config.MaxWikiBytes, MaxUncompressed: 2 * s.Config.MaxWikiBytes}
}
func (s *Server) agentOutput(w http.ResponseWriter, r *http.Request, uid string) error {
	// Bound archive memory before reading either object bytes or request bodies.
	ioUnlock, gateErr := s.lockProject(r.Context(), "agent-io:"+r.PathValue("project"))
	if gateErr != nil {
		return gateErr
	}
	defer ioUnlock()

	_, principal := s.session(r)
	var job model.Job
	if err := s.DB.View(func(st model.State) error {
		var e error
		job, e = agentIOJob(st, principal, r.PathValue("project"), r.PathValue("job"))
		return e
	}); err != nil {
		return err
	}
	lim := s.outputLimits()
	r.Body = http.MaxBytesReader(w, r.Body, lim.MaxCompressed)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": []string{"compressed output exceeds twice max-wiki-bytes"}})
			return nil
		}
		return fail(400, "could not read output")
	}
	pages, err := agentgen.ReadOutput(bytes.NewReader(body), lim)
	if err != nil {
		if errors.Is(err, agentgen.ErrOutputTooLarge) {
			jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": []string{"expanded output exceeds twice max-wiki-bytes"}})
			return nil
		}
		var invalid *agentgen.OutputError
		if errors.As(err, &invalid) {
			jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": invalid.Problems})
			return nil
		}
		return err
	}
	previous, items, err := s.outputContext(r.Context(), job)
	if err != nil {
		return err
	}
	if problems := wiki.ValidateSources(pages, previous, items); len(problems) > 0 {
		jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": problems})
		return nil
	}
	// Serialize object writes against source/project deletion and cleanup.
	unlock, err := s.lockProject(r.Context(), job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	var selectedKey blob.Key
	if err = s.DB.View(func(st model.State) error {
		current, e := agentIOJob(st, principal, job.ProjectID, job.ID)
		if e == nil {
			selectedKey = current.Run.OutputKey
		}
		return e
	}); err != nil {
		return err
	}
	// Retain at most the selected output plus one replacement. If deletion is
	// unavailable, stop before writing another version rather than accumulating.
	keys, err := s.Blobs.List(r.Context(), agentJobPrefix(job)+"output-")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key != selectedKey {
			if err = s.Blobs.Delete(r.Context(), key); err != nil {
				return fail(503, "previous output cleanup pending")
			}
		}
	}
	hash := auth.Hash(string(body))
	key := agentJobKey(job, "output-"+hash+".tar.gz")
	if err = s.Blobs.Put(r.Context(), key, body); err != nil {
		return err
	}
	selected := false
	var previousKey blob.Key
	err = s.DB.Update(func(st *model.State) error {
		current := st.Jobs[job.ID]
		selected = current.Run != nil && current.Run.OutputKey == key
		current, e := agentIOJob(*st, principal, job.ProjectID, job.ID)
		if e != nil {
			return e
		}
		previousKey = current.Run.OutputKey
		current.Run.OutputKey = key
		current.Run.OutputHash = hash
		current.Run.Phase = model.PhaseOutputReceived
		st.Jobs[job.ID] = current
		return nil
	})
	if err != nil {
		// Identical retries share a key: never delete the version being finalized.
		if !selected {
			if cleanupErr := s.Blobs.Delete(r.Context(), key); cleanupErr != nil {
				slog.Error("rejected agent output cleanup failed", "job", job.ID, "error", cleanupErr)
			}
		}
		return err
	}
	if previousKey != "" && previousKey != key {
		if err = s.Blobs.Delete(r.Context(), previousKey); err != nil {
			slog.Warn("superseded agent output cleanup deferred", "job", job.ID, "error", err)
		}
	}
	jsonOut(w, 200, map[string]any{"pages": len(pages), "bytes": len(body), "sha256": hash})
	return nil
}

func agentJobKey(job model.Job, name string) blob.Key { return blob.Key(agentJobPrefix(job) + name) }
