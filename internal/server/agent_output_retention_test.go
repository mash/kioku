package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func TestAgentOutputResendsRetainOnlySelectedVersion(t *testing.T) {
	f, raw, j, _, _ := generatorFixture(t)
	setManagedPhase(t, f, j, model.PhaseAgentRunning, "running")
	for i := range 6 {
		if c, b := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, map[string]string{"wiki/index.md": fmt.Sprintf("# Version %d", i)})); c != 200 {
			t.Fatal(c, string(b))
		}
	}
	keys, err := f.s.Blobs.List(context.Background(), agentJobPrefix(j)+"output-")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatal("superseded outputs accumulate", len(keys))
	}
	current := storedJob(t, f, j.ID)
	b, err := f.s.Blobs.Get(context.Background(), current.Managed.OutputKey)
	if err != nil || auth.Hash(string(b)) != current.Managed.OutputHash {
		t.Fatal("selected output lost", err)
	}
}

func TestAgentOutputCleanupFailureBoundsResends(t *testing.T) {
	f, raw, j, _, _ := generatorFixture(t)
	setManagedPhase(t, f, j, model.PhaseAgentRunning, "running")
	upload := func(i int) int {
		c, _ := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, map[string]string{"wiki/index.md": fmt.Sprintf("# Version %d", i)}))
		return c
	}
	if c := upload(1); c != 200 {
		t.Fatal(c)
	}
	objects := f.s.Blobs
	f.s.Blobs = unavailableCleanupStore{objects}
	if c := upload(2); c != 200 {
		t.Fatal(c)
	}
	if c := upload(3); c != 503 {
		t.Fatal("cleanup failure permitted unbounded writes", c)
	}
	keys, err := objects.List(context.Background(), agentJobPrefix(j)+"output-")
	if err != nil || len(keys) != 2 {
		t.Fatal(keys, err)
	}
	f.s.Blobs = objects
	if c := upload(4); c != 200 {
		t.Fatal(c)
	}
	keys, err = objects.List(context.Background(), agentJobPrefix(j)+"output-")
	if err != nil || len(keys) != 1 {
		t.Fatal(keys, err)
	}
}
