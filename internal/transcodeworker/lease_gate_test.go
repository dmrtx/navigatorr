package transcodeworker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func gateConfig(c *WorkerConfig) { c.BuildNode = "nodeA" }

func postEncodeWorker(t *testing.T) (*Worker, *stubSpawn, *WorkerConfig) {
	t.Helper()
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		gateConfig(c)
	})
	w := NewWorker(cfg)
	stub := newStubSpawn()
	stub.install(w)
	pr6b2SeedPostEncode(t, cfg, "job1", filepath.Join(cfg.StateDir, "_work", "job1", "candidate.mkv"), filepath.Join(extRoot, "out.mkv"))
	return w, stub, cfg
}

// A lease granted between the cheap check and the capacity lock starts nothing:
// the check is repeated under the lock leases are granted under.
func TestResumePostEncodeRechecksTheLeaseUnderTheCapacityLock(t *testing.T) {
	w, stub, cfg := postEncodeWorker(t)
	w.beforeGateLockHook = func() {
		if _, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", PreemptMode: "graceful", TTLSeconds: 60}); err != nil {
			t.Error(err)
		}
	}
	if n, err := w.ResumePostEncode(context.Background(), "self", ""); err != nil || n != 0 || stub.n() != 0 {
		t.Fatalf("started %d (%v), spawns %d", n, err, stub.n())
	}
	if job := pr6b1LoadJob(t, cfg.StateDir, "job1"); job.PID != 0 || job.FinalizationRetryCount != 0 {
		t.Fatalf("the job must stay as it was: %+v", job)
	}
}

// Only a lease stops post-encode work: a drain hold (SetHold, what a drain
// sets) or an operator suspend never did, and finishing that work is how a
// drain ends.
func TestResumePostEncodeGateIsTheLeaseOnly(t *testing.T) {
	w, stub, _ := postEncodeWorker(t)
	if err := w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if err := w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	if n, err := w.ResumePostEncode(context.Background(), "self", ""); err != nil || n != 1 || stub.n() != 1 {
		t.Fatalf("a drain and a suspend must not stop post-encode: started %d (%v)", n, err)
	}

	w2, stub2, _ := postEncodeWorker(t)
	l, err := w2.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", PreemptMode: "graceful", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := w2.ResumePostEncode(context.Background(), "self", ""); n != 0 || stub2.n() != 0 {
		t.Fatalf("a running lease must block it: %d", n)
	}
	if _, err := w2.LeaseFinish(l.ID, leaseStatusSucceeded, ptr(0)); err != nil {
		t.Fatal(err)
	}
	if n, err := w2.ResumePostEncode(context.Background(), "self", ""); err != nil || n != 1 {
		t.Fatalf("after the lease: started %d (%v)", n, err)
	}
}

// A lease granted between the early check and the capacity lock stops the
// publication request too; the candidate stays retained.
func TestPublishRetainedRechecksTheLeaseUnderTheCapacityLock(t *testing.T) {
	f := newRetainedFixture(t, gateConfig)
	f.retain("job1")
	f.w.beforeGateLockHook = func() {
		if _, err := f.w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", PreemptMode: "graceful", TTLSeconds: 60}); err != nil {
			t.Error(err)
		}
	}
	if _, err := f.w.PublishRetained(context.Background(), "job1"); !errors.Is(err, ErrPublishHeld) {
		t.Fatalf("err = %v, want ErrPublishHeld", err)
	}
	f.assertRetained("job1")
}

// A suspend alone never held publication.
func TestPublishRetainedIsNotHeldBySuspend(t *testing.T) {
	f := newRetainedFixture(t, gateConfig)
	f.retain("job1")
	if err := f.w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.PublishRetained(context.Background(), "job1"); err != nil {
		t.Fatal(err)
	}
}

// Two leases at once: the strongest mode applies (a freezing lease freezes
// while it lives), the node stays held until the last one ends, and one that is
// lost while another lives changes nothing for the survivor.
func TestMultipleLeasesStrongestModeAndReleaseAfterTheLast(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	graceful, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "one", PreemptMode: "graceful", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := w.leaseFreeze(); on || !w.leaseHolds() {
		t.Fatalf("graceful alone holds without freezing: freeze=%v holds=%v", on, w.leaseHolds())
	}
	strong, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "two", PreemptMode: "suspend", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := w.leaseFreeze(); !on {
		t.Fatal("the suspend lease freezes while the graceful one runs")
	}
	// The strong one is lost (its heartbeats stopped): the graceful one still holds.
	w.leaseUpdate(strong.ID, func(l *leaseRecord) { l.ExpiresAt = time.Now().Add(-time.Second) })
	w.leaseTick()
	if on, _ := w.leaseFreeze(); on {
		t.Fatal("the expired suspend lease must no longer freeze")
	}
	if !w.leaseHolds() {
		t.Fatal("the surviving lease still holds the node")
	}
	if _, err := w.LeaseFinish(graceful.ID, leaseStatusSucceeded, ptr(0)); err != nil {
		t.Fatal(err)
	}
	if w.leaseHolds() || w.queueHeld() {
		t.Fatal("the node is released after the last lease ends")
	}
}

