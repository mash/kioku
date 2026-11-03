package cli_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/cli"
)

func TestSkillCommandRemoved(t *testing.T) {
	err := cli.Run(context.Background(), []string{"-C", t.TempDir(), "skill"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown command "skill"`) {
		t.Fatalf("got %v", err)
	}
}
