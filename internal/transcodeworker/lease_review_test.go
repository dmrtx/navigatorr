package transcodeworker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// A runner that never confirms the freeze: the lease is rolled back (nothing
// held, nothing recorded, the server's build cancelled) and the socket says 503.
func TestLeaseFreezeTimeoutRollsBack(t *testing.T) {
	old := leaseFreezeWait
	leaseFreezeWait = 300 * time.Millisecond
	defer func() { leaseFreezeWait = old }()
	w, _, _ := newPR3Worker(t, 2, nil)
	w.cfg.BuildNode = "nodeA"
	seedRunningJob(t, w, "job1")
	rep := &fakeReporter{}
	w.leases.reporter = rep

	if _, err := w.LeaseCreate(leaseReq("compile")); !errors.Is(err, errLeaseFreeze) {
		t.Fatalf("create: %v", err)
	}
	if f, _ := w.readLeaseFile(); len(f.Leases) != 0 {
		t.Fatalf("a rolled back lease leaves no record: %+v", f.Leases)
	}
	if w.queueHeld() || w.leaseHolds() {
		t.Fatal("a rolled back lease holds nothing")
	}
	if len(rep.finished) != 1 {
		t.Fatalf("the server's build must be ended: %v", rep.finished)
	}
	for _, status := range rep.finished {
		if status != "cancelled" {
			t.Fatalf("server status = %s", status)
		}
	}
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

// Whatever fails after the server knows the build, the server is told it ended.
func TestLeaseGrantFailureEndsTheServerBuild(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, nil)
	if err := os.MkdirAll(w.cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rep := &fakeReporter{}
	rep.onRegister = func() { _ = os.Chmod(w.cfg.StateDir, 0o500) }
	w.leases.reporter = rep
	defer func() { _ = os.Chmod(w.cfg.StateDir, 0o755) }()
	if _, err := w.LeaseCreate(leaseReq("compile")); err == nil {
		t.Skip("the state directory stayed writable (running as root?)")
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.registers) != 1 || len(rep.finished) != 1 {
		t.Fatalf("registers=%v finished=%v", rep.registers, rep.finished)
	}
	for _, status := range rep.finished {
		if status != "cancelled" {
			t.Fatalf("server status = %s", status)
		}
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

// A build the server does not know any more (404 on a heartbeat) is over.
func TestServerNotFoundEndsTheLease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, `{"error":"build not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := newHTTPLeaseReporter(srv.URL, tokenFile, "nodeA")
	if ended, err := rep.Heartbeat("bld_x", nil, 60, nil); err != nil || ended != leaseStatusCancelled {
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
