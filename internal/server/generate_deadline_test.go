package server

import (
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestDeadlineStage(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 15, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		deadline time.Time
		deleting bool
		want     deadlineResult
	}{
		{"none", time.Time{}, false, deadlineSkip},
		{"before", now.Add(time.Nanosecond), false, deadlineSkip},
		{"exact", now, false, deadlineDue},
		{"after", now.Add(-time.Nanosecond), false, deadlineDue},
		{"deleting", now.Add(-time.Second), true, deadlineSkip},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := model.Project{NextGenerateAt: tt.deadline, Deleting: tt.deleting}
			if got := deadlineStage(p, now); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeadlineAction(t *testing.T) {
	for _, hasJob := range []bool{false, true} {
		for _, hasEligible := range []bool{false, true} {
			for _, ownerUID := range []string{"", "owner"} {
				want := deadlineClear
				if hasJob {
					want = deadlineWait
				} else if hasEligible && ownerUID != "" {
					want = deadlineClearAndQueue
				}
				if got := deadlineAction(hasJob, hasEligible, ownerUID); got != want {
					t.Errorf("job=%v eligible=%v owner=%q: got %v, want %v", hasJob, hasEligible, ownerUID, got, want)
				}
			}
		}
	}
}
