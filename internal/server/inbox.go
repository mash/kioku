package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"github.com/mash/kioku/internal/store"
)

const maxPutBytes = 1 << 20
const maxInboxItems = 200

func (s *Server) put(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	r.Body = http.MaxBytesReader(w, r.Body, maxPutBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return fail(413, "put exceeds 1 MiB")
		}
		return fail(400, "could not read put")
	}
	var req model.PutRequest
	if !decodeMCPArguments(body, &req) {
		return fail(400, "invalid put request")
	}
	result, err := s.acceptInbox(r.Context(), pid, uid, req)
	if err != nil {
		return err
	}
	jsonOut(w, http.StatusAccepted, result)
	return nil
}

func (s *Server) acceptInbox(ctx context.Context, pid, uid string, req model.PutRequest, hooks ...mutationHook) (model.PutResponse, error) {
	item := model.InboxItem{ID: auth.Token()[:20], ProjectID: pid, Submitter: uid, Created: time.Now().UTC()}
	encoded, err := json.Marshal(req)
	if err != nil {
		return model.PutResponse{}, err
	}
	if len(encoded) > maxPutBytes {
		return model.PutResponse{}, fail(413, "put exceeds 1 MiB")
	}
	var body string
	switch {
	case req.File != nil && !req.Remove && req.SourceURL == "":
		doc, content, err := source.InboxFile(req.File.Name, req.File.Content)
		if err != nil {
			return model.PutResponse{}, fail(400, err.Error())
		}
		item.SourceURL, body = doc.URL, content
	case req.File == nil && req.Remove && req.SourceURL != "":
		if err := source.ValidateSourceURL(req.SourceURL); err != nil {
			return model.PutResponse{}, fail(400, err.Error())
		}
		item.SourceURL, item.Remove = req.SourceURL, true
	default:
		return model.PutResponse{}, fail(400, "provide one file, or remove: true with source_url; put source_url and updated_at in the file frontmatter; the source_url argument is only for remove")
	}
	hooks = append(hooks, s.requestHook(ctx, pid))
	if err := s.storage.putInbox(ctx, store.InboxRow{ID: item.ID, ProjectID: pid, SourceURL: item.SourceURL, Remove: item.Remove, Created: item.Created.UnixMilli(), Data: store.InboxData{Submitter: uid}}, []byte(body), hooks...); err != nil {
		return model.PutResponse{}, err
	}
	return model.PutResponse{SourceURL: item.SourceURL}, nil
}
