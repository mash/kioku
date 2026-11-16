package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func sqlAuthFixture(t *testing.T) (*store.Database, *auth.Cipher) {
	t.Helper()
	d, err := store.OpenDatabase(filepath.Join(t.TempDir(), "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Write(t.Context(), func(q *store.Queries) error {
		if err := q.PutUser(store.UserRow{ID: "u", Email: "u@example.com"}); err != nil {
			return err
		}
		if err := q.PutOrganization(store.OrganizationRow{ID: "o", Slug: "org"}); err != nil {
			return err
		}
		if err := q.PutProject(store.ProjectRow{ID: "p", OrgID: "o", Slug: "project"}); err != nil {
			return err
		}
		return q.PutProjectMember("p", "u", "owner")
	}); err != nil {
		t.Fatal(err)
	}
	return d, c
}
func sqlTestPtr[T any](v T) *T { return &v }
func sqlJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func sqlWrite(t *testing.T, d catalog, fn func(*store.Queries) error) {
	t.Helper()
	if err := d.Write(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}
func expectSQLStatus(t *testing.T, err error, status int) {
	t.Helper()
	var api apiError
	if !errors.As(err, &api) || api.status != status {
		t.Fatalf("status %d: %v", status, err)
	}
}
func TestLoopbackIssuanceFailureRollsBackConsumption(t *testing.T) {
	t.Parallel()
	d, _ := sqlAuthFixture(t)
	now := time.UnixMilli(1000)
	data := store.CLILoginData{LoopbackCodeHash: sqlTestPtr(auth.Hash("code")), LoopbackChallenge: sqlTestPtr(auth.Challenge("verifier"))}
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.InsertSession(store.SessionRow{ID: auth.Hash("device"), Kind: "cli_login", Status: "pending", UserID: "u", Expires: 2000, Data: sqlJSON(t, data)}); err != nil {
			return err
		}
		return q.InsertSession(store.SessionRow{ID: auth.Hash("collision"), Kind: "user", Status: "active", UserID: "u", Expires: 2000, Data: json.RawMessage(`{}`)})
	})
	err := d.Write(t.Context(), func(q *store.Queries) error {
		_, err := exchangeCLILogin(q, "device", "code", "verifier", "collision", now)
		return err
	})
	if err == nil {
		t.Fatal("collision accepted")
	}
	sqlWrite(t, d, func(q *store.Queries) error {
		v, err := q.Session(auth.Hash("device"))
		if err != nil {
			return err
		}
		if v.Status != "pending" {
			t.Fatal("failed issuance consumed code")
		}
		_, err = exchangeCLILogin(q, "device", "code", "verifier", "unique", now)
		return err
	})
}
