package store

import (
	"errors"
	"testing"

	"github.com/mash/kioku/internal/model"
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

func TestCountProjects(t *testing.T) {
	s := &Store{state: model.State{Projects: map[string]model.Project{
		"active":   {ID: "active"},
		"deleting": {ID: "deleting", Deleting: true},
	}}}
	count, err := s.CountProjects(func(p model.Project) bool { return !p.Deleting })
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	count, err = s.CountProjects(func(p model.Project) bool { return false })
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, err := s.CountProjects(func(p model.Project) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("allocated %v times", allocs)
	}
	s.poisoned = errors.New("poisoned")
	called := false
	count, err = s.CountProjects(func(p model.Project) bool { called = true; return true })
	if !errors.Is(err, s.poisoned) || count != 0 || called {
		t.Fatalf("count=%d err=%v called=%v", count, err, called)
	}
}
