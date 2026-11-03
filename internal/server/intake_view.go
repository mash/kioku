package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

// The append-only journal is stored as immutable R2 segments. Readers get one
// ordinary Markdown file; generation is never allowed to replace this view.
func (s *Server) addIntakeView(ctx context.Context, uid, pid string, files map[string]string) error {
	var entries []model.IntakeEntry
	live := map[string]model.Source{}
	if err := s.DB.View(func(st model.State) error {
		if _, err := member(st, pid, uid); err != nil {
			return err
		}
		for _, e := range st.Intake {
			if e.ProjectID == pid {
				entries = append(entries, e)
			}
		}
		for id, src := range st.Sources {
			if src.ProjectID == pid {
				live[id] = src
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
	var out strings.Builder
	out.WriteString("# Project intake\n\nRecent entries from the append-only intake journal. Full journal segments remain in project storage; generated pages may be updated later.\n\n")
	for _, e := range entries {
		fmt.Fprintf(&out, "## %d · %s · %s\n\nContributor: `%s` · Job: `%s`\n\n", e.Sequence, e.Created.UTC().Format("2006-01-02T15:04:05Z"), e.Kind, e.UserID, e.JobID)
		if e.Kind == "submitted" {
			fmt.Fprintf(&out, "URL submitted: %s\n\n", e.URL)
		}
		for _, record := range e.Sources {
			src, ok := live[record.ID]
			if !ok {
				out.WriteString("Source removed.\n\n")
				continue
			}
			fmt.Fprintf(&out, "- [%s](/api/projects/%s/sources/%s/text)\n", src.Title, pid, src.ID)
		}
	}
	files["wiki/inbox.md"] = out.String()
	return nil
}
func snapshotToken(files map[string]string) string {
	b, _ := json.Marshal(files)
	return auth.Hash(string(b))[:32]
}
