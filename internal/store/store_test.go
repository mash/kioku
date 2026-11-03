package store

import (
	"errors"
	"github.com/mash/kioku/internal/model"
	"testing"
)

func TestTransactionRollbackPersistenceAndLock(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(dir); err == nil {
		second.Close()
		t.Fatal("second writer allowed")
	}
	if err = s.Update(func(st *model.State) error { st.Users["alice"] = model.User{ID: "alice", Name: "Before"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *model.State) error {
		u := st.Users["alice"]
		u.Name = "After"
		st.Users["alice"] = u
		return errors.New("abort")
	}); err == nil {
		t.Fatal("expected abort")
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.View(func(st model.State) error {
		if st.Users["alice"].Name != "Before" {
			t.Fatal("rollback or persistence failed")
		}
		return nil
	})
}
