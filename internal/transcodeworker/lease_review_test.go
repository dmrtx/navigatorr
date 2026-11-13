package transcodeworker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedRunningJob puts a job the worker believes is running (and alive) on disk.
func seedRunningJob(t *testing.T, w *Worker, id string) (jobDir, jobFile string) {
	t.Helper()
	jobDir = filepath.Join(w.cfg.StateDir, id)
	jobFile = filepath.Join(jobDir, "job.json")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveJobAtomic(jobFile, &JobRecord{ID: id, Status: "running", PID: os.Getpid(), DurationSec: 100}); err != nil {
		t.Fatal(err)
	}
	w.isAlive = func(*JobRecord) bool { return true }
	return jobDir, jobFile
}

// A normal lease is answered "running" only once the runner of a running
// encode reported itself frozen: when the build starts, nothing encodes.
func TestLeaseIsAnsweredOnlyOnceTheRunnersFroze(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, nil)
	jobDir, jobFile := seedRunningJob(t, w, "job1")
	ctl, file, stop := startFrozenWatcher(t, w)
	defer stop()

	quit := make(chan struct{})
	defer close(quit)
	go func() {
		for {
			select {
			case <-quit:
				return
			case <-time.After(20 * time.Millisecond):
				_ = w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC())
			}
		}
	}()

	if _, err := w.LeaseCreate(leaseReq("compile")); err != nil {
		t.Fatal(err)
	}
	if !ctl.isFrozen() {
		t.Fatal("the lease was answered before the encoder froze")
	}
	if _, frozen, _ := w.suspendCounts(); frozen != 1 {
		t.Fatalf("frozen = %d", frozen)
	}
	time.Sleep(60 * time.Millisecond)
	frozenFor(t, file)
}

