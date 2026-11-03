package server

import (
	"context"
	"sync"
	"testing"

	"github.com/mash/kioku/internal/managedagents"
)

type pauseSessionClient struct {
	managedagents.Client
	entered, release chan struct{}
	once             sync.Once
}

func (c *pauseSessionClient) GetSession(ctx context.Context, id string) (managedagents.Session, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Client.GetSession(ctx, id)
}
func TestManagedOutputArrivingDuringSessionInspectionIsPublished(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	c := &pauseSessionClient{Client: f.s.Config.ManagedAgents, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.Config.ManagedAgents = c
	done := make(chan error, 1)
	go func() { done <- f.s.reconcileAgentJob(context.Background(), j) }()
	<-c.entered
	sendOutput(t, f, ma, j, "Accepted output")
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 123)
	close(c.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := storedJob(t, f, j.ID); got.Status != "completed" {
		t.Fatal("accepted output lost", got.Status, got.Error)
	}
}
