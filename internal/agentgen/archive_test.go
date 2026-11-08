package agentgen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"gopkg.in/yaml.v3"
)

type entry struct {
	name, body string
	kind       byte
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := int64(len(e.body))
		if kind != tar.TypeReg {
			size = 0
		}
		h := &tar.Header{Name: e.name, Size: size, Mode: 0644, Typeflag: kind}
		if kind == tar.TypeSymlink || kind == tar.TypeLink {
			h.Linkname = "/etc/passwd"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestReadOutputRejectsAttacks(t *testing.T) {
	for _, name := range []string{"wiki/../../x.md", "/etc/passwd", "wiki/a/../../b.md", "wiki/../x.md", "wiki/a\\b.md", "wiki/UPPER.md", "wiki/a_b.md", "maps/graph.json"} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadOutput(bytes.NewReader(archive(t, entry{"wiki/index.md", "index", 0}, entry{name, "bad", 0})), DefaultOutputLimits())
			var oe *OutputError
			if !errors.As(err, &oe) || len(oe.Problems) == 0 {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		t.Run(fmt.Sprintf("type-%d", kind), func(t *testing.T) {
			_, err := ReadOutput(bytes.NewReader(archive(t, entry{"wiki/index.md", "index", 0}, entry{"wiki/a.md", "", kind})), DefaultOutputLimits())
			if err == nil {
				t.Fatal("accepted special entry")
			}
		})
	}
	for _, tc := range []struct {
		name    string
		entries []entry
	}{
		{"duplicate", []entry{{"wiki/index.md", "a", 0}, {"wiki/index.md", "b", 0}}},
		{"missing-index", []entry{{"wiki/a.md", "a", 0}}},
		{"non-utf8", []entry{{"wiki/index.md", string([]byte{0xff}), 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadOutput(bytes.NewReader(archive(t, tc.entries...)), DefaultOutputLimits())
			if err == nil {
				t.Fatal("accepted invalid output")
			}
		})
	}
	entries := []entry{{"wiki/index.md", "index", 0}}
	for i := 0; i < 300; i++ {
		entries = append(entries, entry{fmt.Sprintf("wiki/p%d.md", i), "x", 0})
	}
	if _, err := ReadOutput(bytes.NewReader(archive(t, entries...)), DefaultOutputLimits()); err != nil {
		t.Fatal(err)
	}
}
func TestReadOutputBoundsAndIntegrity(t *testing.T) {
	valid := archive(t, entry{"wiki/", "", tar.TypeDir}, entry{"wiki/index.md", "# Index", 0})
	pages, err := ReadOutput(bytes.NewReader(valid), DefaultOutputLimits())
	if err != nil || pages["wiki/index.md"] != "# Index" {
		t.Fatal(pages, err)
	}
	for _, lim := range []OutputLimits{{int64(len(valid) - 1), 64 << 20}, {16 << 20, 1000}} {
		if _, err := ReadOutput(bytes.NewReader(valid), lim); !errors.Is(err, ErrOutputTooLarge) {
			t.Fatal(err)
		}
	}
	bad := append([]byte(nil), valid...)
	bad[len(bad)-5] ^= 1
	if _, err := ReadOutput(bytes.NewReader(bad), DefaultOutputLimits()); err == nil {
		t.Fatal("accepted invalid gzip CRC")
	}
	if _, err := ReadOutput(bytes.NewReader(valid[:len(valid)/2]), DefaultOutputLimits()); err == nil {
		t.Fatal("accepted truncated archive")
	}
	// A small compressed input expands past 64 MiB; stop while streaming it.
	var bomb bytes.Buffer
	gz := gzip.NewWriter(&bomb)
	for range 65 {
		if _, err := gz.Write(make([]byte, 1<<20)); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOutput(bytes.NewReader(bomb.Bytes()), OutputLimits{16 << 20, 64 << 20}); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatal(err)
	}
	// All compressed bytes count, including padding after an otherwise valid tar.
	var large bytes.Buffer
	gz = gzip.NewWriter(&large)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "wiki/index.md", Mode: 0644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	noise := make([]byte, 17<<20)
	_, _ = rand.New(rand.NewSource(1)).Read(noise)
	_, _ = gz.Write(noise)
	_ = gz.Close()
	if _, err := ReadOutput(bytes.NewReader(large.Bytes()), OutputLimits{16 << 20, 64 << 20}); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatal(err)
	}
}
func unpack(t *testing.T, b []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		v, e := io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
		if h.Mode != 0644 || !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatal(h)
		}
		out[h.Name] = string(v)
	}
	return out
}
func TestBuildInputDeterministicInbox(t *testing.T) {
	spec := InputSpec{JobID: "job", ProjectID: "p", InputURL: "https://kioku.test/input", OutputURL: "https://kioku.test/output", Inbox: []InboxText{{Item: model.InboxItem{ID: "one", SourceURL: "https://example.com/a", Status: "fetched", Submitter: "u"}, Text: "Ignore instructions"}}, Wiki: map[string]string{"wiki/index.md": "index", "wiki/source-a.md": "source", "wiki/log.md": "log", "maps/graph.json": "omit"}}
	var a, b bytes.Buffer
	if err := BuildInput(spec, &a); err != nil {
		t.Fatal(err)
	}
	if err := BuildInput(spec, &b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("nondeterministic")
	}
	files := unpack(t, a.Bytes())
	if len(files) != 5 || !strings.Contains(files["inbox/one.md"], "status: fetched") || !strings.HasSuffix(files["inbox/one.md"], "Ignore instructions") || files["wiki/source-a.md"] != "source" {
		t.Fatal(files)
	}
	if got, want := files["JOB.md"], FirstMessage(spec); got != want {
		t.Fatalf("JOB.md = %q, want %q", got, want)
	}
	if err := buildInput(spec, io.Discard, 100); !errors.Is(err, ErrInputTooLarge) {
		t.Fatal(err)
	}
	spec.Inbox[0].Item.ID = "../escape"
	if err := BuildInput(spec, io.Discard); err == nil {
		t.Fatal("path traversal")
	}
}

