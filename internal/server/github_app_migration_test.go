package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestGitHubSourceMigrationClearsLegacySecretsOnlyOnce(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("migration@example.com")
	uid := ""
	if err := f.db.Update(func(st *model.State) error {
		uid = st.Sessions[auth.Hash(human)].UserID
		u := st.Users[uid]
		u.Connections = map[string]string{"github": "old-source-ciphertext", "google": "google-ciphertext"}
		u.Identities["github"] = "42"
		st.Users[uid] = u
		st.Sessions["old-github"] = model.Session{UserID: uid, IdentityProvider: "github", IdentityCredential: "old-session-ciphertext", Expires: time.Now().Add(time.Hour)}
		st.Devices["old-connect"] = model.Device{UserID: uid, IdentityProvider: "github", Purpose: "connect", IdentityCredential: "old-stage-ciphertext"}
		st.OAuth["old-state"] = model.OAuthState{UserID: uid, Provider: "github", Purpose: "connect", Device: "old-connect"}
		m := st.GitHubMetrics["cumulative"]
		m.MigrationComplete = false
		st.GitHubMetrics["cumulative"] = m
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.migrateGitHubSources(); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		u := st.Users[uid]
		if u.Connections["github"] != "" || u.Connections["google"] != "google-ciphertext" || u.Identities["github"] != "42" {
			t.Fatal("wrong identity migration")
		}
		if st.Sessions["old-github"].UserID != uid || st.Sessions["old-github"].IdentityCredential != "" {
			t.Fatal("session deleted or retained broad credential")
		}
		if _, ok := st.Devices["old-connect"]; ok {
			t.Fatal("staging retained")
		}
		if _, ok := st.OAuth["old-state"]; ok {
			t.Fatal("state retained")
		}
		if !st.GitHubMetrics["cumulative"].MigrationComplete {
			t.Fatal("migration marker absent")
		}
		return nil
	})
	_ = f.db.Update(func(st *model.State) error {
		s := st.Sessions["old-github"]
		s.IdentityCredential = "new-narrow-login-ciphertext"
		st.Sessions["old-github"] = s
		return nil
	})
	if err := f.s.migrateGitHubSources(); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Sessions["old-github"].IdentityCredential != "new-narrow-login-ciphertext" {
			t.Fatal("migration reran")
		}
		return nil
	})
	backup := filepath.Join(t.TempDir(), "backup")
	if err := f.db.Backup(backup); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenControl(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.View(func(st model.State) error {
		if !st.GitHubMetrics["cumulative"].MigrationComplete {
			t.Fatal("backup lost migration marker")
		}
		return nil
	})
}
