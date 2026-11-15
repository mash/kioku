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

	"github.com/mash/kioku/internal/store"
)

func TestOnlineBackupKeepsServiceOpenAndIncludesAllSQLContent(t *testing.T) {
	dir := t.TempDir()
	stateStore, err := store.OpenDatabase(filepath.Join(dir, "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	if err = stateStore.Write(t.Context(), func(q *store.Queries) error {
		if err := q.PutUser(store.UserRow{ID: "u"}); err != nil {
			return err
		}
		if err := q.PutProject(store.ProjectRow{ID: "p", Slug: "p"}); err != nil {
			return err
		}
		return q.PutInbox(store.InboxRow{ID: "s", ProjectID: "p", SourceURL: "https://example.com/a"})
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
	stop, err := serveAdmin(context.Background(), socket, stateStore)
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
	if err = stateStore.Write(t.Context(), func(q *store.Queries) error {
		return q.PutUser(store.UserRow{ID: "u", Data: store.UserData{Name: "Still serving"}})
	}); err != nil {
		t.Fatal("backup closed or poisoned live store", err)
	}
	restored, err := store.OpenBackupDatabase(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Read(t.Context(), func(q *store.Queries) error {
		u, err := q.User("u")
		if err != nil {
			return err
		}
		items, err := q.ProjectInbox("p")
		if len(items) != 1 || u.Data.Name != "" {
			t.Error("backup mixed snapshots or lost SQL content")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if second, err := serveAdmin(context.Background(), socket, stateStore); err == nil {
		second()
		t.Fatal("active admin socket replaced")
	}
}
