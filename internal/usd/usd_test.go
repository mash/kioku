package usd

import "testing"

func TestParse(t *testing.T) {
	for input, want := range map[string]int64{"0": 0, "20": 20_000_000, "0.000001": 1, "12.34": 12_340_000, "10000": 10_000_000_000} {
		got, err := Parse(input)
		if err != nil || got != want {
			t.Fatal(input, got, err)
		}
	}
	for _, input := range []string{"-1", "1e3", "NaN", "10000.01", "1.0000001", ".5", ""} {
		if _, err := Parse(input); err == nil {
			t.Fatal("invalid amount", input)
		}
	}
}
