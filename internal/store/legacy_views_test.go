package store

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func migrationZIP(t *testing.T, text string) []byte {
	t.Helper()
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	f, e := w.Create("index.html")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write([]byte(text)); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	return body.Bytes()
}
func TestMigrationSelectsCommittedViewResultAndSettings(t *testing.T) {
	for _, which := range []string{"next preserves latest failure", "prior restores result", "stale epoch clears references"} {
		t.Run(which, func(t *testing.T) {
			path, b, c, content := migrationFixture(t)
			content.ViewEpoch = "current"
			b.set(t, "projects/p/.kioku/state.json", content)
			priorBody, nextBody := migrationZIP(t, "old"), migrationZIP(t, "new")
			priorHash, nextHash := string(model.HashContent(priorBody)), string(model.HashContent(nextBody))
			now := time.Unix(100, 123456789).UTC()
			prior := legacyViewResult{Status: "completed", JobID: "old-job", WikiSnapshotID: "old-wiki", GeneratedAt: &now}
			next := legacyViewResult{Status: "completed", JobID: "new-job", WikiSnapshotID: "new-wiki", GeneratedAt: &now}
			view := legacyViewDefinition{ID: "v", Slug: "map", Title: "old title", Prompt: "old prompt", CreatedAt: &now, legacyViewResult: legacyViewResult{Status: "failed", Error: "latest failure", JobID: "new-job", WikiSnapshotID: "new-wiki", GeneratedAt: &now}}
			entry := legacyViewEntry{ID: "v", Version: "committed-edit", Result: "new-job", Hash: nextHash, Epoch: "current"}
			if which == "prior restores result" {
				entry.Result = "old-job"
				entry.Hash = priorHash
			}
			if which == "stale epoch clears references" {
				entry.Epoch = "stale"
			}
			b.set(t, "projects/p/.kioku/views/index.json", legacyViewIndex{Views: map[string]legacyViewEntry{"map": entry}})
			b.set(t, "projects/p/.kioku/views/v/view.json", view)
			b.set(t, "projects/p/.kioku/views/v/edit.json", legacyViewEdit{Version: "committed-edit", Title: "edited title", Prompt: "edited prompt"})
			b.set(t, "projects/p/.kioku/views/v/publish.json", legacyViewPublication{Prior: prior, Next: next, PriorHash: priorHash, NextHash: nextHash})
			b.values["projects/p/.kioku/views/v/bundle.zip"] = []byte("interrupted replacement")
			b.values["projects/p/.kioku/views/v/candidate.zip"] = nextBody
			b.values["projects/p/.kioku/views/v/previous.zip"] = priorBody
			if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e != nil {
				t.Fatal(e)
			}
			d, e := OpenDatabase(path, false)
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			if e = d.Read(t.Context(), func(q *Queries) error {
				v, e := q.View("p", "v")
				if e != nil {
					return e
				}
				if ValueString(v.Data.Title) != "edited title" || ValueString(v.Data.Prompt) != "edited prompt" || v.CreatedAt == nil || *v.CreatedAt != 100123 {
					t.Fatal(v)
				}
				switch which {
				case "next preserves latest failure":
					if ValueString(v.Data.Status) != "failed" || ValueString(v.Data.Error) != "latest failure" || ValueString(v.BundleHash) != nextHash {
						t.Fatal(v)
					}
				case "prior restores result":
					if ValueString(v.Data.Status) != "completed" || v.Data.Error != nil || ValueString(v.Data.JobID) != "old-job" || ValueString(v.BundleHash) != priorHash {
						t.Fatal(v)
					}
				case "stale epoch clears references":
					if v.BundleHash != nil || v.Data.JobID != nil || v.Data.WikiSnapshotID != nil || v.Data.GeneratedAt != nil || ValueString(v.Data.Status) != "failed" || ValueString(v.Data.Error) != "latest failure" {
						t.Fatal(v)
					}
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			for key := range b.values {
				if obsoleteV2Object(strings.TrimPrefix(string(key), "projects/p/")) {
					t.Fatal("legacy object retained", key)
				}
			}
		})
	}
}
func TestMigrationViewInitializationDistinguishesAbsentAndEmptyIndex(t *testing.T) {
	for _, which := range []string{"absent", "empty", "residual"} {
		t.Run(which, func(t *testing.T) {
			path, b, c, _ := migrationFixture(t)
			if which == "empty" {
				b.set(t, "projects/p/.kioku/views/index.json", legacyViewIndex{Views: map[string]legacyViewEntry{}})
			}
			if which == "residual" {
				b.values["projects/p/.kioku/views/v/candidate.zip"] = []byte("residual")
			}
			e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000))
			if which == "residual" {
				if e == nil || b.deletes != 0 || b.puts != 0 {
					t.Fatal(e, b.deletes, b.puts)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			d, e := OpenDatabase(path, false)
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			if e = d.Read(t.Context(), func(q *Queries) error {
				v, e := q.ProjectViews("p")
				want := 0 // E8: no Wiki row until its first PATCH.
				if which == "absent" {
					want = 1
				}
				if len(v) != want {
					t.Fatal(v)
				}
				return e
			}); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestMigrationRejectsBrokenViewEditBeforeMutation(t *testing.T) {
	path, b, c, _ := migrationFixture(t)
	b.set(t, "projects/p/.kioku/views/index.json", legacyViewIndex{Views: map[string]legacyViewEntry{"map": {ID: "v", Version: "committed"}}})
	b.set(t, "projects/p/.kioku/views/v/view.json", legacyViewDefinition{ID: "v", Slug: "map", Title: "Map", Prompt: "Show", legacyViewResult: legacyViewResult{Status: "pending"}})
	b.values["projects/p/.kioku/views/v/edit.json"] = []byte(`{}`)
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e == nil {
		t.Fatal("accepted malformed edit")
	}
	if b.puts != 0 || b.deletes != 0 {
		t.Fatal("mutated before validation")
	}
}
