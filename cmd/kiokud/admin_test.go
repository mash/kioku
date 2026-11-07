package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestOnlineBackupKeepsServiceStoreOpenAndExcludesProjectContent(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Update(func(st *model.State) error {
		st.Users["u"] = model.User{ID: "u"}
		st.Projects["p"] = model.Project{ID: "p"}
		st.Inbox["s"] = model.InboxItem{ID: "s", ProjectID: "p", Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Unix socket paths have a small OS limit, even in deeply nested worktrees.
	socketDir, err := os.MkdirTemp(".", ".admin-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "admin.sock")
	stop, err := serveAdmin(context.Background(), socket, db)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe admin socket", info, err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	backup := filepath.Join(t.TempDir(), "backup")
	body, _ := json.Marshal(map[string]string{"destination": backup})
	resp, err := client.Post("http://localhost/backup", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode, string(raw))
	}
	if err = db.Update(func(st *model.State) error {
		u := st.Users["u"]
		u.Name = "Still serving"
		st.Users[u.ID] = u
		return nil
	}); err != nil {
		t.Fatal("backup closed or poisoned live store", err)
	}
	restored, err := store.OpenControl(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	_ = restored.View(func(st model.State) error {
		if len(st.Inbox) != 0 || st.Users["u"].ID != "u" {
			t.Error("backup did not separate config and content")
		}
		return nil
	})
	if second, err := serveAdmin(context.Background(), socket, db); err == nil {
		second()
		t.Fatal("active admin socket replaced")
	}
}
