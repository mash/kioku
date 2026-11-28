package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

const maxSourceFileBytes = 1 << 20

func localFilePath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err == nil && (u.Host != "" || u.User != nil) {
		return "", errors.New("put: file URL にホストとユーザー情報を書かない。file:///絶対パス の形にする")
	}
	if err == nil && (u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#")) {
		return "", errors.New("put: file URL にクエリとフラグメントを書かない")
	}
	if err != nil || !strings.HasPrefix(raw, "file:///") || !filepath.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) {
		return "", fmt.Errorf("put: file URL を解釈できない: %s。file:///絶対パス の形にする", raw)
	}
	return u.Path, nil
}

func readLocalFile(path string) ([]byte, os.FileInfo, error) {
	// Lstat gives the user a specific diagnostic. O_NOFOLLOW also checks at open.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("put: file: はシンボリックリンクを受け付けない: %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("put: file: は通常ファイル 1 つだけを指す: %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("put: file: は通常ファイル 1 つだけを指す: %s", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(maxSourceFileBytes)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxSourceFileBytes {
		return nil, nil, fmt.Errorf("%s exceeds 1 MiB", path)
	}
	return b, info, nil
}

func (a *App) putLocalFile(raw string) error {
	path, err := localFilePath(raw)
	if err != nil {
		return err
	}
	original, info, err := readLocalFile(path)
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	submitted, err := source.LocalFileContent(name, string(original), info.ModTime())
	switch {
	case errors.Is(err, source.ErrHTTPSLocalFile):
		return fmt.Errorf("put: %s の source_url は HTTPS の URL です。file: を付けず、通常のパスで put する: kioku put %s", name, shellQuote(path))
	case errors.Is(err, source.ErrLocalFileSource):
		return fmt.Errorf("put: %s の source_url は省くか kioku:local にする", name)
	case err != nil:
		return fmt.Errorf("%s: %w", path, err)
	case len(submitted) > maxSourceFileBytes:
		return fmt.Errorf("%s exceeds 1 MiB", path)
	}
	endpoint, err := a.projectPath()
	if err != nil {
		return err
	}
	if err := a.selectCredential(false); err != nil {
		return err
	}
	var result model.PutResponse
	code, err := a.request(http.MethodPost, endpoint+"/put", model.PutRequest{File: &model.FileInput{Name: name, Content: submitted}}, &result)
	if code >= 400 && err != nil {
		return err
	}
	if err != nil || code != http.StatusAccepted || !source.IsLocalKey(result.SourceURL) {
		return errors.New("put: 受付を確認できない。本文を変えずに同じコマンドで再試行する")
	}
	_, err = fmt.Fprintf(a.Out, "accepted %s\n", result.SourceURL)
	return err
}
