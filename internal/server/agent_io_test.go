package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"sort"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func outputArchive(t *testing.T, pages map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(pages))
	for name := range pages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := pages[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func agentRequest(t *testing.T, f *fixture, job fixtureJob, raw, method, suffix string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+f.publicPath("/api/projects/"+job.ProjectID+"/jobs/"+job.ID+"/agent/"+suffix), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Kioku-Client-Version", "dev")
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}
func setManagedPhase(t *testing.T, f *fixture, job fixtureJob, phase model.ManagedPhase, status string) {
	t.Helper()
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		j := fixtureJobsGet(t, st, job.ID)
		if j.Run == nil {
			j.Run = &fixtureAgentRun{}
		}
		j.Run.Phase = phase
		j.Status = status
		fixtureJobsPut(t, st, j.ID, j)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestAgentIOPhasesAndResend(t *testing.T) {
	t.Parallel()
	f, raw, job, _, human := jobTokenFixture(t)
	input := []byte("saved input archive")
	if err := f.s.Blobs.Put(context.Background(), fixtureJobKey(job, "input.tar.gz"), input); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []model.ManagedPhase{model.PhasePreparing, model.PhaseAgentRunning, model.PhaseOutputReceived} {
		setManagedPhase(t, f, job, phase, "running")
		if code, b := agentRequest(t, f, job, raw, "GET", "input", nil); code != 200 || !bytes.Equal(b, input) {
			t.Fatal(code, string(b))
		}
		body := outputArchive(t, map[string]string{"wiki/index.md": "# " + string(phase)})
		if code, b := agentRequest(t, f, job, raw, "PUT", "output", body); code != 200 {
			t.Fatal(code, string(b))
		}
		_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
			j := fixtureJobsGet(t, st, job.ID)
			if j.Run.Phase != model.PhaseOutputReceived || j.Run.OutputHash != auth.Hash(string(body)) {
				t.Error(j.Run)
			}
			return nil
		})
	}
	keys, err := f.s.Blobs.List(context.Background(), fixtureJobPrefix(job))
	if err != nil || len(keys) != 3 {
		t.Fatal(keys, err)
	}
	for _, status := range []string{"running", "completed", "failed"} {
		setManagedPhase(t, f, job, model.PhaseFinalizing, status)
		for _, method := range []string{"GET", "PUT"} {
			suffix := "input"
			if method == "PUT" {
				suffix = "output"
			}
			if code, b := agentRequest(t, f, job, raw, method, suffix, outputArchive(t, map[string]string{"wiki/index.md": "x"})); code != map[string]int{"running": 409, "completed": 403, "failed": 403}[status] {
				t.Fatal(status, method, code, string(b))
			}
		}
	}
	setManagedPhase(t, f, job, model.PhasePreparing, "running")
	for _, bearer := range []string{human, ""} {
		if c, _ := agentRequest(t, f, job, bearer, "GET", "input", nil); c != 403 {
			t.Fatal(c)
		}
	}
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		j, err := st.Job(job.ID)
		if err != nil {
			return err
		}
		j.Data.Run.AbortReason = store.NullableString("revoked")
		return st.PutJob(j)
	}); err != nil {
		t.Fatal(err)
	}
	if c, _ := agentRequest(t, f, job, raw, "GET", "input", nil); c != 403 {
		t.Fatal(c)
	}
}
func TestAgentOutputRejectsBeforeStorage(t *testing.T) {
	t.Parallel()
	f, raw, job, _, _ := jobTokenFixture(t)
	f.s.Config.MaxWikiBytes = 4096
	setManagedPhase(t, f, job, model.PhasePreparing, "running")
	for _, pages := range []map[string]string{{"wiki/index.md": "ok", "wiki/../../x.md": "bad"}, {"wiki/index.md": "ok", "wiki/source-x.md": "bad"}, {"wiki/a.md": "no index"}, {"wiki/index.md": string([]byte{0xff})}} {
		if code, b := agentRequest(t, f, job, raw, "PUT", "output", outputArchive(t, pages)); code != 400 || !bytes.Contains(b, []byte(`"problems"`)) {
			t.Fatal(code, string(b))
		}
	}
	if code, b := agentRequest(t, f, job, raw, "PUT", "output", make([]byte, 8193)); code != 400 {
		t.Fatal(code, string(b))
	}
	keys, err := f.s.Blobs.List(context.Background(), fixtureJobPrefix(job))
	if err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureJobsGet(t, st, job.ID).Run.Phase != model.PhasePreparing {
			t.Error("invalid output advanced phase")
		}
		return nil
	})
}
func TestAgentOutputExpandedSizeLimit(t *testing.T) {
	t.Parallel()
	f, raw, job, _, _ := jobTokenFixture(t)
	f.s.Config.MaxWikiBytes = 4096
	setManagedPhase(t, f, job, model.PhasePreparing, "running")
	code, b := agentRequest(t, f, job, raw, "PUT", "output", outputArchive(t, map[string]string{"wiki/index.md": string(bytes.Repeat([]byte("x"), 8193))}))
	if code != 400 || !bytes.Contains(b, []byte(`"problems"`)) {
		t.Fatal(code, string(b))
	}
	keys, err := f.s.Blobs.List(t.Context(), fixtureJobPrefix(job))
	if err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
}

func TestAgentIORejectsTerminalStatusIndependently(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			f, raw, job, _, _ := jobTokenFixture(t)
			setManagedPhase(t, f, job, model.PhaseOutputReceived, status)
			body := outputArchive(t, map[string]string{"wiki/index.md": "# Late replacement"})
			if code, b := agentRequest(t, f, job, raw, "PUT", "output", body); code != 403 {
				t.Fatalf("status=%s: got %d: %s", status, code, b)
			}
		})
	}
}
