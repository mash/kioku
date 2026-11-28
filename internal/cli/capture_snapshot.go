package cli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mash/kioku/internal/atomicfile"
)

func captureSnapshot(token, dir string) (string, []queueRow, error) {
	if !captureTokenPattern.MatchString(token) {
		return "", nil, errors.New("invalid capture token")
	}
	if !filepath.IsAbs(dir) || filepath.Base(dir) != token || filepath.Base(filepath.Dir(dir)) != "capture-tmp" || filepath.Base(filepath.Dir(filepath.Dir(dir))) != ".kioku" {
		return "", nil, errors.New("invalid capture directory")
	}
	lock, err := readCaptureLock(captureLockPathFromTmp(dir))
	if err != nil {
		return "", nil, err
	}
	if !lockBusy(lock, time.Now()) || lock.Token != token {
		return "", nil, errors.New("capture lock ownership changed")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() {
		return "", nil, errors.New("capture directory must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, err
	}
	data, err := readCaptureFile(resolved, filepath.Join(resolved, "snapshot.tsv"), maxCaptureSnapshotBytes)
	if err != nil {
		return "", nil, err
	}
	if lock.SnapshotHash == "" || fmt.Sprintf("%x", sha256.Sum256(data)) != lock.SnapshotHash {
		return "", nil, errors.New("capture snapshot changed")
	}
	rows, err := parseQueue(data)
	return resolved, rows, err
}

func (a *App) saveCaptureSnapshot(token string, rows []queueRow) error {
	return updateCaptureLock(a.captureLockPath(), func(lock *captureLock) error {
		if lock == nil || lock.Token != token {
			return errors.New("capture lock ownership changed")
		}
		snapshot := formatQueue(rows)
		if err := atomicfile.Write(filepath.Join(a.captureTmp(token), "snapshot.tsv"), snapshot, 0600); err != nil {
			return err
		}
		lock.SnapshotHash = fmt.Sprintf("%x", sha256.Sum256(snapshot))
		return nil
	})
}