// A runner that never confirms the freeze: the lease ends at once (nothing
// held, the socket says 503, no server call on that path), stays on record
// ended, and the report pass finishes the server's build, retrying a Finish
// that failed. Asking again with the same id gets the same refusal.
func TestLeaseFreezeTimeoutEndsTheLeaseAndTheReporterFinishesIt(t *testing.T) {
	old := leaseFreezeWait
	leaseFreezeWait = 300 * time.Millisecond
	defer func() { leaseFreezeWait = old }()
	w, _, _ := newPR3Worker(t, 2, nil)
	w.cfg.BuildNode = "nodeA"
	seedRunningJob(t, w, "job1")
	rep := &fakeReporter{}
	w.leases.reporter = rep

	req := leaseReq("compile")
	req.ID = "bld_cfreeze0001"
	if _, err := w.LeaseCreate(req); !errors.Is(err, errLeaseFreeze) {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.LeaseCreate(req); !errors.Is(err, errLeaseFreeze) {
		t.Fatalf("asking again: %v", err)
	}
	f, _ := w.readLeaseFile()
	if len(f.Leases) != 1 || f.Leases[0].Status != leaseStatusCancelled || f.Leases[0].ServerID == "" {
		t.Fatalf("the ended lease stays for the reporter: %+v", f.Leases)
	}
	if w.queueHeld() || w.leaseHolds() {
		t.Fatal("an ended lease holds nothing")
	}
	if len(rep.finished) != 0 {
		t.Fatal("the refusal makes no server call")
	}
	rep.mu.Lock()
	rep.down = true
	rep.mu.Unlock()
	w.leaseReportPass()
	rep.mu.Lock()
	rep.down = false
	rep.mu.Unlock()
	w.leaseReportPass()
	if len(rep.finished) != 1 {
		t.Fatalf("finished: %v", rep.finished)
	}
	for _, status := range rep.finished {
		if status != "cancelled" {
			t.Fatalf("server status = %s", status)
		}
	}
	w.cfg.BuildNode = "nodeA"
	if code, _ := leaseCall(t, w.leaseHandler(), "POST", "/api/maintenance/builds", `{"target":"nodeA","source":"s","title":"t"}`); code != 503 {
		t.Fatalf("socket answer = %d, want 503", code)
	}
}

// An unreadable or empty record freezes the node (fail closed) only until the
// tick: it heals the record and the encoders thaw.
func TestLeaseTickHealsAnUnreadableRecord(t *testing.T) {
	for name, content := range map[string]string{"corrupt": "{not json", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			w, _, _ := newPR3Worker(t, 2, nil)
			w.cfg.BuildNode = "nodeA"
			ctl, _, stop := startFrozenWatcher(t, w)
			defer stop()
			if err := os.MkdirAll(w.cfg.StateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(w.leasePath(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the runner to freeze on the unreadable record", ctl.isFrozen)
			if !w.queueHeld() {
				t.Fatal("an unreadable record holds the queue")
			}
			w.leaseTick()
			waitFor(t, "the encoder to thaw after the heal", func() bool { return !ctl.isFrozen() })
			if w.queueHeld() {
				t.Fatal("the healed record must release the queue")
			}
		})
	}
}

// A lease past its expiry is lost for whoever finds it first, not only for the
// tick: asking, or finishing "succeeded", does not revive it.
func TestLeaseExpiredIsLostOnReadAndOnFinish(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	var skew time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(skew) }

	a, err := w.LeaseCreate(leaseReq("one"))
	if err != nil {
		t.Fatal(err)
	}
	skew = 2 * time.Minute
	if got, err := w.LeaseGet(a.ID); !errors.Is(err, errLeaseEnded) || got.Status != leaseStatusLost {
		t.Fatalf("get: %+v %v", got, err)
	}
	if f, _ := w.readLeaseFile(); len(f.Leases) != 1 || f.Leases[0].Status != leaseStatusLost {
		t.Fatalf("the read must record it as lost: %+v", f.Leases)
	}

	skew = 0
	b, err := w.LeaseCreate(leaseReq("two"))
	if err != nil {
		t.Fatal(err)
	}
	skew = 2 * time.Minute
	got, err := w.LeaseFinish(b.ID, leaseStatusSucceeded, ptr(0))
	if err != nil || got.Status != leaseStatusLost {
		t.Fatalf("a finish after the expiry cannot claim success: %+v %v", got, err)
	}
	if w.leaseHolds() {
		t.Fatal("nothing holds the node")
	}
}

// Asking again with the same client id returns the same lease: one record, one
// server build.
func TestLeaseCreateWithAClientIDIsIdempotent(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	req := leaseReq("compile")
	req.ID = "bld_c0123456789abcdef"
	a, err := w.LeaseCreate(req)
	if err != nil || a.ID != req.ID {
		t.Fatalf("first: %+v %v", a, err)
	}
	b, err := w.LeaseCreate(req)
	if err != nil || b.ID != a.ID || b.ServerID != a.ServerID {
		t.Fatalf("second: %+v %v", b, err)
	}
	if f, _ := w.readLeaseFile(); len(f.Leases) != 1 || len(rep.registers) != 1 {
		t.Fatalf("leases=%d registers=%v", len(f.Leases), rep.registers)
	}
	req.ID = "nope"
	if _, err := w.LeaseCreate(req); err == nil {
		t.Fatal("a client id must look like bld_...")
	}
}

// Ended leases leave the record an hour after the server has the end (or at
// once without a server), a day after when the server cannot be reached.
func TestEndedLeasesArePruned(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	var skew time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(skew) }
	rep := &fakeReporter{}
	w.leases.reporter = rep

	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.LeaseFinish(l.ID, leaseStatusSucceeded, ptr(0)); err != nil {
		t.Fatal(err)
	}
	skew = 2 * time.Hour
	w.leaseTick()
	if f, _ := w.readLeaseFile(); len(f.Leases) != 1 {
		t.Fatal("an end the server has not heard stays")
	}
	w.leaseMarkReported(l.ID)
	w.leaseTick()
	if f, _ := w.readLeaseFile(); len(f.Leases) != 0 {
		t.Fatalf("a reported end goes after an hour: %+v", f.Leases)
	}
	if _, ok := w.leases.pending[l.ID]; ok {
		t.Fatal("its pending report goes with it")
	}

	skew = 0
	m, _ := w.LeaseCreate(leaseReq("again"))
	_, _ = w.LeaseFinish(m.ID, leaseStatusFailed, ptr(1))
	skew = 25 * time.Hour
	w.leaseTick()
	if f, _ := w.readLeaseFile(); len(f.Leases) != 0 {
		t.Fatal("a report that never got through is given up after a day")
	}
}

