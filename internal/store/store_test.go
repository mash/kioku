package store

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
)

func TestTransactionRollbackPersistenceAndLock(t *testing.T) {
	dir := t.TempDir()
	objects := &migrationObjects{values: map[blob.Key][]byte{}}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*Database, error) {
		d, _, err := OpenStorage(t.Context(), dir, objects, func(bool) (*auth.Cipher, error) { return key, nil })
		return d, err
	}
	d, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if other, err := open(); err == nil {
		other.Close()
		t.Fatal("second writer allowed")
	}
	catalogWrite(t, d, func(q *Queries) error { return q.PutUser(UserRow{ID: "alice", Data: UserData{Name: "Before"}}) })
	if err = d.Write(t.Context(), func(q *Queries) error {
		if e := q.PutUser(UserRow{ID: "alice", Data: UserData{Name: "After"}}); e != nil {
			return e
		}
		return errors.New("abort")
	}); err == nil {
		t.Fatal("expected rollback")
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = open()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Read(t.Context(), func(q *Queries) error {
		u, e := q.User("alice")
		if u.Data.Name != "Before" {
			t.Fatal(u)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
func TestProjectMetricsSQLFailureDoesNotPoison(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		if e := q.PutProject(ProjectRow{ID: "active", Slug: "active"}); e != nil {
			return e
		}
		return q.PutProject(ProjectRow{ID: "deleting", Slug: "deleting", Deleting: true})
	})
	if e := d.Read(t.Context(), func(q *Queries) error {
		n, _, _, _, e := q.Metrics()
		if n != 2 {
			t.Fatal(n)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e := d.Write(t.Context(), func(q *Queries) error { _, e := q.exec("SELECT * FROM missing_table"); return e }); e == nil {
		t.Fatal("failure missing")
	}
	if e := d.Read(t.Context(), func(q *Queries) error { return q.Healthy() }); e != nil {
		t.Fatal(e)
	}
}
