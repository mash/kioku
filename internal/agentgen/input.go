package agentgen

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

const MaxInputBytes int64 = 256 << 20

var ErrInputTooLarge = errors.New("agent input exceeds 256 MiB")

type InboxText struct {
	Item model.InboxItem
	Text string
}
type InputSpec struct {
	JobID, ProjectID, InputURL, OutputURL string
	Inbox                                 []InboxText
	Wiki                                  map[string]string
}

func AgentPage(name string) bool { return wiki.ValidFile(name) && strings.HasPrefix(name, "wiki/") }

var inboxID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func BuildInput(spec InputSpec, w io.Writer) error { return buildInput(spec, w, MaxInputBytes) }
func buildInput(spec InputSpec, w io.Writer, limit int64) error {
	files := map[string]string{"JOB.md": FirstMessage(spec)}
	for _, in := range spec.Inbox {
		if !inboxID.MatchString(in.Item.ID) {
			return errors.New("invalid inbox ID")
		}
		name := "inbox/" + in.Item.ID + ".md"
		if _, exists := files[name]; exists {
			return errors.New("duplicate inbox ID")
		}
		var updatedAt string
		if in.Item.Status == "fetched" && in.Item.UpdatedAt != nil {
			// yaml omitempty treats *time.Time pointing to zero as absent.
			updatedAt = in.Item.UpdatedAt.Format(time.RFC3339Nano)
		}
		metadata, err := yaml.Marshal(struct {
			SourceURL string `yaml:"source_url"`
			UpdatedAt string `yaml:"updated_at,omitempty"`
			Status    string `yaml:"status"`
			Submitter string `yaml:"submitter"`
		}{in.Item.SourceURL, updatedAt, in.Item.Status, in.Item.Submitter})
		if err != nil {
			return err
		}
		files[name] = "---\n" + string(metadata) + "---\n" + in.Text
	}
	for name, body := range spec.Wiki {
		if AgentPage(name) {
			files[name] = body
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
