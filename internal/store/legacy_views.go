package store

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

type legacyViewIndex struct {
	Views   map[string]legacyViewEntry `json:"views"`
	Removed []string                   `json:"removed"`
}
type legacyViewEntry struct {
	ID      string `json:"id"`
	Version string `json:"settings_generation"`
	Result  string `json:"result_job_id"`
	Hash    string `json:"bundle_hash"`
	Epoch   string `json:"view_epoch"`
}
type legacyViewResult struct {
	Status         string     `json:"status"`
	Error          string     `json:"error"`
	JobID          string     `json:"job_id"`
	WikiSnapshotID string     `json:"wiki_snapshot_id"`
	GeneratedAt    *time.Time `json:"generated_at"`
}
type legacyViewDefinition struct {
	ID        string     `json:"view_id"`
	Slug      string     `json:"slug"`
	Title     string     `json:"title"`
	Prompt    string     `json:"prompt"`
	CreatedAt *time.Time `json:"created_at"`
	legacyViewResult
}
type legacyViewEdit struct {
	Version string `json:"settings_generation"`
	Title   string `json:"title"`
	Prompt  string `json:"prompt"`
}
type legacyViewPublication struct {
	Prior     legacyViewResult `json:"prior"`
	Next      legacyViewResult `json:"next"`
	PriorHash string           `json:"prior_hash"`
	NextHash  string           `json:"next_hash"`
}

