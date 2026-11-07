package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/buildinfo"
)

func TestProjectCommandBoundary(t *testing.T) {
	a := &App{Out: io.Discard, Err: io.Discard, ctx: context.Background()}
	for _, cmd := range []string{"create", "join", "list", "delete", "share", "usage", "audit", "maintenance"} {
		if err := a.dispatch([]string{cmd}); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
}

func TestClientVersionHeader(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Kioku-Client-Version"); got != buildinfo.Version {
			t.Errorf("version = %q", got)
		}
		w.Write([]byte(`[]`))
	}))
	defer ts.Close()
	a := &App{Config: Config{Server: ts.URL}, Client: ts.Client(), Out: io.Discard, ctx: context.Background()}
	if err := a.dispatch([]string{"project", "list"}); err != nil {
		t.Fatal(err)
	}
}
