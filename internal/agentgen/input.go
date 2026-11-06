package agentgen

import (
	"archive/tar"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
	"github.com/mash/kioku/prompts"
)

const MaxInputBytes int64 = 256 << 20

var ErrInputTooLarge = errors.New("agent input exceeds 256 MiB")

type SourceText struct {
	Source model.Source
	Text   string
}
type UpdatedSource struct{ Previous, Current model.Source }
type Changes struct {
	Added   []model.Source
	Updated []UpdatedSource
	Removed []model.Source
}
type InputSpec struct {
	Mode                                                     model.GenerationMode
	JobID, ProjectID, InputURL, OutputURL, PublishedRevision string
	Sources, Previous                                        []SourceText
	Changes                                                  Changes
	Wiki                                                     map[string]string
}

func DiffChanges(published, active []model.Source) Changes {
	c := Changes{}
	oldIDs, newIDs := map[string]bool{}, map[string]bool{}
	oldURLs, newURLs := map[string]model.Source{}, map[string]model.Source{}
	for _, s := range published {
		oldIDs[s.ID] = true
		if s.URL != "" {
			oldURLs[s.URL] = s
		}
	}
	for _, s := range active {
		newIDs[s.ID] = true
		if s.URL != "" {
			newURLs[s.URL] = s
		}
	}
	for _, s := range active {
		if oldIDs[s.ID] {
			continue
		}
		if old, ok := oldURLs[s.URL]; ok {
			c.Updated = append(c.Updated, UpdatedSource{old, s})
		} else {
			c.Added = append(c.Added, s)
		}
	}
	for _, s := range published {
		if newIDs[s.ID] {
			continue
		}
		if _, ok := newURLs[s.URL]; !ok {
			c.Removed = append(c.Removed, s)
		}
	}
	sort.Slice(c.Added, func(i, j int) bool { return c.Added[i].ID < c.Added[j].ID })
	sort.Slice(c.Updated, func(i, j int) bool { return c.Updated[i].Current.ID < c.Updated[j].Current.ID })
	sort.Slice(c.Removed, func(i, j int) bool { return c.Removed[i].ID < c.Removed[j].ID })
	return c
}

// AgentPage excludes files managed by the service, including the intake view.
func AgentPage(name string) bool {
	return wiki.ValidFile(name) && strings.HasPrefix(name, "wiki/") && !strings.HasPrefix(name, "wiki/source-") && !strings.HasPrefix(name, "wiki/inbox")
}
func StripSnapshots(body string) string { return wiki.StripSnapshots(body) }

var sourceID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func BuildInput(spec InputSpec, w io.Writer) error {
	return buildInput(spec, w, MaxInputBytes)
}
func buildInput(spec InputSpec, w io.Writer, limit int64) error {
	if spec.Mode != model.ModeFull && spec.Mode != model.ModeIncremental {
		return errors.New("invalid generation mode")
	}
	files := map[string]string{
		"JOB.md":     fmt.Sprintf(prompts.Job, spec.Mode, spec.JobID, spec.ProjectID, spec.InputURL, spec.OutputURL, prompts.OutputRules),
		"CHANGES.md": changesMarkdown(spec),
	}
	ordered := append([]SourceText(nil), spec.Sources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Source.ID < ordered[j].Source.ID })
	var index strings.Builder
	tsv := csv.NewWriter(&index)
	tsv.Comma = '\t'
	_ = tsv.Write([]string{"id", "url", "title", "contributor", "created", "hash"})
	for _, in := range ordered {
		s := in.Source
		if !sourceID.MatchString(s.ID) || len(s.ID) > 100 {
			return errors.New("invalid source ID")
		}
		name := "sources/" + s.ID + ".txt"
		if _, exists := files[name]; exists {
			return errors.New("duplicate source ID")
		}
		files[name] = in.Text
		_ = tsv.Write([]string{s.ID, s.URL, s.Title, s.CreatedBy, s.Created.UTC().Format(time.RFC3339), s.ContentHash})
	}
	tsv.Flush()
	if err := tsv.Error(); err != nil {
		return err
	}
	files["sources/index.tsv"] = index.String()
	if spec.Mode == model.ModeIncremental {
		for _, in := range spec.Previous {
			if !sourceID.MatchString(in.Source.ID) || len(in.Source.ID) > 100 {
				return errors.New("invalid previous source ID")
			}
			files["previous/"+in.Source.ID+".txt"] = in.Text
		}
		for name, body := range spec.Wiki {
			if AgentPage(name) {
				files[name] = StripSnapshots(body)
			}
		}
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(&inputWriter{w: gz, remaining: limit})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

type inputWriter struct {
	w         io.Writer
	remaining int64
}

func (w *inputWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrInputTooLarge
	}
	n, e := w.w.Write(p)
	w.remaining -= int64(n)
	return n, e
}

func changesMarkdown(spec InputSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, prompts.ChangesHeader, spec.Mode, spec.PublishedRevision, len(spec.Changes.Added))
	for _, s := range spec.Changes.Added {
		fmt.Fprintf(&b, prompts.ChangesSource, s.ID, s.URL, s.Title)
	}
	fmt.Fprintf(&b, prompts.ChangesUpdatedHeader, len(spec.Changes.Updated))
	for _, s := range spec.Changes.Updated {
		fmt.Fprintf(&b, prompts.ChangesUpdatedSource, s.Current.ID, s.Previous.ID, s.Current.URL, s.Previous.ID, s.Current.ID)
	}
	fmt.Fprintf(&b, prompts.ChangesRemovedHeader, len(spec.Changes.Removed))
	for _, s := range spec.Changes.Removed {
		fmt.Fprintf(&b, prompts.ChangesSource, s.ID, s.URL, s.Title)
	}
	return b.String()
}