// A low lease that is promoted after builds were paused keeps running; the
// refused registration is not tried again.
func TestPromotedLowLeaseRefusedAsPausedKeepsRunningAndIsNotRetried(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	req := leaseReq("warm")
	req.Priority = "low"
	l, err := w.LeaseCreate(req)
	if err != nil || l.Status != leaseStatusWaiting {
		t.Fatalf("%+v %v", l, err)
	}
	rep.mu.Lock()
	rep.paused = true
	rep.mu.Unlock()
	w.leaseTick()
	if got, _ := w.LeaseGet(l.ID); got.Status != leaseStatusRunning {
		t.Fatalf("the promoted lease runs: %+v", got)
	}
	attempts := func() int {
		rep.mu.Lock()
		defer rep.mu.Unlock()
		return rep.registerCalls
	}
	w.leaseReportPass()
	first := attempts()
	if first != 1 {
		t.Fatalf("one registration attempt, got %d", first)
	}
	for i := 0; i < 5; i++ {
		w.leaseReportPass()
	}
	if attempts() != first {
		t.Fatalf("the refused registration is retried: %d attempts", attempts())
	}
	if got, _ := w.LeaseGet(l.ID); got.Status != leaseStatusRunning {
		t.Fatalf("still running: %+v", got)
	}
}

// What the server answers to the pause check is what the worker decides on:
// paused refuses, not paused passes, an unreachable server is no refusal.
func TestHTTPLeaseReporterCheckPaused(t *testing.T) {
	paused := false
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/maintenance/builds/nodes" || r.URL.Query().Get("node") != "nodeA" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		if paused {
			_, _ = rw.Write([]byte(`{"nodes":[{"node":"nodeA","builds_paused":true,"running_builds":[],"transcode_mode":"","transcode_paused":false}]}`))
			return
		}
		_, _ = rw.Write([]byte(`{"nodes":[{"node":"nodeA","running_builds":[],"transcode_mode":"","transcode_paused":false}]}`))
	}))
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newHTTPLeaseReporter(srv.URL, tokenFile, "nodeA")
	if err := r.CheckPaused(); err != nil {
		t.Fatalf("not paused: %v", err)
	}
	paused = true
	if err := r.CheckPaused(); !errors.Is(err, errLeasesPaused) {
		t.Fatalf("paused: %v", err)
	}
	srv.Close()
	if err := r.CheckPaused(); err == nil || errors.Is(err, errLeasesPaused) {
		t.Fatalf("a server that is down is an error but not a pause: %v", err)
	}

	// A low lease with that server down is still granted.
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	w.leases.reporter = r
	req := leaseReq("warm")
	req.Priority = "low"
	if l, err := w.LeaseCreate(req); err != nil || l.Status != leaseStatusWaiting {
		t.Fatalf("server down: %+v %v", l, err)
	}
}

// A job queued behind a lease carries the hold refusal; when the lease ends the
// next pass starts it and the refusal is gone.
func TestHoldRefusalClearsWhenTheLeaseEnds(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newLeaseWorker(t, 2, stub)
	jobFile := filepath.Join(w.cfg.StateDir, "job1", "job.json")
	if err := SaveJobAtomic(jobFile, &JobRecord{ID: "job1", Status: "queued", Source: sourceFile, Candidate: filepath.Join(tempDir, "out.mkv"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ScheduleQueued(context.Background(), "self", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadJob(jobFile); got.RetentionBlocked == "" {
		t.Fatalf("the refusal is recorded: %+v", got)
	}
	if _, err := w.LeaseFinish(l.ID, leaseStatusSucceeded, ptr(0)); err != nil {
		t.Fatal(err)
	}
	if n, err := w.ScheduleQueued(context.Background(), "self", ""); err != nil || n != 1 {
		t.Fatalf("started %d (%v)", n, err)
	}
	got, _ := LoadJob(jobFile)
	if got.RetentionBlocked != "" || got.RoomNote != "" || got.PID == 0 {
		t.Fatalf("the refusal is gone and the job runs: %+v", got)
	}
}

// The drain hold (SetHold) holds publication, and does not stop post-encode.
func TestDrainHoldRefusesPublishRetainedButNotResumePostEncode(t *testing.T) {
	f := newRetainedFixture(t, gateConfig)
	f.retain("job1")
	if err := f.w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.PublishRetained(context.Background(), "job1"); !errors.Is(err, ErrPublishHeld) {
		t.Fatalf("err = %v, want ErrPublishHeld", err)
	}
	f.assertRetained("job1")

	w, stub, _ := postEncodeWorker(t)
	if err := w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if n, err := w.ResumePostEncode(context.Background(), "self", ""); err != nil || n != 1 || stub.n() != 1 {
		t.Fatalf("a drain hold must not stop post-encode: started %d (%v)", n, err)
	}
}

// Another holder of the capacity lock makes the request ask again: nothing is
// published and the candidate stays retained.
func TestPublishRetainedRefusesWhileTheCapacityLockIsBusy(t *testing.T) {
	f := newRetainedFixture(t, gateConfig)
	f.retain("job1")
	lock, err := acquireCapacityLock(filepath.Clean(f.w.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.PublishRetained(context.Background(), "job1"); !errors.Is(err, ErrPublishHeld) {
		lock.Unlock()
		t.Fatalf("err = %v, want ErrPublishHeld", err)
	}
	f.assertRetained("job1")
	lock.Unlock()
	if _, err := f.w.PublishRetained(context.Background(), "job1"); err != nil {
		t.Fatalf("once the lock is free: %v", err)
	}
}
