package claudemanaged_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mash/kioku/internal/runner/claudemanaged"
)

func TestFindJobVaultsPaginatesAndIsolatesAttempts(t *testing.T) {
	a, f := newClient(t)
	f.SetPageSize(1)
	ctx := context.Background()
	var want []string
	for _, in := range []claudemanaged.VaultInput{
		{JobID: "other", AttemptID: "attempt"},
		{JobID: "job", AttemptID: "old-attempt"},
		{JobID: "job", AttemptID: "attempt"},
		{JobID: "job", AttemptID: "attempt"},
	} {
		id, err := a.CreateJobVault(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if in.JobID == "job" && in.AttemptID == "attempt" {
			want = append(want, id)
		}
	}
	got, err := a.FindJobVaults(ctx, "job", "attempt")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, want, err)
	}
	if err := a.ArchiveVault(ctx, want[0]); err != nil {
		t.Fatal(err)
	}
	got, err = a.FindJobVaults(ctx, "job", "attempt")
	if err != nil || !reflect.DeepEqual(got, want[1:]) {
		t.Fatal("archived vault rediscovered", got, err)
	}
	f.SetError("GET", "/v1/vaults", 503)
	if _, err := a.FindJobVaults(ctx, "job", "attempt"); !errors.Is(err, claudemanaged.ErrTemporary) {
		t.Fatal(err)
	}
}
