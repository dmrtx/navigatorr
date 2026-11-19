package transcodeworker

import (
	"fmt"
	"testing"
	"time"
)

func logLines(from, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("out %d", from+i)
	}
	return out
}

// Output reaches the server at once, at most one forward every leaseLogEvery:
// what arrives in between waits, in order, and goes with the next one.
func TestLeaseForwardsOutputPromptlyAndRateLimited(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	var offset time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(offset) }
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	beats := func() int { rep.mu.Lock(); defer rep.mu.Unlock(); return len(rep.beats) }

	if _, err := w.LeaseHeartbeat(l.ID, nil, 0, logLines(0, 256)); err != nil {
		t.Fatal(err)
	}
	w.leaseReportPass()
	if beats() != 1 || len(rep.beats[0].log) != 256 {
		t.Fatalf("the first output goes at once: %+v", rep.beats)
	}
	// Two more beats within the gap are held, not lost.
	for i := 0; i < 2; i++ {
		offset += 500 * time.Millisecond
		if _, err := w.LeaseHeartbeat(l.ID, nil, 0, logLines(256+i*10, 10)); err != nil {
			t.Fatal(err)
		}
		w.leaseReportPass()
	}
	if beats() != 1 {
		t.Fatalf("output inside the gap must wait: %d beats", beats())
	}
	offset += leaseLogEvery
	w.leaseReportPass()
	if beats() != 2 || len(rep.beats[1].log) != 20 || rep.beats[1].log[0] != "out 256" || rep.beats[1].log[19] != "out 275" {
		t.Fatalf("the held lines go together, in order: %+v", rep.beats)
	}
}

// Held output asks the reporter to come back by itself, so it does not wait for
// the next heartbeat or the slow tick.
func TestLeaseHeldOutputKicksTheReporter(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	w.leases.kick = make(chan struct{}, 1)
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.LeaseHeartbeat(l.ID, nil, 0, []string{"a"})
	w.leaseReportPass()
	_, _ = w.LeaseHeartbeat(l.ID, nil, 0, []string{"b"})
	<-w.leases.kick // the heartbeat's own kick
	w.leaseReportPass()
	rep.mu.Lock()
	n := len(rep.beats)
	rep.mu.Unlock()
	if n != 1 {
		t.Fatalf("beats: %d", n)
	}
	select {
	case <-w.leases.kick:
	case <-time.After(leaseLogEvery + time.Second):
		t.Fatal("the reporter was not asked to come back")
	}
	w.leaseReportPass()
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.beats) != 2 || len(rep.beats[1].log) != 1 || rep.beats[1].log[0] != "b" {
		t.Fatalf("%+v", rep.beats)
	}
}

// A progress-only heartbeat is never held by the output gap, and a failed
// forward puts the lines back in front of the newer ones, capped.
func TestLeaseProgressIsNotHeldAndFailedOutputIsRequeued(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	var offset time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(offset) }
	l, _ := w.LeaseCreate(leaseReq("compile"))

	_, _ = w.LeaseHeartbeat(l.ID, nil, 0, []string{"a"})
	w.leaseReportPass()
	_, _ = w.LeaseHeartbeat(l.ID, ptr("step 3"), 0, nil)
	w.leaseReportPass()
	rep.mu.Lock()
	if len(rep.beats) != 2 || rep.beats[1].progress != "step 3" {
		t.Fatalf("progress waits for nothing: %+v", rep.beats)
	}
	rep.down = true
	rep.mu.Unlock()

	offset += 3 * time.Second
	_, _ = w.LeaseHeartbeat(l.ID, nil, 0, []string{"b", "c"})
	w.leaseReportPass() // fails: the lines are kept
	offset += 3 * time.Second
	_, _ = w.LeaseHeartbeat(l.ID, nil, 0, []string{"d"})
	rep.mu.Lock()
	rep.down = false
	rep.mu.Unlock()
	w.leaseReportPass()
	rep.mu.Lock()
	last := rep.beats[len(rep.beats)-1]
	rep.mu.Unlock()
	if fmt.Sprint(last.log) != "[b c d]" {
		t.Fatalf("requeued lines go first, in order: %v", last.log)
	}

	// Never more than leaseLogMax are kept for the server.
	rep.mu.Lock()
	rep.down = true
	rep.mu.Unlock()
	for i := 0; i < 12; i++ {
		offset += 3 * time.Second
		_, _ = w.LeaseHeartbeat(l.ID, nil, 0, logLines(i*256, 256))
		w.leaseReportPass()
	}
	w.leases.mu.Lock()
	n := len(w.leases.pending[l.ID].log)
	w.leases.mu.Unlock()
	if n != leaseLogMax {
		t.Fatalf("kept %d lines, want the cap %d", n, leaseLogMax)
	}
}

func TestLeaseHeartbeatBatchLimit(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	w.leases.reporter = &fakeReporter{}
	l, _ := w.LeaseCreate(leaseReq("compile"))
	if _, err := w.LeaseHeartbeat(l.ID, nil, 0, logLines(0, 256)); err != nil {
		t.Fatalf("256 lines: %v", err)
	}
	if _, err := w.LeaseHeartbeat(l.ID, nil, 0, logLines(0, 257)); err == nil {
		t.Fatal("257 lines must be refused")
	}
}
