package server

import (
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

func attempt(job model.Job) runner.Attempt {
	return runner.Attempt{Key: runner.AttemptKey{JobID: job.ID, AttemptID: job.Run.Attempt.ID, ProjectID: job.ProjectID}, Run: job.Run.Ref}
}