// The server hears the lease's ttl, and an idle lease is kept alive well
// inside it.
func TestLeaseForwardsItsTTL(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.LeaseHeartbeat(l.ID, ptr("x"), 0, nil); err != nil {
		t.Fatal(err)
	}
	w.leaseReportPass()
	if len(rep.beats) != 1 || rep.beats[0].ttl != 60 {
		t.Fatalf("beats: %+v", rep.beats)
	}
	if got := leaseKeepaliveEvery(&leaseRecord{TTLSeconds: 3}); got != time.Second {
		t.Fatalf("short ttl keepalive = %v", got)
	}
	if got := leaseKeepaliveEvery(&leaseRecord{TTLSeconds: 600}); got != leaseKeepaliveGap {
		t.Fatalf("long ttl keepalive = %v", got)
	}
}

// A build the server does not know any more (404 on a heartbeat) is not the
// end of the lease: the reporter says "ended" and the lease registers again.
func TestServerNotFoundIsNotAnEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, `{"error":"build not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := newHTTPLeaseReporter(srv.URL, tokenFile, "nodeA")
	if ended, err := rep.Heartbeat("bld_x", nil, 60, nil); err != nil || ended != "ended" {
		t.Fatalf("heartbeat: %q %v", ended, err)
	}
}

// A socket directory the operator chose is theirs: it is never changed, and
// one others can write to is refused. The worker's own directory is private.
func TestLeaseSocketDirectories(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nbs")
	if err != nil {
		t.Skip("no /tmp")
	}
	defer os.RemoveAll(dir)
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWorker(&WorkerConfig{StateDir: t.TempDir(), BuildNode: "nodeA", BuildSocket: filepath.Join(open, "l.sock")})
	if err := w.StartBuildLeases(ctx); err == nil {
		t.Fatal("a world-writable socket directory must be refused")
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o777 {
		t.Fatalf("the directory must be left alone: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(open, "l.sock")); err == nil {
		t.Fatal("no socket in a refused directory")
	}

	own := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "st"), BuildNode: "nodeA"})
	if err := own.StartBuildLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(own.LeaseSocketPath())); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the worker's own directory is private: %v %v", fi, err)
	}
	if c, err := net.DialTimeout("unix", own.LeaseSocketPath(), time.Second); err != nil {
		t.Fatal(err)
	} else {
		_ = c.Close()
	}
}

// Two creates of one id at once make one lease and one server build.
func TestConcurrentCreatesOfOneIDMakeOneLease(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	rep := &fakeReporter{onRegister: func() { time.Sleep(100 * time.Millisecond) }}
	w.leases.reporter = rep
	req := leaseReq("compile")
	req.ID = "bld_cconcurrent1"
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, err := w.LeaseCreate(req); err != nil || l.ID != req.ID || l.Status != leaseStatusRunning {
				t.Errorf("create: %+v %v", l, err)
			}
		}()
	}
	wg.Wait()
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if f, _ := w.readLeaseFile(); len(f.Leases) != 1 || len(rep.registers) != 1 {
		t.Fatalf("leases=%d registers=%v", len(f.Leases), rep.registers)
	}
}

// A retry while the first create waits for the freeze is not answered
// "running" before the encoder is frozen.
func TestRetryDuringTheFreezeWaitIsNotAnsweredEarly(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, nil)
	jobDir, jobFile := seedRunningJob(t, w, "job1")
	ctl, _, stop := startFrozenWatcher(t, w)
	defer stop()
	var beating atomic.Bool
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		time.Sleep(400 * time.Millisecond)
		beating.Store(true)
		for {
			select {
			case <-quit:
				return
			case <-time.After(20 * time.Millisecond):
				_ = w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC())
			}
		}
	}()
	req := leaseReq("compile")
	req.ID = "bld_cretry000001"
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.LeaseCreate(req); err != nil {
				t.Errorf("create: %v", err)
			}
			if !beating.Load() || !ctl.isFrozen() {
				t.Error("answered running before the encoder was frozen")
			}
		}()
		time.Sleep(100 * time.Millisecond)
	}
	wg.Wait()
}

// A ttl shorter than the freeze wait is not spent waiting for the freeze.
func TestShortTTLSurvivesTheFreezeWait(t *testing.T) {
	old := leaseFreezeWait
	leaseFreezeWait = 3 * time.Second
	defer func() { leaseFreezeWait = old }()
	w, _, _ := newPR3Worker(t, 2, nil)
	jobDir, jobFile := seedRunningJob(t, w, "job1")
	_, _, stop := startFrozenWatcher(t, w)
	defer stop()
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		for {
			select {
			case <-quit:
				return
			case <-time.After(20 * time.Millisecond):
				_ = w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC())
			}
		}
	}()
	req := leaseReq("compile")
	req.TTLSeconds = 1
	l, err := w.LeaseCreate(req)
	if err != nil || l.Status != leaseStatusRunning || !l.Confirmed {
		t.Fatalf("create: %+v %v", l, err)
	}
	if _, err := w.LeaseHeartbeat(l.ID, nil, 0, nil); err != nil {
		t.Fatalf("the ttl starts at the confirmation: %v", err)
	}
}

// Only a record that is empty or not JSON is rewritten. A record that cannot be
// read (here: a directory) is left alone and fails closed.
func TestAnIOErrorIsNotHealed(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	w.cfg.BuildNode = "nodeA"
	if err := os.MkdirAll(w.leasePath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if on, _ := w.freezeState(); !on || !w.queueHeld() {
		t.Fatal("an unreadable record fails closed")
	}
	w.leaseTick()
	if fi, err := os.Stat(w.leasePath()); err != nil || !fi.IsDir() {
		t.Fatalf("nothing was rewritten: %v %v", fi, err)
	}
	if _, err := w.LeaseGet("bld_x"); err == nil || errors.Is(err, errLeaseNotFound) {
		t.Fatalf("an unreadable record is an error, not an empty one: %v", err)
	}
}

// Without leases (no build_node) a bad record is ignored: nothing would heal it.
func TestUnreadableRecordIsIgnoredWithoutLeases(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, nil)
	if err := os.MkdirAll(w.cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.leasePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, _ := w.freezeState(); on || w.queueHeld() {
		t.Fatal("a worker without leases must not freeze on the lease record")
	}
}

// The reporter's "ended" (a 404) leaves the lease running and registers it
// again.
func TestForgottenServerBuildIsRegisteredAgain(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.LeaseHeartbeat(l.ID, ptr("x"), 0, nil); err != nil {
		t.Fatal(err)
	}
	rep.mu.Lock()
	rep.gone = true
	rep.mu.Unlock()
	w.leaseReportPass()
	got, err := w.LeaseGet(l.ID)
	if err != nil || got.Status != leaseStatusRunning || got.ServerID != "" {
		t.Fatalf("after the 404: %+v %v", got, err)
	}
	w.leases.pending = nil
	w.leaseReportPass()
	if got, _ = w.LeaseGet(l.ID); got.ServerID == "" || len(rep.registers) != 2 {
		t.Fatalf("not registered again: %+v %v", got, rep.registers)
	}
}
