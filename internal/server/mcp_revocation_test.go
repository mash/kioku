package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

type afterReadCatalog struct {
	catalog
	after func()
}

func (d *afterReadCatalog) Read(ctx context.Context, fn func(*store.Queries) error) error {
	err := d.catalog.Read(ctx, fn)
	if after := d.after; after != nil {
		d.after = nil
		after()
	}
	return err
}

func TestConnectorWikiRechecksGrantAfterSelection(t *testing.T) {
	for _, change := range []string{"deleted", "inactive", "expired", "ungranted"} {
		t.Run(change, func(t *testing.T) {
			f, human, p := orgProject(t)
			access := connectorAccessToken(t, f, human, p.ID)
			key := auth.Hash(access)
			f.s.Store = &afterReadCatalog{catalog: f.stateStore, after: func() {
				fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
					if change == "deleted" {
						return q.DeleteSession(key)
					}
					row, err := q.Session(key)
					if err != nil {
						return err
					}
					var data store.MCPData
					if err := json.Unmarshal(row.Data, &data); err != nil {
						return err
					}
					switch change {
					case "inactive":
						data.Active = false
					case "expired":
						row.Expires = 0
					case "ungranted":
						data.ProjectIDs = []string{"another-project"}
					}
					row.Data, err = json.Marshal(data)
					if err != nil {
						return err
					}
					return q.PutSession(row)
				}))
			}}
			_, unlock, err := f.s.connectorWikiFiles(t.Context(), key, p.Namespace)
			if unlock != nil {
				unlock()
			}
			want := 401
			if change == "ungranted" {
				want = 404
			}
			expectSQLStatus(t, err, want)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			unlock, err = f.s.lockProject(ctx, p.ID)
			fixtureCheck(t, err)
			unlock()
		})
	}
}
