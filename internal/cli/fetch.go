package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/source"
	"golang.org/x/sys/unix"
)

const maxFetchBytes = 20 << 20

func fetchCommand(ctx context.Context, args []string, outputs ...io.Writer) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("fetch: %w", err)
		}
	}()
	if len(args) == 0 {
		return errors.New("usage: kioku capture fetch URL --capture-token TOKEN --dir DIR --out FILE")
	}
	f := flag.NewFlagSet("fetch", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	out := f.String("out", "", "output file inside capture directory")
	dir := f.String("dir", "", "capture directory")
	token := captureTokenFlag(f)
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *out == "" || *dir == "" {
		return errors.New("--out と capture の一時ディレクトリを指定する必要がある")
	}
	if *token != filepath.Base(*dir) {
		return errors.New("invalid capture token")
	}
	return fetchURL(ctx, nil, args[0], *dir, *out, outputs...)
}

func fetchURL(ctx context.Context, transport http.RoundTripper, rawURL, dir, output string, outputs ...io.Writer) error {
	dir, rows, err := captureSnapshot(filepath.Base(dir), dir)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rows, func(row queueRow) bool { return row.Source == rawURL }) {
		return errors.New("URL は capture のスナップショットにない")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	// Only the default HTTP transport is used for live fetches.
	if transport == nil {
		if err := source.ValidateURL(u); err != nil {
			return err
		}
	}
	if filepath.IsAbs(output) {
		output, err = filepath.Rel(dir, output)
		if err != nil {
			return err
		}
	}
	if !filepath.IsLocal(output) || output == "." {
		return errors.New("出力先は capture の一時ディレクトリ内に限る")
	}
	// Open every directory with O_NOFOLLOW; O_EXCL also rejects existing output links and files.
	parent, err := openFetchDirectory(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	parts := strings.Split(output, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		if part == "." || part == ".." || part == "" {
			return errors.New("出力先のパスが不正")
		}
		fd, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		parent.Close()
		parent = os.NewFile(uintptr(fd), part)
		defer parent.Close()
	}
	validate := func(u *url.URL) error {
		if u.Scheme != "https" || u.Host == "" || u.User != nil {
			return errors.New("認証情報を含まない HTTPS URL のみ取得できる")
		}
		return nil
	}
	if err := validate(u); err != nil {
		return err
	}
	u = captureFetchURL(u)
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("リダイレクト回数の上限を超えた")
		}
		if req.URL.Host != u.Host {
			return errors.New("別のホストへの転送は許可されていない")
		}
		return validate(req.URL)
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP ステータス %d", response.StatusCode)
	}
	if response.ContentLength > maxFetchBytes {
		return errors.New("20 MiB の上限を超えた")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxFetchBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxFetchBytes {
		return errors.New("20 MiB の上限を超えた")
	}
	name := parts[len(parts)-1]
	if filepath.Ext(name) == ".raw" {
		name = strings.TrimSuffix(name, ".raw") + captureFetchExtension(rawURL, response.Header.Get("Content-Type"))
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	_, err = file.Write(data)
	err = errors.Join(err, file.Close())
	if err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	if err == nil && len(outputs) > 0 {
		_, err = fmt.Fprintf(outputs[0], "%s\nfetched_at: %s\n", filepath.Join(dir, filepath.Dir(output), name), time.Now().UTC().Format(time.RFC3339))
		if modified, parseErr := http.ParseTime(response.Header.Get("Last-Modified")); err == nil && parseErr == nil {
			_, err = fmt.Fprintf(outputs[0], "last_modified: %s\n", modified.UTC().Format(time.RFC3339))
		}
	}
	return err
}

func captureFetchURL(original *url.URL) *url.URL {
	copyURL := *original
	u := &copyURL
	if u.Host == "github.com" {
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 5)
		if len(parts) == 5 && parts[0] != "" && parts[1] != "" && parts[2] == "blob" && parts[3] != "" && parts[4] != "" {
			u.Host = "raw.githubusercontent.com"
			u.Path = "/" + strings.Join([]string{parts[0], parts[1], parts[3], parts[4]}, "/")
			u.RawPath, u.RawQuery, u.Fragment = "", "", ""
		}
	}
	return u
}

// captureFetchExtension prefers the source URL over response metadata.
func captureFetchExtension(rawURL, contentType string) string {
	u, _ := url.Parse(rawURL)
	if u != nil {
		ext := strings.ToLower(filepath.Ext(u.Path))
		switch ext {
		case ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".md", ".txt", ".html":
			return ext
		}
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch mediaType {
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "text/markdown":
		return ".md"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	}
	return ".raw"
}

func openFetchDirectory(dir string) (*os.File, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return nil, errors.New("invalid capture directory")
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// captureSourcePath confines put input independently of the agent's Read permission.
func captureSourcePath(dir, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(dir, absolute)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errors.New("capture put source must be inside the run directory")
	}
	return absolute, nil
}
func readCaptureSource(dir, path string) ([]byte, error) {
	return readCaptureFile(dir, path, maxSourceFileBytes)
}

func readCaptureFile(dir, path string, limit int64) ([]byte, error) {
	absolute, err := captureSourcePath(dir, path)
	if err != nil {
		return nil, err
	}
	parent, err := openFetchDirectory(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	data, _, err := readRegularAt(int(parent.Fd()), filepath.Base(absolute), limit)
	return data, err
}
