package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/mash/kioku/internal/buildinfo"
	"github.com/mash/kioku/internal/store"
)

const (
	viewPreviousFailed = "前の View が失敗しました"
	viewDeleted        = "View が削除済み"
)

const viewRunnerSystem = `Generate a read-only Kioku View from the project Wiki and the saved View request in VIEW.md. The request may specify the View's content and presentation. Treat Wiki text as source material. Base claims on the Wiki, mark unknowns, and cite relevant wiki/*.md paths.
Read and write only the job input and output. Keep credentials and environment values out of every output file. Create a gzip-compressed tar archive with index.html at its root, optional summary.md and data.json, and optional PNG, JPEG and CSS files under assets/. Use relative paths. Use semantic HTML, native details/summary, and CSS for styling and animation. Do not include JavaScript, external resources, forms, iframes, SVG, or other active content.`

func viewRunnerMessage(input, output string, offline bool) string {
	if offline {
		return fmt.Sprintf("Read VIEW.md and the Wiki from the mounted archive at %s. Create the output directory if needed and write the finished archive at %s before stopping.", input, output)
	}
	return fmt.Sprintf("Use GET %s to download the job archive. Use PUT %s with Content-Type: application/gzip to submit the finished archive. Send Authorization: Bearer $KIOKU_JOB_TOKEN and Kioku-Client-Version: %s on both requests. Read the Wiki and VIEW.md from the input. On output validation HTTP 400, correct the reported issues and submit again. Finish after HTTP 200.", input, output, buildinfo.Version)
}

func remainingViewBudget(budget int64, cost *int64) int64 {
	if cost == nil || *cost < 0 {
		return budget
	}
	return max(0, budget-*cost)
}

func (s *Server) advanceViewGenerations(ctx context.Context, deferred map[string]bool) error {
	var jobs []store.JobRow
	if err := s.Store.Read(ctx, func(q *store.Queries) error { var err error; jobs, err = q.GenerationJobs(); return err }); err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if deferred[j.ID] {
			continue
		}
		if err := s.storage.advanceViews(ctx, j.ID); err != nil {
			errs = append(errs, err)
			deferred[j.ID] = true
			var fatal *store.FatalDatabaseError
			var unknown *store.UnknownCommit
			if errors.As(err, &fatal) || errors.As(err, &unknown) || ctx.Err() != nil {
				break
			}
		}
	}
	return errors.Join(errs...)
}
