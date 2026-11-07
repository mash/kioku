package agentgen

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mash/kioku/internal/wiki"
)

var ErrOutputTooLarge = errors.New("agent output too large")

type OutputError struct{ Problems []string }

func (e *OutputError) Error() string { return "invalid output: " + strings.Join(e.Problems, "; ") }

type OutputLimits struct {
	MaxCompressed, MaxUncompressed int64
}

func DefaultOutputLimits() OutputLimits { return OutputLimits{128 << 20, 128 << 20} }

var outputName = regexp.MustCompile(`^wiki/[a-z0-9][a-z0-9-]*\.md$`)

func ReadOutput(r io.Reader, lim OutputLimits) (map[string]string, error) {
	if lim.MaxCompressed <= 0 || lim.MaxUncompressed <= 0 {
		return nil, &OutputError{[]string{"invalid output limits"}}
	}
	compressed := &io.LimitedReader{R: r, N: lim.MaxCompressed + 1}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		if compressed.N == 0 {
			return nil, ErrOutputTooLarge
		}
		return nil, &OutputError{[]string{"archive: " + err.Error()}}
	}
	defer gz.Close()
	expanded := &io.LimitedReader{R: gz, N: lim.MaxUncompressed + 1}
	tr := tar.NewReader(expanded)
	invalid := func(name, reason string) (map[string]string, error) {
		return nil, &OutputError{[]string{name + ": " + reason}}
	}
	readError := func(err error) (map[string]string, error) {
		if compressed.N == 0 || expanded.N == 0 {
			return nil, ErrOutputTooLarge
		}
		return invalid("archive", err.Error())
	}
	pages := map[string]string{}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return readError(err)
		}
		name := h.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, "\\") {
			return invalid(name, "path escapes wiki/")
		}
		if seen[name] {
			return invalid(name, "duplicate entry")
		}
		seen[name] = true
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return invalid(name, "only regular files are allowed")
		}
		if h.Size > expanded.N-1 {
			return nil, ErrOutputTooLarge
		}
		if !outputName.MatchString(name) {
			return invalid(name, "invalid wiki filename")
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return readError(err)
		}
		if !utf8.Valid(b) {
			return invalid(name, "body is not UTF-8")
		}
		pages[name] = string(b)
	}
	// Consume tar padding and all gzip members, verifying CRCs and both limits.
	if _, err = io.Copy(io.Discard, expanded); err != nil {
		return readError(err)
	}
	if compressed.N == 0 || expanded.N == 0 {
		return nil, ErrOutputTooLarge
	}
	if _, err = io.Copy(io.Discard, compressed); err != nil {
		return readError(err)
	}
	if compressed.N == 0 {
		return nil, ErrOutputTooLarge
	}
	if err = wiki.ValidatePages(pages); err != nil {
		return invalid("pages", err.Error())
	}
	return pages, nil
}

// ReadAcceptedOutput reads only archives already validated and matched by hash.
func ReadAcceptedOutput(r io.Reader) (map[string]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	pages := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return pages, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		pages[h.Name] = string(b)
	}
}
