package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
)

// wikiFiles holds the project lock until the caller finishes reading files.
func (s *Server) wikiFiles(ctx context.Context, uid, pid string, names ...string) (wikiTree, func(), error) {
	unlock, err := s.storage.lock(ctx, pid)
	if err != nil {
		return wikiTree{}, nil, err
	}
	var rev wikiTree
	err = s.Store.Read(ctx, func(q *store.Queries) error {
		p, err := sqlMember(q, pid, uid)
		if err != nil {
			return err
		}
		rev, err = wikiProjection(q, p, names...)
		return err
	})
	if err != nil {
		unlock()
		return wikiTree{}, nil, err
	}
	return rev, unlock, nil
}

func (s *Server) wikiFile(ctx context.Context, rev wikiTree, name string) (string, error) {
	row, exists := rev.Files[name]
	if !exists {
		return "", fail(404, "wiki file not found")
	}
	b, err := s.Blobs.Get(ctx, wikiKey(row))
	if err == nil && (auth.Hash(string(b)) != row.Hash || int64(len(b)) != row.Data.Bytes) {
		return "", errors.New("wiki content integrity check failed")
	}
	return string(b), err
}

// snapshot serves the file listing and individual pages of the current Wiki.
// Publication and deletion use this same project lock for each file write.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, uid string) error {
	rev, unlock, err := s.wikiFiles(r.Context(), uid, r.PathValue("project"), r.URL.Query().Get("file"))
	if err != nil {
		return err
	}
	defer unlock()
	if name := r.URL.Query().Get("file"); name != "" {
		if !wiki.ValidFile(name) {
			return fail(400, "invalid wiki filename")
		}
		body, err := s.wikiFile(r.Context(), rev, name)
		if err != nil {
			return err
		}
		jsonOut(w, 200, model.SnapshotFile{Body: body, Hash: auth.Hash(body)})
		return nil
	}
	if r.URL.Query().Get("manifest") != "1" {
		return fail(400, "specify manifest=1 or file")
	}
	files := map[string]model.SyncFile{}
	for name, row := range rev.Files {
		if wiki.ValidFile(name) {
			files[name] = model.SyncFile{Hash: row.Hash, Bytes: row.Data.Bytes}
		}
	}
	b, _ := json.Marshal(files)
	jsonOut(w, 200, model.SyncManifest{Revision: auth.Hash(string(b))[:32], Files: files})
	return nil
}
