package cli

import (
	"strings"
	"testing"
)

func TestRemovedSourceCommandsPointToPutAndWebViewer(t *testing.T) {
	for _, command := range []string{"add", "refresh", "list", "get"} {
		t.Run(command, func(t *testing.T) {
			// With no project or client, rejection must precede any API access.
			err := (&App{}).source([]string{command, "unused"})
			if err == nil || !strings.Contains(err.Error(), "kioku put") || !strings.Contains(err.Error(), "kioku open") {
				t.Fatal("missing replacement guidance", err)
			}
			if strings.Contains(help, "source "+command) {
				t.Fatal("removed command is advertised in help")
			}
		})
	}
}
