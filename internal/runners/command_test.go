package runners

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestCommandJSON(t *testing.T) {
	for _, raw := range []string{"null", "", `{}`, `"command"`, `[1]`, `["/program",null]`, `["/bin/program"] trailing`} {
		if _, err := ParseCommand("exec", raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	argv := []string{os.Args[0], "-test.run=TestExecHelper", "--", "argument with spaces"}
	raw, _ := json.Marshal(argv)
	got, err := ParseCommand("exec", string(raw))
	if err != nil || !reflect.DeepEqual(got, argv) {
		t.Fatal(got, err)
	}
	if got, err := ParseCommand("managed", "invalid unused command"); err != nil || got != nil {
		t.Fatal(got, err)
	}
}
