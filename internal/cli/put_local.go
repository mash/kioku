package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"gopkg.in/yaml.v3"
)

const maxSourceFileBytes = 1 << 20
const maxLocalFileBytes = maxSourceFileBytes + source.LocalKeyLength - len(source.LocalSource)

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
	b, err := io.ReadAll(io.LimitReader(f, int64(maxLocalFileBytes)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxLocalFileBytes {
		return nil, nil, fmt.Errorf("%s exceeds 1 MiB", path)
	}
	return b, info, nil
}

func localFileContent(path, content, date string, dateSet bool) (string, string, error) {
	name := filepath.Base(path)
	if _, err := source.File(name, content); err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	var metadata *yaml.Node
	if source.HasFrontmatter(content) {
		var err error
		metadata, err = source.Frontmatter(content)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", path, err)
		}
	}
	hasLocalKey := false
	if value := source.FrontmatterValue(metadata, "source_url"); value != nil {
		u, _ := source.FrontmatterSourceURL(metadata)
		switch {
		case u == source.LocalSource:
		case source.LocalKeyDate(u) != "":
			hasLocalKey = true
		case strings.HasPrefix(strings.ToLower(u), "https:"):
			return "", "", fmt.Errorf("put: %s の source_url は HTTPS の URL です。file: を付けず、通常のパスで put する: kioku put %s", name, shellQuote(path))
		default:
			return "", "", fmt.Errorf("put: %s の source_url は HTTPS の URL でもローカルキーでもない: %s", name, value.Value)
		}
	}
	newDate := ""
	if value := source.FrontmatterValue(metadata, "updated_at"); value != nil {
		if _, err := source.ParseUpdatedAt(value.Value); err != nil || value.Kind != yaml.ScalarNode {
			return "", "", fmt.Errorf("put: %s の updated_at はオフセット付きの RFC 3339 でない: %s", name, value.Value)
		}
		if dateSet {
			return "", "", fmt.Errorf("put: %s には updated_at がある。--date を外す", name)
		}
	} else {
		if !dateSet {
			return "", "", fmt.Errorf("put: %s に updated_at が無い。資料自身の日付（例: 会議の日）を --date 2026-11-20 で渡すか、frontmatter に updated_at を書く。ファイルの更新時刻や put した時刻で補わない", name)
		}
		newDate = date
		if _, err := time.Parse(time.DateOnly, date); err == nil {
			newDate += "T00:00:00+09:00"
		} else if _, err := source.ParseUpdatedAt(date); err != nil {
			return "", "", errors.New("put: --date は YYYY-MM-DD かオフセット付きの RFC 3339 で書く")
		}
	}
	submitted, err := source.RewriteFrontmatter(content, source.LocalSource, newDate)
	// A key written by putLocalFile can make a previously accepted file larger.
	// Only permit that overhead when replacing the key brings it back under the limit.
	if err == nil && len(content) > maxSourceFileBytes && (!hasLocalKey || len(submitted) > maxSourceFileBytes) {
		return "", "", fmt.Errorf("%s exceeds 1 MiB", path)
	}
	return submitted, newDate, err
}

func (a *App) putLocalFile(raw, date string, dateSet bool) error {
	path, err := localFilePath(raw)
	if err != nil {
		return err
	}
	original, info, err := readLocalFile(path)
	if err != nil {
		return err
	}
	submitted, newDate, err := localFileContent(path, string(original), date, dateSet)
	if err != nil {
		return err
	}
	endpoint, err := a.projectPath()
	if err != nil {
		return err
	}
	if err := a.selectCredential(false); err != nil {
		return err
	}
	var result model.PutResponse
	code, err := a.request(http.MethodPost, endpoint+"/put", model.PutRequest{File: &model.FileInput{Name: filepath.Base(path), Content: submitted}}, &result)
	if code >= 400 && err != nil {
		return err
	}
	if err != nil || code != http.StatusAccepted || source.LocalKeyDate(result.SourceURL) == "" {
		return errors.New("put: 受付を確かめられない。同じコマンドをもう一度実行する（同じキーになる）")
	}
	next, err := source.RewriteFrontmatter(string(original), result.SourceURL, newDate)
	if err == nil {
		err = writeLocalFile(path, original, []byte(next), info)
	}
	_, outErr := fmt.Fprintf(a.Out, "accepted %s\n", result.SourceURL)
	if errors.Is(err, errLocalFileChanged) {
		return fmt.Errorf("put: 受け付けたが、読んだ後に %s が変わったので書き戻さなかった", path)
	}
	if err != nil {
		return fmt.Errorf("put: 受け付けたが、%s に書き戻せなかった: %w", path, err)
	}
	return outErr
}

var errLocalFileChanged = errors.New("local file changed")

func writeLocalFile(path string, original, next []byte, info os.FileInfo) error {
	if bytes.Equal(original, next) {
		return checkLocalFile(path, original, info)
	}
	// filepath.Dir cleans .., which can change the parent through a symlink.
	dir, _ := filepath.Split(path)
	f, err := os.CreateTemp(dir, ".kioku-put-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(next)
	if err == nil {
		err = f.Chmod(info.Mode())
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := checkLocalFile(path, original, info); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func checkLocalFile(path string, original []byte, info os.FileInfo) error {
	current, currentInfo, err := readLocalFile(path)
	if err != nil {
		if stat, statErr := os.Lstat(path); os.IsNotExist(statErr) || (statErr == nil && (!os.SameFile(info, stat) || stat.Size() != int64(len(original)))) {
			return errLocalFileChanged
		}
		return err
	}
	if !os.SameFile(info, currentInfo) || info.Mode() != currentInfo.Mode() || !bytes.Equal(original, current) {
		return errLocalFileChanged
	}
	return nil
}
