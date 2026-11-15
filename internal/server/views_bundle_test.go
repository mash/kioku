package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"maps"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func viewZIPFixture(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func viewTarFixture(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	w := tar.NewWriter(gz)
	if err := w.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := w.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestViewBundleRoundTripAndRendering(t *testing.T) {
	var raster bytes.Buffer
	if err := png.Encode(&raster, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	entry := []byte(`<!doctype html><html lang="ja"><head><meta charset="utf-8">
<link rel="stylesheet" href="./assets/styles/theme.css">
<style>.tag { border-color: rgb(0, 80, 55) }</style></head>
<body><details><summary>ステータス</summary><img src="assets/flow.png" alt="図">
<a href="wiki/index.md">Wiki の出典</a> <a href="#status">状態へ</a>
<p id="status" style="color: #163e30">進行中 &amp; 次の予定</p></details></body></html>`)
	files := map[string][]byte{
		"index.html":      entry,
		"summary.md":      []byte("# 進行中"),
		"data.json":       []byte(`{"status":"active"}`),
		"assets/flow.png": raster.Bytes(),
		"assets/styles/theme.css": []byte(`@keyframes fade { from { opacity: 0 } to { opacity: 1 } }
.tag { animation: fade 1s; background-image: url("../flow.png") }`),
	}
	output, err := readViewRunnerOutput(viewTarFixture(t, files))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := output.Archive()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := readViewBundle(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bundle.Citations(), []viewCitation{{Path: "wiki/index.md", Label: "Wiki の出典"}}) {
		t.Fatalf("unexpected citations: %#v", bundle.Citations())
	}
	for _, name := range []string{"index.html", "summary.md", "data.json"} {
		if got, err := readPublishedViewText(stored, name); err != nil || got != string(files[name]) {
			t.Fatalf("text entry %s: value=%q err=%v", name, got, err)
		}
	}
	if _, err := readPublishedViewText(stored, "assets/flow.png"); err == nil {
		t.Fatal("binary file readable through text path")
	}
	document := bundle.Document()
	if !strings.Contains(document, `http-equiv="Content-Security-Policy"`) || !strings.Contains(document, "@keyframes fade") ||
		!strings.Contains(document, "<details>") || strings.Count(document, "data:image/png;base64,") != 2 ||
		strings.Contains(document, `href="wiki/`) || strings.Contains(document, `src="assets/`) {
		t.Fatalf("incorrectly transformed generated content: %s", document)
	}
	var parent bytes.Buffer
	frame := template.Must(template.New("frame").Parse(`<iframe sandbox srcdoc="{{.}}"></iframe>`))
	if err := frame.Execute(&parent, document); err != nil {
		t.Fatal(err)
	}
	framed, err := html.Parse(&parent)
	if err != nil {
		t.Fatal(err)
	}
	for node := range framed.Descendants() {
		if node.Data == "iframe" && viewAttr(node, "srcdoc") == document {
			return
		}
	}
	t.Fatal("templating altered the frame document")
}

func TestViewBundleRejectsInvalidEntriesAndReferences(t *testing.T) {
	for _, c := range []struct {
		name, html string
		file, body string
		expected   error
	}{
		{"executable element", `<script></script>`, "", "", errViewHTML},
		{"metadata redirect", `<meta http-equiv="refresh" content="0">`, "", "", errViewHTML},
		{"external image", `<img src="https://example.invalid/logo.png">`, "", "", errViewAsset},
		{"external hyperlink", `<a href="https://example.invalid">External</a>`, "", "", errViewAsset},
		{"unsupported element", `<form>Form</form>`, "", "", errViewHTML},
		{"inline event attribute", `<div onclick="">Text</div>`, "", "", errViewHTML},
		{"remote CSS resource", `<style>p { background: url("https://example.invalid/a.png"); }</style>`, "", "", errViewAsset},
		{"imported CSS resource", `<style>@import "https://example.invalid/a.css";</style>`, "", "", errViewCSS},
		{"wrapped CSS function", `<p style="color: rgb(0, 0, 0">Text</p>`, "", "", errViewCSS},
		{"missing CSS file", `<link rel="stylesheet" href="assets/theme.css">`, "", "", errViewAsset},
		{"mismatched image bytes", `<h1>View</h1>`, "assets/flow.png", "not an image", errViewEntry},
		{"unreferenced extra JS", `<h1>View</h1>`, "assets/app.js", "", errViewEntry},
		{"invalid archive path", `<h1>View</h1>`, "assets/../flow.png", "", errViewEntry},
		{"invalid JSON", `<h1>View</h1>`, "data.json", "{", errViewEntry},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string][]byte{"index.html": []byte(c.html)}
			if c.file != "" {
				files[c.file] = []byte(c.body)
			}
			if _, err := readViewBundle(viewZIPFixture(t, files)); !errors.Is(err, c.expected) {
				t.Fatalf("got %v, want %v", err, c.expected)
			}
			if _, err := readViewRunnerOutput(viewTarFixture(t, files)); !errors.Is(err, c.expected) {
				t.Fatalf("runner: got %v, want %v", err, c.expected)
			}
		})
	}
}

func TestViewBundleBounds(t *testing.T) {
	t.Run("compressed size", func(t *testing.T) {
		oversized := make([]byte, maxViewArchiveBytes+1)
		if _, err := readViewBundle(oversized); !errors.Is(err, errViewLimit) {
			t.Fatalf("ZIP: got %v, want %v", err, errViewLimit)
		}
		if _, err := readViewRunnerOutput(oversized); !errors.Is(err, errViewLimit) {
			t.Fatalf("tar.gz: got %v, want %v", err, errViewLimit)
		}
	})
	for _, name := range []string{"file count", "expanded size", "file size"} {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{"index.html": []byte("<p>View</p>")}
			switch name {
			case "file count":
				for i := range maxViewArchiveFiles {
					files[fmt.Sprintf("assets/%d.css", i)] = []byte("p { display: block }")
				}
			case "expanded size":
				sheet := bytes.Repeat([]byte{' '}, maxViewCSSBytes)
				for i := range maxViewExpandedBytes / len(sheet) {
					files[fmt.Sprintf("assets/%d.css", i)] = sheet
				}
			case "file size":
				files["summary.md"] = bytes.Repeat([]byte{'a'}, maxViewTextBytes+1)
			}
			if _, err := readViewBundle(viewZIPFixture(t, files)); !errors.Is(err, errViewLimit) {
				t.Fatalf("ZIP: got %v, want %v", err, errViewLimit)
			}
			if _, err := readViewRunnerOutput(viewTarFixture(t, files)); !errors.Is(err, errViewLimit) {
				t.Fatalf("tar.gz: got %v, want %v", err, errViewLimit)
			}
		})
	}
}
