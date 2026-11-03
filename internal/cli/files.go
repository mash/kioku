package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// A process lock protects config-adjacent receipt and snapshot publication.
func (a *App) localLock() (func(), error) {
	if err := ensureDirectory(a.metadataDir()); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(filepath.Join(a.metadataDir(), "lock")); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("invalid project lock file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(a.metadataDir(), "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type manifest struct {
	Incomplete bool              `json:"incomplete,omitempty"`
	Revision   string            `json:"revision"`
	Server     string            `json:"server"`
	Project    string            `json:"project"`
	Hashes     map[string]string `json:"hashes"`
}
