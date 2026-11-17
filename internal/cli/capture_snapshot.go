package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mash/kioku/internal/atomicfile"
)

func (a *App) captureTokenDir(token string) (string, error) {
	if !captureTokenPattern.MatchString(token) {
		return "", errors.New("invalid capture token")
	}
	lock, err := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
	if err != nil {
		return "", err
	}
	if lock == nil || lock.invalid || lock.Token != token {
		return "", errors.New("capture lock ownership changed")
	}
	return filepath.EvalSymlinks(a.captureTmp(token))
}

func (a *App) saveCaptureSnapshot(token string, rows []queueRow) error {
	return updateLockedFile(filepath.Join(a.metadataDir(), "capture.lock"), func(data []byte) ([]byte, bool, error) {
		var lock captureLock
		if err := decodeJSON(data, &lock); err != nil {
			return nil, false, err
		}
		if lock.Token != token {
			return nil, false, errors.New("capture lock ownership changed")
		}
		snapshot := formatQueue(rows)
		if err := atomicfile.Write(filepath.Join(a.captureTmp(token), "snapshot.tsv"), snapshot, 0600); err != nil {
			return nil, false, err
		}
		lock.SnapshotHash = fmt.Sprintf("%x", sha256.Sum256(snapshot))
		next, err := json.Marshal(lock)
		return next, true, err
	})
}

func captureSnapshot(dir string) ([]queueRow, error) {
	lock, err := readCaptureLock(filepath.Join(filepath.Dir(filepath.Dir(dir)), "capture.lock"))
	if err != nil {
		return nil, err
	}
	if lock == nil || lock.invalid || lock.Token != filepath.Base(dir) {
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
