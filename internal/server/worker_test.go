package server

import (
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestRevokedAgentCannotPublishAnAlreadyRunningGeneration(t *testing.T) {
	f, human, p, ma := managedFixture(t)
	id, token := testAgent(t, f, human, p.ID, "write")
	putFile(t, f, token, p.ID, "evidence.md", "Evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Summary")
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 123)
	_ = f.db.Update(func(st *model.State) error {
		v := st.AgentTokens[id]
		v.RevokedAt = time.Now().UTC()
		st.AgentTokens[id] = v
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal(got)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].Revision != "" || len(st.Revisions) != 0 {
			t.Error("revoked job published")
		}
		if st.Jobs[j.ID].Managed.ListCostCents != 123 {
			t.Error("actual consumed cost lost")
		}
		return nil
	})
}
