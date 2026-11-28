package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// A process lock protects project configuration and snapshot publication.
func (a *App) localLock() (func(), error) {
	return a.projectLock("lock")
}

// captureSubmissionLock orders puts, deletes and changes to active capture
// ownership without making asynchronous queue registration wait on HTTP.
func (a *App) captureSubmissionLock() (func(), error) {
	return a.projectLock("capture-submit.lock")
}

// lockCaptureQueue always takes the submission lock before the queue lock.
func (a *App) lockCaptureQueue() (unlockSubmission, unlockQueue func(), err error) {
	unlockSubmission, err = a.captureSubmissionLock()
	if err != nil {
		return nil, nil, err
	}
	unlockQueue, err = a.localLock()
	if err != nil {
		unlockSubmission()
		return nil, nil, err
	}
	return unlockSubmission, unlockQueue, nil
}

func (a *App) projectLock(name string) (func(), error) {
	if err := ensureDirectory(a.metadataDir()); err != nil {
		return nil, err
	}
	f, err := lockRegularFile(filepath.Join(a.metadataDir(), name), os.O_CREATE)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func lockRegularFile(path string, flags int) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("expected a regular file: %s", path)
	} else if err != nil && !(os.IsNotExist(err) && flags&os.O_CREATE != 0) {
		return nil, err
	}
	return openLockedFile(path, flags|os.O_RDWR|syscall.O_NOFOLLOW)
}

func openLockedFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
