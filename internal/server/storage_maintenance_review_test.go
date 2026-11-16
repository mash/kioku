package server

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/mash/kioku/internal/store"
)

func TestMaintenancePrunesSessionsAndPreservesPendingLogin(t *testing.T) {
	t.Parallel()
	o, _ := storageFixture(t)
	sqlWrite(t, o.db, func(q *store.Queries) error {
		for _, row := range []store.SessionRow{
			{ID: "expired", Kind: "web", Status: "active", UserID: "u", Expires: 1000, Data: []byte(`{}`)},
			{ID: "consumed", Kind: "cli_login", Status: "active", UserID: "u", Expires: 2000, Data: []byte(`{}`)},
			{ID: "access", Kind: "mcp", Status: "active", UserID: "u", Expires: 1000, Data: fixtureJSON(t, store.MCPData{TokenKind: "access"})},
			{ID: "pending", Kind: "login_code", Status: "pending", UserID: "u", Expires: 2000, Data: fixtureJSON(t, store.LoginCodeData{GrantKey: "access"})},
		} {
			if err := q.InsertSession(row, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err := o.pruneExpired(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixtureCheck(t, o.db.Read(t.Context(), func(q *store.Queries) error {
		for _, id := range []string{"expired", "consumed"} {
			if _, err := q.Session(id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("session %s was not pruned: %v", id, err)
			}
		}
		for _, id := range []string{"access", "pending"} {
			if _, err := q.Session(id); err != nil {
				return err
			}
		}
		return nil
	}))
}
