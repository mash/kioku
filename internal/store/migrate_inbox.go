package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MigrateInbox irreversibly clears schema-1 project content under the catalog
// process lock. Operators must stop kiokud and approve the deletion beforehand.
// Writing schema 2 last makes interrupted deletion retryable. Schema-2 projects
// are never cleared, including projects already completed by an earlier attempt.
func MigrateInbox(ctx context.Context, dir string, objects Objects) (int, error) {
	if _, err := os.Stat(filepath.Join(dir, "catalog.sqlite")); err != nil {
		return 0, err
	}
	s, err := OpenControl(dir)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	s.objects = objects
	registerBuckets(objects, s.state)
	ids := make([]string, 0, len(s.state.Projects))
	for id, p := range s.state.Projects {
		if !validID(id) || p.ID != id || p.StorageRoot != "" && p.StorageRoot != "projects/"+id+"/" {
			return 0, errors.New("invalid project configuration")
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	legacy := map[string]projectContent{}
	for _, id := range ids {
		b, err := objects.Get(ctx, contentKey(id))
		if err != nil {
			return 0, err
		}
		var old projectContent
		if err = json.Unmarshal(b, &old); err != nil {
			return 0, err
		}
		if old.ProjectID != id {
			return 0, errors.New("project content identity mismatch")
		}
		if old.Schema == 2 {
			continue
		}
		if old.Schema != 1 {
			return 0, fmt.Errorf("unsupported project content schema %d", old.Schema)
		}
		legacy[id] = old
	}
	// Retire only credentials for jobs being deleted, before touching resources.
	for id, token := range s.state.AgentTokens {
		if _, ok := legacy[token.ProjectID]; ok && token.JobID != "" {
			delete(s.state.AgentTokens, id)
		}
	}
	if len(legacy) == 0 {
		return 0, nil
	}
	if err = writeRecords(s.catalog, s.state); err != nil {
		return 0, err
	}
	migrated := 0
	for _, id := range ids {
		old, ok := legacy[id]
		if !ok {
			continue
		}
		prefix := "projects/" + id + "/"
		keys, err := objects.List(ctx, prefix)
		if err != nil {
			return migrated, err
		}
		for _, key := range keys {
			name := strings.TrimPrefix(string(key), prefix)
			for _, remove := range []string{"wiki/", "maps/", "sources/", ".kioku/jobs/", ".kioku/inbox/", ".kioku/transactions/"} {
				if strings.HasPrefix(name, remove) {
					if err = objects.Delete(ctx, key); err != nil {
						return migrated, err
					}
					break
				}
			}
		}
		fresh := contentFor(s.state, id)
		fresh.Audit = old.Audit
		fresh.JobsValidAfter = time.Now().UTC()
		b, err := json.Marshal(fresh)
		if err != nil {
			return migrated, err
		}
		if err = objects.Put(ctx, contentKey(id), b); err != nil {
			return migrated, err
		}
		migrated++
	}
	return migrated, nil
}
