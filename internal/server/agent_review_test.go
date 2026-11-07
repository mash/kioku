package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestManagedIOBoundsConcurrentBodyReads(t *testing.T) {
	f, raw, j, _, _ := jobTokenFixture(t)
	setManagedPhase(t, f, j, model.PhaseAgentRunning, "running")
	first := &pausedReader{make(chan struct{}), make(chan struct{})}
	second := &pausedReader{make(chan struct{}), make(chan struct{})}
	call := func(body *pausedReader, done chan struct{}) {
		defer close(done)
		r := httptest.NewRequest("PUT", "/api/projects/"+j.ProjectID+"/jobs/"+j.ID+"/agent/output", body)
		r.Header.Set("Authorization", "Bearer "+raw)
		f.s.Handler().ServeHTTP(httptest.NewRecorder(), r)
	}
	a, b := make(chan struct{}), make(chan struct{})
	go call(first, a)
	<-first.started
	go call(second, b)
	concurrent := false
	select {
	case <-second.started:
		concurrent = true
	case <-time.After(100 * time.Millisecond):
	}
	close(first.release)
	close(second.release)
	<-a
	<-b
	if concurrent {
		t.Fatal("parallel requests buffered bodies before acquiring a per-project I/O gate")
	}
}
