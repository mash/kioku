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
)

const maxPutBytes = 1 << 20
const maxInboxItems = 200

func (s *Server) put(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	if err := s.check(pid, uid); err != nil {
		return err
	}
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
	item, err := s.acceptInbox(r.Context(), pid, uid, req)
	if err != nil {
		return err
	}
	jsonOut(w, http.StatusAccepted, map[string]string{"id": item.ID, "status": item.Status})
	return nil
}

func (s *Server) acceptInbox(ctx context.Context, pid, uid string, req model.PutRequest) (model.InboxItem, error) {
	item := model.InboxItem{ID: auth.Token()[:20], ProjectID: pid, Submitter: uid, Created: time.Now().UTC()}
	encoded, err := json.Marshal(req)
	if err != nil {
		return item, err
	}
	if len(encoded) > maxPutBytes {
		return item, fail(413, "put exceeds 1 MiB")
	}
	var body string
	switch {
	case req.File != nil && !req.Remove && req.SourceURL == "":
		doc, err := source.FileWithProvenance(req.File.Name, req.File.Content)
		if err != nil {
			return item, fail(400, err.Error())
		}
		item.SourceURL, item.Status, body = doc.URL, "fetched", req.File.Content
	case req.File == nil && req.Remove && req.SourceURL != "":
		if err := source.ValidateSourceURL(req.SourceURL); err != nil {
			return item, fail(400, err.Error())
		}
		item.SourceURL, item.Status = req.SourceURL, "removed"
	default:
		return item, fail(400, "provide one file, or remove: true with source_url; put source_url and updated_at in the file frontmatter; the source_url argument is only for remove")
	}
	unlock, err := s.lockProject(ctx, pid)
	if err != nil {
		return item, err
	}
	defer unlock()
	check := func(st model.State) error {
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		if item.Status != "removed" && st.Revisions[p.Revision].StoredBytes > s.Config.MaxWikiBytes {
			return fail(413, "published wiki exceeds max-wiki-bytes; remove sources first")
		}
		count := 0
		for _, v := range st.Inbox {
			if v.ProjectID == pid && !(v.RunID == "" && v.SourceURL == item.SourceURL) {
				count++
			}
		}
		if count >= maxInboxItems {
			return fail(429, "project inbox is full")
		}
		return nil
	}
	if err = s.DB.View(check); err != nil {
		return item, err
	}
	if item.Status == "fetched" {
		if err = s.Blobs.Put(ctx, item.BodyKey(), []byte(body)); err != nil {
			return item, err
		}
	}
	err = s.DB.Update(func(st *model.State) error {
		if err := check(*st); err != nil {
			return err
		}
		for id, v := range st.Inbox {
			if v.ProjectID == pid && v.RunID == "" && v.SourceURL == item.SourceURL {
				delete(st.Inbox, id)
			}
		}
		st.Inbox[item.ID] = item
		return nil
	})
	return item, err
}
