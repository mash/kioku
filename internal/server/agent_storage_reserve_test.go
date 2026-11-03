package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestManagedAcceptedLargeOutputCanPublish(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "small evidence")
	j := startManaged(t, f, token, p.ID, false)
	pages := map[string]string{"wiki/index.md": "# Large wiki"}
	for i := range 72 {
		pages[fmt.Sprintf("wiki/page-%d.md", i)] = "# Page\n" + strings.Repeat("a", 256*1024-7)
	}
	code, body := agentRequest(t, f, j, ma.Credential(j.Managed.VaultID), "PUT", "output", outputArchive(t, pages))
	if code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatalf("accepted output could not publish: %s: %s", j.Status, j.Error)
	}
}
