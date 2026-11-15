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
func (s *Server) wikiFiles(ctx context.Context, uid, pid string) (model.Revision, func(), error) {
	unlock, err := s.lockProject(ctx, pid)
	if err != nil {
		return model.Revision{}, nil, err
	}
	var rev model.Revision
	err = s.Store.View(func(st model.State) error {
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		rev = st.Revisions[p.Revision]
		return nil
	})
	if err != nil {
		unlock()
		return model.Revision{}, nil, err
	}
	return rev, unlock, nil
}

func (s *Server) wikiFile(ctx context.Context, rev model.Revision, name string) (string, error) {
	key, exists := rev.Files[name]
	if !exists {
		return "", fail(404, "wiki file not found")
	}
	b, err := s.Blobs.Get(ctx, key)
	return string(b), err
}

// snapshot serves the file listing and individual pages of the current Wiki.
// Publication and deletion use this same project lock for each file write.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, uid string) error {
	rev, unlock, err := s.wikiFiles(r.Context(), uid, r.PathValue("project"))
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
	for name, key := range rev.Files {
		if !wiki.ValidFile(name) {
			continue
		}
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
	b, _ := json.Marshal(files)
	jsonOut(w, 200, model.SyncManifest{Revision: auth.Hash(string(b))[:32], Files: files})
	return nil
}
