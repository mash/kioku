package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestPendingAgentCleanupRemainsInHotIndex(t *testing.T) {
	st := model.NewState()
	st.Projects["p"] = model.Project{ID: "p"}
	st.Jobs["pending"] = model.Job{ID: "pending", ProjectID: "p", Status: "completed", Run: &model.AgentRun{}}
	for i := range hotRecords + 1 {
		id := fmt.Sprintf("recent-%d", i)
		st.Jobs[id] = model.Job{ID: id, ProjectID: "p", Status: "completed", Created: time.Now()}
	}
	compactContent(&st)
	if st.Jobs["pending"].Run == nil {
		t.Fatal("pending cleanup evicted")
	}
	j := st.Jobs["pending"]
	j.Run.CleanupComplete = true
	st.Jobs[j.ID] = j
	compactContent(&st)
	if _, ok := st.Jobs["pending"]; ok {
		t.Fatal("completed cleanup retained forever")
	}
}
