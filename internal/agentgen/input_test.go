package agentgen

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBuildInputWikiPrompt(t *testing.T) {
	for _, prompt := range []*string{nil, new(string), new(" \n決定を分ける。\n{{.Namespace}}\n")} {
		var archive bytes.Buffer
		if err := BuildInput(InputSpec{WikiPrompt: prompt}, &archive); err != nil {
			t.Fatal(err)
		}
		files := unpack(t, archive.Bytes())
		body, exists := files["WIKI.md"]
		if exists != (prompt != nil) || prompt != nil && body != *prompt {
			t.Fatalf("WIKI.md = %q, exists = %v", body, exists)
		}
		if _, exists := files["VIEW.md"]; exists {
			t.Fatal("Wiki input contains VIEW.md")
		}
	}
}

func TestBuildInputWikiPromptExpandedLimit(t *testing.T) {
	spec := InputSpec{WikiPrompt: new(strings.Repeat("x", 32<<10))}
	var archive bytes.Buffer
	if err := BuildInput(spec, &archive); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	size, err := io.Copy(io.Discard, gz)
	if err != nil {
		t.Fatal(err)
	}
	if int64(archive.Len()) >= size-1 {
		t.Fatal("fixture must compress below the expanded limit")
	}
	if err := buildInput(spec, io.Discard, size); err != nil {
		t.Fatal(err)
	}
	if err := buildInput(spec, io.Discard, size-1); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("limit did not include WIKI.md: %v", err)
	}
	spec.WikiPrompt = nil
	if err := buildInput(spec, io.Discard, size-1); err != nil {
		t.Fatalf("input without WIKI.md should fit: %v", err)
	}
}
