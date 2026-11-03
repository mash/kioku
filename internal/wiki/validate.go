package wiki

import (
	"fmt"
	"strings"
)

type PageLimits struct{ MaxPages int }

func ValidatePages(pages map[string]string, limits PageLimits) error {
	if len(pages) == 0 || limits.MaxPages <= 0 || len(pages) > limits.MaxPages {
		return fmt.Errorf("invalid page count: %d (maximum %d)", len(pages), limits.MaxPages)
	}
	if _, ok := pages["wiki/index.md"]; !ok {
		return fmt.Errorf("wiki/index.md: required page missing")
	}
	for name, body := range pages {
		if !ValidFile(name) || !strings.HasPrefix(name, "wiki/") {
			return fmt.Errorf("%s: invalid wiki filename", name)
		}
		if strings.HasPrefix(name, "wiki/source-") || strings.HasPrefix(name, "wiki/inbox") {
			return fmt.Errorf("%s: reserved service-owned page", name)
		}
		if len(body) > 256<<10 {
			return fmt.Errorf("%s: page exceeds 256 KiB", name)
		}
	}
	return nil
}

type SnapshotTooLargeError struct{ Bytes int64 }

func (e *SnapshotTooLargeError) Error() string { return "generated snapshot exceeds 64 MiB" }
