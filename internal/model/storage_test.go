package model

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
)

func TestStorageTimePreservesNullAndTruncatesSubMilliseconds(t *testing.T) {
	if TimeToStorage(time.Time{}) != nil {
		t.Fatal("zero must be null")
	}
	for _, ms := range []int64{-1234, 0, 1794712365123} {
		original := time.UnixMilli(ms).Add(456789 * time.Nanosecond)
		stored := TimeToStorage(original)
		if stored == nil || int64(*stored) != ms || !stored.Time().Equal(time.UnixMilli(ms)) {
			t.Fatalf("timestamp: %v", stored)
		}
		b, err := json.Marshal(stored)
		if err != nil || string(b) != strconv.FormatInt(ms, 10) {
			t.Fatalf("JSON: %s %v", b, err)
		}
	}
}
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
	b, err := json.Marshal(UsageValues{})
	if err != nil || !strings.Contains(string(b), `"cost_cents":null`) {
		t.Fatalf("unknown JSON: %s %v", b, err)
	}
}
