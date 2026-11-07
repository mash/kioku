package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

type failGraphPublication struct{ blob.Store }

func (b failGraphPublication) Put(ctx context.Context, key blob.Key, body []byte) error {
	if strings.HasSuffix(string(key), "/maps/graph.json") {
		return errors.New("publication unavailable")
	}
	return b.Store.Put(ctx, key, body)
}
func TestManagedPublicationFailurePreservesFinalUsage(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Final result")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	client, objects := f.runnerConfig.ManagedAgents, f.s.Blobs
	f.runnerConfig.ManagedAgents = &failSecondSessionLookup{Client: client}
	f.setRunners()
	f.s.Blobs = failGraphPublication{objects}
	if err := f.s.reconcileAgentJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	f.runnerConfig.ManagedAgents = client
	f.setRunners()
	f.s.Blobs = objects
	restartManagedFixture(t, f)
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Status != "failed" {
			t.Error("publication failure not recorded")
		}
		if costOf(st.Jobs[j.ID].Run.CostCents) != 123 {
			t.Error("final usage discarded on publication failure")
		}
		return nil
	})
}
