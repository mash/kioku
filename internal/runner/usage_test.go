package runner

import (
	"math"
	"testing"
)

func usageInt(v int64) *int64 { return &v }
func TestUsagePreservesUnknownAndKnownZero(t *testing.T) {
	got := SumUsage([]UsageSample{{CostCents: usageInt(0), InputTokens: usageInt(5)}, {CostCents: usageInt(40)}})
	if got.CostCents == nil || *got.CostCents != 40 || got.InputTokens != nil {
		t.Fatal(got)
	}
	if got := SumUsage([]UsageSample{{CostCents: usageInt(0)}}); got.CostCents == nil || *got.CostCents != 0 {
		t.Fatal("known zero lost")
	}
	if got := SumUsage(nil); got.CostCents != nil {
		t.Fatal("empty samples became zero")
	}
}
func TestUsageOverflowAffectsOnlyThatField(t *testing.T) {
	model := "claude"
	got := SumUsage([]UsageSample{{Model: &model, InputTokens: usageInt(math.MaxInt64), CostCents: usageInt(1)}, {Model: &model, InputTokens: usageInt(1), CostCents: usageInt(2)}})
	if got.InputTokens != nil || got.CostCents == nil || *got.CostCents != 3 || got.Model == nil || *got.Model != model {
		t.Fatal(got)
	}
	other := "different"
	got = SumUsage([]UsageSample{{Model: &model}, {Model: &other}})
	if got.Model != nil {
		t.Fatal("mismatched models combined")
	}
}
