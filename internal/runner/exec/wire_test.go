package execrunner

import (
	"bytes"
	"fmt"
	"testing"
)

func TestResultLine(t *testing.T) {
	for _, raw := range []string{"", "log only", `{"version":2,"cost_cents":1}`, `{"version":1}`, `{"version":1,"cost_cents":null}`, `{"version":1,"cost_cents":-1}`, `{"version":1,"cost_cents":1000001}`, `{"version":1,"cost_cents":1.5}`, `{"version":1,"cost_cents":"1"}`, `{"version":1,"cost_cents":1} trailing`} {
		if _, ok := parseResult([]byte(raw)); ok {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, cost := range []int64{0, 137, 1_000_000} {
		raw := fmt.Sprintf("log\n{\"version\":1,\"cost_cents\":%d}\n \n", cost)
		var b tailBuffer
		_, _ = b.Write([]byte(raw))
		got, ok := parseResult(b.line())
		if !ok || *got.CostCents != cost {
			t.Fatal(got, ok)
		}
	}
}
func TestOutputTails(t *testing.T) {
	var b tailBuffer
	_, _ = b.Write(bytes.Repeat([]byte("x"), outputLimit*2))
	tail := []byte("\n{\"version\":1,\"cost_cents\":9}\n")
	for _, v := range tail {
		_, _ = b.Write([]byte{v})
	}
	if len(b.current) > outputLimit || len(b.last) > outputLimit || b.total != int64(outputLimit*2+len(tail)) {
		t.Fatal(len(b.current), len(b.last), b.total)
	}
	if result, ok := parseResult(b.line()); !ok || *result.CostCents != 9 {
		t.Fatal(result, ok)
	}
}

func TestLargeOutput(t *testing.T) {
	var b tailBuffer
	chunk := bytes.Repeat([]byte("large output line\n"), 4096)
	for i := 0; i < 1024; i++ {
		_, _ = b.Write(chunk)
	}
	for i := 0; i < outputLimit*2; i++ {
		_, _ = b.Write([]byte("x"))
	}
	if _, ok := parseResult(b.line()); ok {
		t.Fatal("accepted an oversized line")
	}
	_, _ = b.Write([]byte("\n{\"version\":1,\"cost_cents\":9}\n \n"))
	if got, ok := parseResult(b.line()); !ok || *got.CostCents != 9 {
		t.Fatal(got, ok)
	}
	if len(b.current) > outputLimit || len(b.last) > outputLimit {
		t.Fatal("unbounded output")
	}
	_, _ = b.Write([]byte("{\"version\":1,\"cost_cents\":10}"))
	if got, ok := parseResult(b.line()); !ok || *got.CostCents != 10 {
		t.Fatal(got, ok)
	}
	var stderr byteCounter
	for i := 0; i < 1024; i++ {
		n, err := stderr.Write(chunk)
		if n != len(chunk) || err != nil {
			t.Fatal(n, err)
		}
	}
	if stderr.total != int64(len(chunk))*1024 {
		t.Fatal(stderr.total)
	}
}
