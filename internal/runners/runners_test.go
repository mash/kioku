package runners

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/managedagents/fake"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

func TestBYOKResolvesCredentialsOnEveryOperation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	ma := fake.New()
	defer ma.Close()
	c := Config{MABaseURL: ma.URL(), AnthropicAPIKey: "operator", MAAgentID: "operator-agent", MAEnvironmentID: "operator-env", BaseURL: "https://kioku.test"}
	g, err := Load(c, db, cipher, nil)
	if err != nil {
		t.Fatal(err)
	}
	connection := &model.AnthropicConnection{ID: "connection", AgentID: "agent", EnvironmentID: "env", WorkspaceID: "workspace", State: "ready"}
	connection.EncryptedKey = cipher.Seal("first-key", "anthropic:user:connection")
	if err := db.Update(func(st *model.State) error {
		st.Users["user"] = model.User{ID: "user", Anthropic: connection}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	job := model.Job{ID: "job", UserID: "user", ProjectID: "project", AnthropicConnectionID: connection.ID}
	r, err := g.New(job)
	if err != nil {
		t.Fatal(err)
	}
	key := runner.AttemptKey{JobID: job.ID, AttemptID: "attempt", ProjectID: job.ProjectID}
	ref, err := r.Start(context.Background(), runner.RunSpec{Key: key, OutputURL: "https://kioku.test/output", Token: "token", BudgetCents: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(st *model.State) error {
		u := st.Users["user"]
		u.Anthropic.EncryptedKey = cipher.Seal("rotated-key", "anthropic:user:connection")
		st.Users[u.ID] = u
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Inspect(context.Background(), runner.Attempt{Key: key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	requests := ma.Requests()
	last := requests[len(requests)-1]
	if last.Header.Get("Authorization") != "Bearer rotated-key" || last.Header.Get("Anthropic-Workspace-Id") != "workspace" {
		t.Fatal("stale credentials")
	}
	if err := db.Update(func(st *model.State) error { u := st.Users["user"]; u.Anthropic = nil; st.Users[u.ID] = u; return nil }); err != nil {
		t.Fatal(err)
	}
	a := runner.Attempt{Key: key, Run: ref}
	for _, op := range []func() error{
		func() error { _, err := r.Start(context.Background(), runner.RunSpec{Key: key}); return err },
		func() error { _, err := r.Inspect(context.Background(), a); return err },
		func() error { return r.Stop(context.Background(), a) },
		func() error { return r.Cleanup(context.Background(), a) },
	} {
		if err := op(); err == nil {
			t.Fatal("removed connection accepted")
		}
	}
	if len(ma.Requests()) != len(requests) {
		t.Fatal("fell back to operator credentials")
	}
}

func TestNewWithoutOperatorConfiguration(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := Load(Config{}, db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.New(model.Job{})
	if r != nil || !errors.Is(err, runner.ErrNotConfigured) {
		t.Fatal(r, err)
	}
	_, err = s.New(model.Job{UserID: "missing", AnthropicConnectionID: "missing"})
	if err == nil || errors.Is(err, runner.ErrNotConfigured) {
		t.Fatal("BYOK must not fall back to deterministic processing", err)
	}
}
