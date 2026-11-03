package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPullReturnsOrdinaryOutputAndPartialProgress(t *testing.T) {
	a, files, hits, status := syncFixture(t)
	out := &bytes.Buffer{}
	a.Out = out
	if err := a.pull(); err != nil {
		t.Fatal(err)
	}
	var result syncResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Added) != 2 {
		t.Fatal(out.String(), err)
	}
	before := (*hits)[""]
	out.Reset()
	if err := a.pull(); err != nil || strings.TrimSpace(out.String()) != "最新です" || (*hits)[""] != before+1 {
		t.Fatal(out.String(), err)
	}
	(*files)["wiki/index.md"] = "changed"
	(*files)["wiki/broken.md"] = "fail"
	out.Reset()
	if err := a.pull(); err == nil {
		t.Fatal("partial failure succeeded")
	}
	result = syncResult{}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Updated) != 1 {
		t.Fatal(out.String(), err)
	}
	*status = 403
	if err := a.pull(); err == nil {
		t.Fatal("access loss succeeded")
	}
}

func TestRemovedPullModesAndCommandsDoNotIssueRequests(t *testing.T) {
	a, _, hits, _ := syncFixture(t)
	for _, args := range [][]string{{"pull", "--json"}, {"pull", "--all"}, {"pull", "--due"}, {"pull", "unexpected"}, {"init", "project"}, {"use", "org/project"}, {"hook", "end"}, {"sync"}} {
		if err := a.dispatch(args); err == nil {
			t.Fatalf("removed operation accepted: %v", args)
		}
	}
	if len(*hits) != 0 {
		t.Fatal("removed operation issued requests", *hits)
	}
}
