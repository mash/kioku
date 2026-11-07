package agentgen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/wiki"
)

func TestIncrementalAcceptsPublishedLongName(t *testing.T) {
	pages := map[string]string{"wiki/index.md": "# Index", "wiki/" + strings.Repeat("a", 101) + ".md": "# Topic"}
	if err := wiki.ValidatePages(pages); err != nil {
		t.Fatal(err)
	}
	var out, again bytes.Buffer
	spec := InputSpec{Wiki: pages}
	if err := BuildInput(spec, &out); err != nil {
		t.Fatal(err)
	}
	if err := BuildInput(spec, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Error("long-name archive is not deterministic")
	}
	gz, err := gzip.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = string(b)
	}
	for name, want := range pages {
		if found[name] != want {
			t.Errorf("lost page %s", name)
		}
	}
}
