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
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
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
func agentRequest(t *testing.T, f *fixture, job model.Job, raw, method, suffix string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+"/api/projects/"+job.ProjectID+"/jobs/"+job.ID+"/agent/"+suffix, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
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
func setManagedPhase(t *testing.T, f *fixture, job model.Job, phase model.ManagedPhase, status string) {
	t.Helper()
	if err := f.db.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if j.Managed == nil {
			j.Managed = &model.ManagedRun{}
		}
		j.Managed.Phase = phase
		j.Status = status
		st.Jobs[j.ID] = j
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestAgentIOPhasesAndResend(t *testing.T) {
	f, raw, job, _, human := jobTokenFixture(t)
	input := []byte("saved input archive")
	if err := f.s.Blobs.Put(context.Background(), agentJobKey(job, "input.tar.gz"), input); err != nil {
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
		_ = f.db.View(func(st model.State) error {
			j := st.Jobs[job.ID]
			if j.Managed.Phase != model.PhaseOutputReceived || j.Managed.OutputHash != auth.Hash(string(body)) {
				t.Error(j.Managed)
			}
			return nil
		})
	}
	keys, err := f.s.Blobs.List(context.Background(), agentJobPrefix(job))
	if err != nil || len(keys) != 2 {
		t.Fatal(keys, err)
	}
	for _, status := range []string{"running", "completed", "failed"} {
		setManagedPhase(t, f, job, model.PhaseFinalizing, status)
		for _, method := range []string{"GET", "PUT"} {
			suffix := "input"
			if method == "PUT" {
				suffix = "output"
			}
			if code, b := agentRequest(t, f, job, raw, method, suffix, outputArchive(t, map[string]string{"wiki/index.md": "x"})); code != 409 {
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
	if err := f.db.Update(func(st *model.State) error { revokeJobToken(st, job.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if c, _ := agentRequest(t, f, job, raw, "GET", "input", nil); c != 403 {
		t.Fatal(c)
	}
}
func TestAgentOutputRejectsBeforeStorage(t *testing.T) {
	f, raw, job, _, _ := jobTokenFixture(t)
	setManagedPhase(t, f, job, model.PhasePreparing, "running")
	for _, pages := range []map[string]string{{"wiki/index.md": "ok", "wiki/../../x.md": "bad"}, {"wiki/index.md": "ok", "wiki/source-x.md": "bad"}, {"wiki/a.md": "no index"}, {"wiki/index.md": string([]byte{0xff})}} {
		if code, b := agentRequest(t, f, job, raw, "PUT", "output", outputArchive(t, pages)); code != 400 || !bytes.Contains(b, []byte(`"problems"`)) {
			t.Fatal(code, string(b))
		}
	}
	if code, b := agentRequest(t, f, job, raw, "PUT", "output", make([]byte, (16<<20)+1)); code != 413 {
		t.Fatal(code, string(b))
	}
	keys, err := f.s.Blobs.List(context.Background(), agentJobPrefix(job))
	if err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[job.ID].Managed.Phase != model.PhasePreparing {
			t.Error("invalid output advanced phase")
		}
		return nil
	})
}
func TestAgentOutputIncludesSourcePagesInSizeLimit(t *testing.T) {
	f, raw, job, _, _ := jobTokenFixture(t)
	setManagedPhase(t, f, job, model.PhasePreparing, "running")
	src := model.Source{ID: "large", ProjectID: job.ProjectID, TextKey: blob.Key("projects/" + job.ProjectID + "/sources/large/extracted.md")}
	if err := f.s.Blobs.Put(context.Background(), src.TextKey, bytes.Repeat([]byte("x"), 64<<20)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Update(func(st *model.State) error {
		st.Sources[src.ID] = src
		j := st.Jobs[job.ID]
		j.Managed.SourceIDs = []string{src.ID}
		st.Jobs[j.ID] = j
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, b := agentRequest(t, f, job, raw, "PUT", "output", outputArchive(t, map[string]string{"wiki/index.md": "# Index"}))
	if code != 413 {
		t.Fatal(code, string(b))
	}
	keys, err := f.s.Blobs.List(context.Background(), agentJobPrefix(job))
	if err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
}

func TestAgentIORejectsTerminalStatusIndependently(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			f, raw, job, _, _ := jobTokenFixture(t)
			setManagedPhase(t, f, job, model.PhaseOutputReceived, status)
			body := outputArchive(t, map[string]string{"wiki/index.md": "# Late replacement"})
			if code, b := agentRequest(t, f, job, raw, "PUT", "output", body); code != 409 {
				t.Fatalf("status=%s: got %d: %s", status, code, b)
			}
		})
	}
}
