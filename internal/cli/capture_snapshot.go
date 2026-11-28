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

func resolveCaptureDir(token, dir string) (string, error) {
	if !captureTokenPattern.MatchString(token) {
		return "", errors.New("invalid capture token")
	}
	if !filepath.IsAbs(dir) || filepath.Base(dir) != token || filepath.Base(filepath.Dir(dir)) != "capture-tmp" || filepath.Base(filepath.Dir(filepath.Dir(dir))) != ".kioku" {
		return "", errors.New("invalid capture directory")
	}
	lock, err := readCaptureLock(captureLockPathFromTmp(dir))
	if err != nil {
		return "", err
	}
	if !lockBusy(lock, time.Now()) || lock.Token != token {
		return "", errors.New("capture lock ownership changed")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("capture directory must be a real directory")
	}
	return filepath.EvalSymlinks(dir)
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

func captureSnapshot(dir string) ([]queueRow, error) {
	lock, err := readCaptureLock(captureLockPathFromTmp(dir))
	if err != nil {
		return nil, err
	}
	if !lockBusy(lock, time.Now()) || lock.Token != filepath.Base(dir) {
		return nil, errors.New("capture lock ownership changed")
	}
	data, err := readCaptureSource(dir, filepath.Join(dir, "snapshot.tsv"))
	if err != nil {
		return nil, err
	}
	if lock.SnapshotHash == "" || fmt.Sprintf("%x", sha256.Sum256(data)) != lock.SnapshotHash {
		return nil, errors.New("capture snapshot changed")
	}
	return parseQueue(data)
}
