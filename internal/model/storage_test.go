package model

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
)

func TestStorageContentHashesAndKeys(t *testing.T) {
	h := HashContent([]byte("abc"))
	if h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(h)
	}
	if _, err := ParseContentHash(string(h)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "abc", strings.ToUpper(string(h)), strings.Repeat("z", 64)} {
		if _, err := ParseContentHash(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	if got := WikiContentKey("p1", h); got != blob.Key("projects/p1/.kioku/wiki/"+string(h)+".md") {
		t.Fatal(got)
	}
}
func TestStorageMeasurementsKeepUnknownZeroAndOverflow(t *testing.T) {
	zero, forty, max := int64(0), int64(40), int64(math.MaxInt64)
	for _, tc := range []struct {
		in   []*int64
		want *int64
	}{
		{nil, nil}, {[]*int64{nil, &forty}, nil}, {[]*int64{&zero}, &zero}, {[]*int64{&zero, &forty}, &forty}, {[]*int64{&max, &forty}, nil},
	} {
		got := SumMeasurements(tc.in...)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Fatalf("sum: %v want %v", got, tc.want)
		}
	}
	b, err := json.Marshal(UsageMeasurements{})
	if err != nil || !strings.Contains(string(b), `"cost_cents":null`) {
		t.Fatalf("unknown JSON: %s %v", b, err)
	}
}
