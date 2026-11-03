package wiki

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestAssembleBoundsGraphAmplification(t *testing.T) {
	pages := map[string]string{}
	body := "# " + strings.Repeat("<", (256<<10)-2)
	for i := 0; i < 16; i++ {
		pages[fmt.Sprintf("wiki/p%d.md", i)] = body
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	files, err := Assemble(nil, pages)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("4 MiB page payload allocated %d bytes", allocated)
	if err != nil {
		t.Fatal(err)
	}
	if allocated > 64<<20 {
		t.Errorf("graph amplified payload beyond bounded working memory: %d", allocated)
	}
	if !strings.HasPrefix(files["wiki/p0.md"], body) {
		t.Error("source page body was truncated")
	}
}
