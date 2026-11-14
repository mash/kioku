package server

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
)

func TestGenerationLogsOnlyPersistedChanges(t *testing.T) {
	for _, queue := range []bool{false, true} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("queue=%v/reject=%v", queue, reject), func(t *testing.T) {
				objects := &rejectRunRefStore{Store: local.Store{Dir: t.TempDir()}}
				f := objectFixture(t, t.TempDir(), objects)
				token := f.login("owner@example.com")
				p := securityProject(t, f, token)
				if queue {
					putFile(t, f, token, p.ID, "a.md", "Body")
				}
				deadline := time.Now().UTC()
				if err := f.stateStore.Update(func(st *model.State) error {
					p := st.Projects[p.ID]
					p.NextGenerateAt = deadline
					st.Projects[p.ID] = p
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				objects.reject = reject
				err := f.s.scheduleGenerations(deadline)
				if reject {
					if err == nil {
						t.Fatal("expected persistence failure")
					}
					// The next tick must also report the poisoned store, even before
					// considering whether any project has a due deadline.
					if nextErr := f.s.scheduleGenerations(deadline); !errors.Is(nextErr, err) {
						t.Fatalf("poisoned tick error = %v, want %v", nextErr, err)
					}
					if logs.Len() != 0 || len(f.s.wake) != 0 {
						t.Fatalf("failed save reported success: logs=%q wake=%d", logs.String(), len(f.s.wake))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				message := "automatic generation deadline cleared"
				if queue {
					message = "automatic generation queued"
				}
				if strings.Count(logs.String(), message) != 1 || !strings.Contains(logs.String(), p.Namespace) {
					t.Fatalf("missing committed change: %s", &logs)
				}
			})
		}
	}
}
