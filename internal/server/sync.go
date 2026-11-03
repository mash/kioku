package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

// wikiFiles holds the project lock until the caller finishes reading files.
func (s *Server) wikiFiles(ctx context.Context, uid, pid string) (model.Revision, map[string]string, func(), error) {
	unlock, err := s.lockProject(ctx, pid)
	if err != nil {
		return model.Revision{}, nil, nil, err
	}
	var rev model.Revision
	err = s.DB.View(func(st model.State) error {
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		rev = st.Revisions[p.Revision]
		return nil
	})
	if err != nil {
		unlock()
		return model.Revision{}, nil, nil, err
	}
	views := map[string]string{}
	if err = s.addIntakeView(ctx, uid, pid, views); err != nil {
		unlock()
		return model.Revision{}, nil, nil, err
	}
	return rev, views, unlock, nil
}

func (s *Server) wikiFile(ctx context.Context, rev model.Revision, views map[string]string, name string) (string, error) {
	if body, ok := views[name]; ok {
		return body, nil
	}
	key, exists := rev.Files[name]
	if !exists {
		return "", fail(404, "wiki file not found")
	}
	b, err := s.Blobs.Get(ctx, key)
	return string(b), err
}

// syncSnapshot exposes the current mutable tree, not historical revisions.
// Publication and deletion use this same project lock for each file write.
func (s *Server) syncSnapshot(w http.ResponseWriter, r *http.Request, uid string) error {
	rev, views, unlock, err := s.wikiFiles(r.Context(), uid, r.PathValue("project"))
	if err != nil {
		return err
	}
	defer unlock()
	if name := r.URL.Query().Get("file"); name != "" {
		if !wiki.ValidFile(name) {
			return fail(400, "invalid wiki filename")
		}
		body, err := s.wikiFile(r.Context(), rev, views, name)
		if err != nil {
			return err
		}
		jsonOut(w, 200, model.SnapshotFile{Body: body, Hash: auth.Hash(body)})
		return nil
	}
	files := map[string]model.SyncFile{}
	for name, key := range rev.Files {
		hash, size := rev.FileHashes[name], rev.FileBytes[name]
		if hash == "" {
			b, err := s.Blobs.Get(r.Context(), key)
			if err != nil {
				return err
			}
			hash = auth.Hash(string(b))
			size = int64(len(b))
		}
		files[name] = model.SyncFile{Hash: hash, Bytes: size}
	}
	for name, body := range views {
		files[name] = model.SyncFile{Hash: auth.Hash(body), Bytes: int64(len(body))}
	}
	b, _ := json.Marshal(files)
	jsonOut(w, 200, model.SyncManifest{Revision: auth.Hash(string(b))[:32], Files: files})
	return nil
}
