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
	if job.Status != "running" || job.Managed == nil {
		return false
	}
	switch job.Managed.Phase {
	case model.PhasePreparing, model.PhaseAgentRunning, model.PhaseOutputReceived:
		return true
	}
	return false
}
func agentIOJob(st model.State, principal model.Session, pid, jid string) (model.Job, error) {
	if err := checkGenerator(st, principal, pid, jid); err != nil {
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
func (s *Server) managedInputs(ctx context.Context, job model.Job) ([]wiki.Input, error) {
	var sources []model.Source
	if err := s.DB.View(func(st model.State) error {
		if job.Managed == nil {
			return fail(409, "managed job missing")
		}
		for _, id := range job.Managed.SourceIDs {
			src, ok := st.Sources[id]
			if !ok || src.ProjectID != job.ProjectID {
				return fail(409, "generation inputs were removed")
			}
			sources = append(sources, src)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	inputs := make([]wiki.Input, 0, len(sources))
	for _, src := range sources {
		b, err := s.Blobs.Get(ctx, src.TextKey)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, wiki.Input{Source: src, Text: string(b)})
	}
	return inputs, nil
}
func outputTooLarge(w http.ResponseWriter, size, limit int64) {
	jsonOut(w, 413, map[string]any{"error": "wiki too large", "bytes": size, "limit": limit})
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
	lim := agentgen.DefaultOutputLimits()
	r.Body = http.MaxBytesReader(w, r.Body, lim.MaxCompressed)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			outputTooLarge(w, lim.MaxCompressed+1, lim.MaxCompressed)
			return nil
		}
		return fail(400, "could not read output")
	}
	pages, err := agentgen.ReadOutput(bytes.NewReader(body), lim)
	if err != nil {
		if errors.Is(err, agentgen.ErrOutputTooLarge) {
			outputTooLarge(w, lim.MaxUncompressed+1, lim.MaxUncompressed)
			return nil
		}
		var invalid *agentgen.OutputError
		if errors.As(err, &invalid) {
			jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": invalid.Problems})
			return nil
		}
		return err
	}
	inputs, err := s.managedInputs(r.Context(), job)
	if err != nil {
		return err
	}
	if _, err = wiki.Assemble(inputs, pages); err != nil {
		var tooLarge *wiki.SnapshotTooLargeError
		if errors.As(err, &tooLarge) {
			outputTooLarge(w, tooLarge.Bytes, 64<<20)
			return nil
		}
		return err
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
			selectedKey = current.Managed.OutputKey
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
		selected = current.Managed != nil && current.Managed.OutputKey == key
		current, e := agentIOJob(*st, principal, job.ProjectID, job.ID)
		if e != nil {
			return e
		}
		previousKey = current.Managed.OutputKey
		current.Managed.OutputKey = key
		current.Managed.OutputHash = hash
		current.Managed.Phase = model.PhaseOutputReceived
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