func (p *migrationPlan) readViews(ctx context.Context, project ProjectRow, objects blob.Store, keys []blob.Key, now time.Time) error {
	prefix := "projects/" + project.ID + "/.kioku/views/"
	existing := map[string][]byte{}
	for _, key := range keys {
		name, ok := strings.CutPrefix(string(key), prefix)
		if !ok {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		body, e := objects.Get(ctx, key)
		if e != nil {
			return e
		}
		if e = decodeLegacyJSON(body, &map[string]json.RawMessage{}); e != nil {
			return e
		}
		existing[name] = body
		// Even an uncommitted/removed View's JSON must be readable before deletion.
		switch path.Base(name) {
		case "edit.json":
			var v legacyViewEdit
			if e = decodeLegacyJSON(body, &v); e != nil {
				return e
			}
			if v.Version == "" || strings.TrimSpace(v.Title) == "" || strings.TrimSpace(v.Prompt) == "" {
				return errors.New("invalid legacy View edit")
			}
		case "publish.json":
			var v legacyViewPublication
			if e = decodeLegacyJSON(body, &v); e != nil {
				return e
			}
			if v.Prior.Status == "" || v.Next.Status == "" {
				return errors.New("invalid legacy View publication")
			}
		case "view.json":
			var v legacyViewDefinition
			if e = decodeLegacyJSON(body, &v); e != nil {
				return e
			}
			if !validID(v.ID) || !validID(v.Slug) || (v.Slug != "wiki" && (strings.TrimSpace(v.Title) == "" || strings.TrimSpace(v.Prompt) == "" || v.Status == "")) {
				return errors.New("invalid legacy View metadata")
			}
		}
	}
	wikiID := "vw_" + auth.Hash(project.ID + "/wiki")[:20]
	wikiRow := ViewRow{ProjectID: project.ID, ID: wikiID, Slug: "wiki"}
	raw, exists := existing["index.json"]
	if !exists {
		for _, key := range keys {
			if strings.HasPrefix(string(key), prefix) {
				return errors.New("legacy View objects exist without index")
			}
		}
		created := now.UnixMilli()
		p.views = append(p.views, ViewRow{ProjectID: project.ID, ID: "vw_" + auth.Hash(project.ID + "/project-map")[:20], Slug: "project-map", CreatedAt: &created, Data: ViewData{Title: migrationString("Project map"), Prompt: migrationString("Wiki をもとに、プロジェクトの現在地、日程、次のマイルストーン、直近の判断、根拠を示す。"), Status: migrationString("pending")}})
		return nil
	}
	var index legacyViewIndex
	if e := decodeLegacyJSON(raw, &index); e != nil {
		return e
	}
	for _, slug := range slices.Sorted(maps.Keys(index.Views)) {
		entry := index.Views[slug]
		if !validID(entry.ID) || !validID(slug) {
			return errors.New("invalid legacy View identity")
		}
		raw, ok := existing[entry.ID+"/view.json"]
		if !ok {
			return errors.New("referenced legacy View metadata missing")
		}
		var v legacyViewDefinition
		if e := decodeLegacyJSON(raw, &v); e != nil {
			return e
		}
		if v.ID != entry.ID || v.Slug != slug {
			return errors.New("legacy View metadata identity mismatch")
		}
		if slug == "wiki" {
			if entry.ID != wikiID {
				return errors.New("legacy Wiki View identity mismatch")
			}
			wikiRow.Data.Title = migrationString(v.Title)
			wikiRow.Data.Prompt = migrationString(v.Prompt)
			continue
		}
		if raw, ok := existing[entry.ID+"/edit.json"]; ok {
			var edit legacyViewEdit
			if e := decodeLegacyJSON(raw, &edit); e != nil {
				return e
			}
			if edit.Version == entry.Version {
				v.Title, v.Prompt = edit.Title, edit.Prompt
			}
		}
		if raw, ok := existing[entry.ID+"/publish.json"]; ok {
			var publication legacyViewPublication
			if e := decodeLegacyJSON(raw, &publication); e != nil {
				return e
			}
			var selected *legacyViewResult
			if publication.Next.JobID == entry.Result && publication.NextHash == entry.Hash {
				selected = &publication.Next
			} else if publication.Prior.JobID == entry.Result && publication.PriorHash == entry.Hash {
				selected = &publication.Prior
			}
			if selected == nil {
				return errors.New("legacy View publication disagrees with index")
			}
			if v.JobID != selected.JobID {
				v.legacyViewResult = *selected
			}
		}
		if v.JobID != entry.Result {
			return errors.New("legacy View result disagrees with index")
		}
		title, prompt, status := v.Title, v.Prompt, v.Status
		row := ViewRow{ProjectID: project.ID, ID: entry.ID, Slug: slug, CreatedAt: migrationOptionalTime(v.CreatedAt), BundleHash: migrationString(entry.Hash), ViewEpoch: migrationString(entry.Epoch), Data: ViewData{Title: &title, Prompt: &prompt, Status: &status, Error: migrationString(v.Error), JobID: migrationString(v.JobID), WikiSnapshotID: migrationString(v.WikiSnapshotID), GeneratedAt: migrationOptionalTime(v.GeneratedAt)}}
		if entry.Result != "" {
			if _, e := model.ParseContentHash(entry.Hash); e != nil {
				return e
			}
			var selected []byte
			for _, name := range []string{"bundle.zip", "candidate.zip", "previous.zip"} {
				body, e := objects.Get(ctx, blob.Key(prefix+entry.ID+"/"+name))
				if missingObject(e) {
					continue
				}
				if e != nil {
					return e
				}
				if string(model.HashContent(body)) == entry.Hash {
					selected = body
					break
				}
			}
			if selected == nil {
				return errors.New("legacy View has no matching ZIP")
			}
			if e := validateLegacyViewZIP(selected); e != nil {
				return e
			}
			if entry.Epoch == valueMigrationString(project.ViewEpoch) {
				p.copies[model.ViewContentKey(project.ID, entry.ID, model.ContentHash(entry.Hash))] = selected
			}
		} else if entry.Hash != "" {
			return errors.New("legacy View hash without result")
		}
		if entry.Epoch != valueMigrationString(project.ViewEpoch) {
			row.BundleHash = nil
			row.ViewEpoch = nil
			row.Data.JobID = nil
			row.Data.WikiSnapshotID = nil
			row.Data.GeneratedAt = nil
		}
		p.views = append(p.views, row)
	}
	if _, exists := index.Views["wiki"]; exists {
		p.views = append(p.views, wikiRow)
	}
	return nil
}
func valueMigrationString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func validateLegacyViewZIP(body []byte) error {
	if len(body) > 16<<20 {
		return errors.New("legacy View ZIP exceeds size limit")
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return errors.New("invalid legacy View ZIP")
	}
	if len(reader.File) > 128 {
		return errors.New("legacy View ZIP exceeds file limit")
	}
	names := map[string]bool{}
	var total int64
	for _, entry := range reader.File {
		name := entry.Name
		if !blob.ValidKey(blob.Key(name)) || names[name] || entry.FileInfo().IsDir() {
			return errors.New("invalid legacy View ZIP entry")
		}
		names[name] = true
		r, e := entry.Open()
		if e != nil {
			return e
		}
		n, copyErr := io.Copy(io.Discard, io.LimitReader(r, (8<<20)-total+1))
		closeErr := r.Close()
		if e = errors.Join(copyErr, closeErr); e != nil {
			return e
		}
		total += n
		if total > 8<<20 {
			return errors.New("legacy View ZIP exceeds expanded size limit")
		}
	}
	if !names["index.html"] {
		return errors.New("legacy View ZIP missing index.html")
	}
	return nil
}
