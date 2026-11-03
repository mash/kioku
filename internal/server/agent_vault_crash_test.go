package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
)

type crashAfterVaultCreate struct {
	managedagents.Client
	vault string
}

func (c *crashAfterVaultCreate) CreateJobVault(ctx context.Context, in managedagents.VaultInput) (string, error) {
	id, err := c.Client.CreateJobVault(ctx, in)
	if err != nil {
		return id, err
	}
	c.vault = id
	panic("simulated crash before vault ID persistence")
}

func TestManagedVaultCreationCrashRecoversCleanup(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := managedJob(t, f, token, p.ID, false)
	client := f.s.Config.ManagedAgents
	crash := &crashAfterVaultCreate{Client: client}
	f.s.Config.ManagedAgents = crash
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash point not reached")
			}
		}()
		_ = f.s.startAgentJob(context.Background(), j)
	}()
	f.s.Config.ManagedAgents = client
	if crash.vault == "" || ma.Credential(crash.vault) == "" {
		t.Fatal("remote credential was not created")
	}
	if err := f.db.Update(func(st *model.State) error {
		current := st.Jobs[j.ID]
		current.Managed.PreparedAt = time.Now().Add(-6 * time.Minute)
		st.Jobs[j.ID] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restartManagedFixture(t, f)
	ma.SetError("GET", "/v1/vaults", 503)
	j = reconcileManaged(t, f, storedJob(t, f, j.ID))
	if j.Status != "failed" || j.Managed.CleanupComplete {
		t.Fatal("unavailable vault discovery lost cleanup intent")
	}
	ma.SetError("GET", "/v1/vaults", 0)
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, storedJob(t, f, j.ID))
	if j.Status != "failed" || !j.Managed.CleanupComplete || !ma.VaultArchived(crash.vault) {
		t.Fatalf("vault orphaned after restart: status=%s cleanup=%v archived=%v", j.Status, j.Managed.CleanupComplete, ma.VaultArchived(crash.vault))
	}
}
