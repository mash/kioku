package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const maxFetchBytes = 20 << 20

func fetchCommand(ctx context.Context, args []string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("fetch: %w", err)
		}
	}()
	if len(args) == 0 {
		return errors.New("usage: kioku fetch URL --out FILE [--dir DIR]")
	}
	f := flag.NewFlagSet("fetch", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	out := f.String("out", "", "output file inside capture directory")
	dir := f.String("dir", os.Getenv("KIOKU_CAPTURE_DIR"), "capture directory")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *out == "" || *dir == "" {
		return errors.New("--out と capture の一時ディレクトリを指定する必要がある")
	}
	if bound := os.Getenv("KIOKU_CAPTURE_DIR"); bound != "" && filepath.Clean(*dir) != filepath.Clean(bound) {
		return errors.New("--dir が capture の一時ディレクトリと異なる")
	}
	return fetchURL(ctx, nil, args[0], *dir, *out)
}

func fetchURL(ctx context.Context, transport http.RoundTripper, rawURL, dir, output string) error {
	if !filepath.IsAbs(dir) || filepath.Base(filepath.Dir(dir)) != "capture-tmp" || filepath.Base(filepath.Dir(filepath.Dir(dir))) != ".kioku" || !captureTokenPattern.MatchString(filepath.Base(dir)) {
		return errors.New("capture-tmp の所有トークン付きディレクトリが必要")
	}
	lock, err := readCaptureLock(filepath.Join(filepath.Dir(filepath.Dir(dir)), "capture.lock"))
	if err != nil {
		return err
	}
	if lock == nil || lock.Token != filepath.Base(dir) {
		return errors.New("capture のロック所有者が変わった")
	}
	rows, err := captureSnapshot(dir)
	if err != nil {
		return err
	}
	found := false
	for _, row := range rows {
		if row.URL == rawURL {
			found = true
		}
	}
	if !found {
		return errors.New("URL は capture のスナップショットにない")
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
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if err := validate(u); err != nil {
		return err
	}
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
	return err
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
	absolute, err := captureSourcePath(dir, path)
	if err != nil {
		return nil, err
	}
	parent, err := openFetchDirectory(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(absolute), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), absolute)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("capture put requires a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("capture put source exceeds 1 MiB")
	}
	return data, nil
}
