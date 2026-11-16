package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
)

func (s *Server) agentInput(w http.ResponseWriter, r *http.Request, uid string) error {
	unlock, err := s.storage.lock(r.Context(), r.PathValue("project"))
	if err != nil {
		return err
	}
	defer unlock()
	var job store.JobRow
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		var err error
		job, err = checkSQLJobCredential(q, s.Cipher, requestBearer(r), r.PathValue("project"), r.PathValue("job"), time.Now())
		return err
	}); err != nil {
		return err
	}
	body, err := s.Blobs.Get(r.Context(), sqlJobKey(job, "input.tar.gz"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/gzip")
	_, err = w.Write(body)
	return err
}
func (s *Server) outputContext(ctx context.Context, job store.JobRow) (map[string]string, []model.InboxItem, error) {
	unlock, err := s.storage.lock(ctx, job.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	var rev wikiTree
	items := []model.InboxItem{}
	err = s.Store.Read(ctx, func(q *store.Queries) error {
		_, err := s.storage.runningJob(q, job.ID)
		if err != nil {
			return err
		}
		p, err := q.Project(job.ProjectID)
		if err != nil {
			return err
		}
		rev, err = wikiProjection(q, p)
		if err != nil {
			return err
		}
		pinned, err := q.RunInbox(job.ProjectID, job.ID)
		if err != nil {
			return err
		}
		for _, item := range pinned {
			items = append(items, inboxProjection(item))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	previous, err := s.generationWiki(ctx, rev)
	return previous, items, err
}
func (s *Server) generationWiki(ctx context.Context, rev wikiTree) (map[string]string, error) {
	previous := map[string]string{}
	for name := range rev.Files {
		if !agentgen.AgentPage(name) {
			continue
		}
		body, err := s.wikiFile(ctx, rev, name)
		if err != nil {
			return nil, err
		}
		previous[name] = body
	}
	return previous, nil
}
func (s *Server) outputLimits() agentgen.OutputLimits {
	return agentgen.OutputLimits{MaxCompressed: 2 * s.Config.MaxWikiBytes, MaxUncompressed: 2 * s.Config.MaxWikiBytes}
}

func (s *Server) agentOutputLimit(job store.JobRow) int64 {
	if job.ParentID != nil {
		return maxViewArchiveBytes
	}
	return s.outputLimits().MaxCompressed
}
func (s *Server) agentOutput(w http.ResponseWriter, r *http.Request, uid string) error {
	unlock, err := s.storage.lock(r.Context(), "agent-io:"+r.PathValue("project"))
	if err != nil {
		return err
	}
	defer unlock()
	var job store.JobRow
	check := func(q *store.Queries) (store.JobRow, error) {
		return checkSQLJobCredential(q, s.Cipher, requestBearer(r), r.PathValue("project"), r.PathValue("job"), time.Now())
	}
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error { var err error; job, err = check(q); return err }); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.agentOutputLimit(job))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem := "compressed output exceeds twice the Wiki limit"
			if job.ParentID != nil {
				if err := s.recordViewOutputRejection(r.Context(), job.ID, check, errViewLimit); err != nil {
					return err
				}
				problem = errViewLimit.Error()
			}
			jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": []string{problem}})
			return nil
		}
		return fail(400, "could not read output")
	}
	pages, hash, err := s.saveAgentOutput(r.Context(), job, body, check, func(q *store.Queries) error { return s.auditMutation(q, r, uid, job.ProjectID) })
	if err != nil {
		var invalid *agentgen.OutputError
		if errors.As(err, &invalid) {
			jsonOut(w, 400, map[string]any{"error": "invalid output", "problems": invalid.Problems})
			return nil
		}
		return err
	}
	jsonOut(w, 200, map[string]any{"pages": pages, "bytes": len(body), "sha256": hash})
	return nil
}
func (s *Server) saveAgentOutput(ctx context.Context, job store.JobRow, body []byte, check func(*store.Queries) (store.JobRow, error), commit ...func(*store.Queries) error) (int, string, error) {
	var count int
	if job.ParentID != nil {
		bundle, err := readViewRunnerOutput(body)
		if err != nil {
			if saveErr := s.recordViewOutputRejection(ctx, job.ID, check, err); saveErr != nil {
				return 0, "", saveErr
			}
			return 0, "", &agentgen.OutputError{Problems: []string{err.Error()}}
		}
		count = len(bundle.files)
	} else {
		var err error
		count, err = s.validateWikiOutput(ctx, job, body)
		if err != nil {
			return 0, "", err
		}
	}
	hash, err := s.storage.saveOutput(ctx, job.ID, body, check, commit...)
	return count, hash, err
}
func (s *Server) recordViewOutputRejection(ctx context.Context, id string, check func(*store.Queries) (store.JobRow, error), rejection error) error {
	return s.Store.Write(ctx, func(q *store.Queries) error {
		j, err := s.storage.outputJob(q, id, check)
		if err != nil {
			return err
		}
		if j.OutputHash == nil {
			j.Data.Run.OutputRejection = store.NullableString(rejection.Error())
			return q.PutJob(j)
		}
		return nil
	})
}
func (s *Server) validateWikiOutput(ctx context.Context, job store.JobRow, body []byte) (int, error) {
	lim := s.outputLimits()
	if int64(len(body)) > lim.MaxCompressed {
		return 0, &agentgen.OutputError{Problems: []string{"compressed output exceeds twice the Wiki limit"}}
	}
	pages, err := agentgen.ReadOutput(bytes.NewReader(body), lim)
	if errors.Is(err, agentgen.ErrOutputTooLarge) {
		return 0, &agentgen.OutputError{Problems: []string{"expanded output exceeds twice the Wiki limit"}}
	}
	if err != nil {
		return 0, err
	}
	previous, items, err := s.outputContext(ctx, job)
	if err != nil {
		return 0, err
	}
	if problems := wiki.ValidateSources(pages, previous, items); len(problems) > 0 {
		return 0, &agentgen.OutputError{Problems: problems}
	}
	return len(pages), nil
}
