package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
)

type failSecondSessionLookup struct {
	managedagents.Client
	calls int
}

func (c *failSecondSessionLookup) GetSession(ctx context.Context, id string) (managedagents.Session, error) {
	c.calls++
	if c.calls == 2 {
		return managedagents.Session{}, managedagents.ErrTemporary
	}
	return c.Client.GetSession(ctx, id)
}
func TestManagedRetainsObservedTerminalUsage(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "idle", "retries_exhausted", 123)
	f.runnerConfig.ManagedAgents = &failSecondSessionLookup{Client: f.runnerConfig.ManagedAgents}
	f.setRunners()
	j = reconcileManaged(t, f, j)
	restartManagedFixture(t, f)
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if costOf(st.Jobs[j.ID].Run.CostCents) != 123 {
			t.Error("observed terminal usage was discarded")
		}
		return nil
	})
}
