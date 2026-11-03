package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mash/kioku/internal/store"
)

// Online backups are operator-only. No public HTTP route exposes account data.
func serveAdmin(ctx context.Context, path string, db *store.Store) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		probe, probeErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if probeErr == nil {
			probe.Close()
			return nil, errors.New("admin socket already listening")
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("admin path exists and is not a socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	busy := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /backup", func(w http.ResponseWriter, r *http.Request) {
		select {
		case busy <- struct{}{}:
			defer func() { <-busy }()
		default:
			http.Error(w, "backup already running", 409)
			return
		}
		var req struct {
			Destination string `json:"destination"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil || !filepath.IsAbs(req.Destination) {
			http.Error(w, "absolute destination path required", 400)
			return
		}
		// The new backup directory must be operator-owned and must not exist. The
		// service never opens another live catalog or pauses generation for uploads.
		if err := db.Backup(req.Destination); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "completed", "destination": req.Destination})
	})
	server := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 10 * time.Minute}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	stop := func() { _ = server.Close(); <-done; _ = os.Remove(path) }
	return stop, nil
}
func requestOnlineBackup(socket, dest string) error {
	absolute, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"destination": absolute})
	client := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	resp, err := client.Post("http://localhost/backup", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("online backup failed: %s", b)
	}
	fmt.Fprint(os.Stdout, string(b))
	return nil
}