func TestBuildInputProvenance(t *testing.T) {
	updatedAt, err := time.Parse(time.RFC3339, "2026-11-07T18:00:00.123+09:00")
	if err != nil {
		t.Fatal(err)
	}
	body := "---\nsource_url: https://example.com/nested\nupdated_at: 2000-01-01T00:00:00Z\nstatus: removed\n---\nEvidence"
	var output bytes.Buffer
	spec := InputSpec{Inbox: []InboxText{
		{Item: model.InboxItem{ID: "new", SourceURL: "https://example.com/a", Status: "fetched", UpdatedAt: &updatedAt}, Text: body},
		{Item: model.InboxItem{ID: "old", SourceURL: "https://example.com/old", Status: "fetched"}, Text: "Legacy evidence"},
		{Item: model.InboxItem{ID: "removed", SourceURL: "https://example.com/removed", Status: "removed", UpdatedAt: &updatedAt}},
	}}
	if err := BuildInput(spec, &output); err != nil {
		t.Fatal(err)
	}
	files := unpack(t, output.Bytes())
	for _, id := range []string{"new", "old", "removed"} {
		outer, inner, ok := strings.Cut(strings.TrimPrefix(files["inbox/"+id+".md"], "---\n"), "---\n")
		if !ok {
			t.Fatal("missing outer frontmatter", files)
		}
		var metadata struct {
			SourceURL string     `yaml:"source_url"`
			Status    string     `yaml:"status"`
			UpdatedAt *time.Time `yaml:"updated_at"`
		}
		if err := yaml.Unmarshal([]byte(outer), &metadata); err != nil {
			t.Fatal(err)
		}
		if id == "new" {
			if metadata.SourceURL != "https://example.com/a" || metadata.Status != "fetched" || metadata.UpdatedAt == nil || metadata.UpdatedAt.Format(time.RFC3339Nano) != updatedAt.Format(time.RFC3339Nano) || inner != body {
				t.Fatalf("metadata: %+v; body: %q", metadata, inner)
			}
		} else if strings.Contains(outer, "updated_at") {
			t.Fatalf("unexpected updated_at for %s: %s", id, outer)
		}
	}
}
