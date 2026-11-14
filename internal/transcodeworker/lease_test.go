package transcodeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func leaseReq(title string) LeaseRequest {
	return LeaseRequest{Source: "tester", Title: title, TTLSeconds: 60}
}

// A runner with a counting child, following the freeze the worker reads.
func startFrozenWatcher(t *testing.T, w *Worker) (ctl *suspendControl, file string, stop func()) {
	t.Helper()
	old := suspendPollInterval
	suspendPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { suspendPollInterval = old })
	runCtx, cancel := context.WithCancel(context.Background())
	ctl = newSuspendControl()
	w.watchSuspend(runCtx, ctl)
	ctx := withSuspendControl(runCtx, ctl)
	file = filepath.Join(t.TempDir(), "count")
	cmd := counterCmd(ctx, file)
	done := make(chan error, 1)
	go func() { done <- runCharged(ctx, cmd) }()
	waitFor(t, "the child to count", func() bool { return readCount(t, file) > 3 })
	return ctl, file, func() {
		_ = cmd.Process.Kill()
		<-done
		cancel()
	}
}

// A normal lease freezes the encoder and holds the queue; finishing it thaws
// both, and an operator suspend that is also on keeps the freeze.
func TestLeaseFreezesTheEncoderAndHoldsTheQueueUntilFinished(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newLeaseWorker(t, 2, stub)
	ctl, file, stop := startFrozenWatcher(t, w)
	defer stop()

	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil || l.Status != leaseStatusRunning {
		t.Fatalf("create: %+v %v", l, err)
	}
	waitFor(t, "the runner to freeze", ctl.isFrozen)
	time.Sleep(100 * time.Millisecond)
	frozenFor(t, file)
	if !w.queueHeld() || w.Suspended() {
		t.Fatalf("a lease holds the queue without being the server's suspend: held=%v suspended=%v", w.queueHeld(), w.Suspended())
	}
	if started, err := w.scheduleQueuedLocked(context.Background(), "self", ""); err != nil || started != 0 {
		t.Fatalf("a lease let %d jobs start (%v)", started, err)
	}

	// The server's own suspend comes and goes under the lease: still frozen.
	if err := w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	if err := w.SetSuspend(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	frozenFor(t, file)

	if _, err := w.LeaseFinish(l.ID, leaseStatusSucceeded, ptr(0)); err != nil {
		t.Fatal(err)
	}
	at := readCount(t, file)
	waitFor(t, "the encoder to thaw", func() bool { return readCount(t, file) > at+3 })
	if w.queueHeld() {
		t.Fatal("the queue must go on after the lease")
	}
	// Finishing twice changes nothing.
	if got, err := w.LeaseFinish(l.ID, leaseStatusFailed, ptr(1)); err != nil || got.Status != leaseStatusSucceeded {
		t.Fatalf("second finish: %+v %v", got, err)
	}
}

func ptr[T any](v T) *T { return &v }

// A lease whose heartbeats stop expires by itself: the encoder thaws with no
// tick, no socket and no server (the runners read the expiry from the record),
// and the worker then marks it lost.
func TestLeaseExpiryThawsTheEncoder(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	ctl, file, stop := startFrozenWatcher(t, w)
	defer stop()

	req := leaseReq("compile")
	req.TTLSeconds = 1
	l, err := w.LeaseCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the runner to freeze", ctl.isFrozen)
	// Heartbeats keep it alive well past one ttl.
	for i := 0; i < 4; i++ {
		time.Sleep(400 * time.Millisecond)
		if _, err := w.LeaseHeartbeat(l.ID, nil, 0, nil); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	frozenFor(t, file)

	// They stop.
	at := readCount(t, file)
	waitFor(t, "the encoder to thaw after the expiry", func() bool { return readCount(t, file) > at+3 })
	if w.queueHeld() {
		t.Fatal("an expired lease must not hold the queue")
	}
	// The tick marks it lost, and a late heartbeat is told it is over.
	w.leaseTick()
	got, err := w.LeaseHeartbeat(l.ID, nil, 0, nil)
	if !errors.Is(err, errLeaseEnded) || got.Status != leaseStatusLost {
		t.Fatalf("late heartbeat: %+v %v", got, err)
	}
}

// The worker restarting in the middle of a lease changes nothing for the
// build: the freeze is in the record, the lease answers heartbeats on the new
// worker, and it still expires there.
func TestLeaseSurvivesAWorkerRestart(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	req := leaseReq("compile")
	req.TTLSeconds = 2
	l, err := w.LeaseCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	on, gen := w.freezeState()
	if !on || gen <= unreadableSuspendGeneration {
		t.Fatalf("freeze = %v %d", on, gen)
	}

	restarted := NewWorker(w.cfg)
	restarted.leaseRestart()
	if on2, gen2 := restarted.freezeState(); !on2 || gen2 != gen {
		t.Fatalf("freeze after restart = %v %d, want the same generation %d", on2, gen2, gen)
	}
	if !restarted.queueHeld() {
		t.Fatal("the restarted worker must hold the queue")
	}
	if got, err := restarted.LeaseHeartbeat(l.ID, ptr("still going"), 0, []string{"line"}); err != nil || got.Status != leaseStatusRunning || got.Progress != "still going" {
		t.Fatalf("heartbeat on the restarted worker: %+v %v", got, err)
	}
	if _, err := restarted.LeaseFinish(l.ID, leaseStatusFailed, ptr(3)); err != nil {
		t.Fatal(err)
	}
	if restarted.queueHeld() {
		t.Fatal("finish on the restarted worker must release")
	}
	if on, _ := w.freezeState(); on {
		t.Fatal("the first worker object reads the same record: nothing frozen")
	}
}

// A lease that expired while the worker was down is not revived by the restart.
func TestLeaseRestartDoesNotReviveAnExpiredLease(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	req := leaseReq("compile")
	req.TTLSeconds = 1
	l, err := w.LeaseCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	restarted := NewWorker(w.cfg)
	restarted.leaseRestart()
	if on, _ := restarted.freezeState(); on || restarted.queueHeld() {
		t.Fatal("an expired lease must stay expired across a restart")
	}
	if _, err := restarted.LeaseHeartbeat(l.ID, nil, 0, nil); !errors.Is(err, errLeaseEnded) {
		t.Fatalf("heartbeat of the dead lease: %v", err)
	}
}

// A low lease waits for an idle node, freezes nothing, and holds the queue once
// it runs. A second normal lease coexists; the freeze follows the normal one.
func TestLowLeaseWaitsForIdleAndNeverFreezes(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newLeaseWorker(t, 2, stub)
	seed := &JobRecord{ID: "job-running", Status: "running", PID: stub.occupy("job-running"), Source: sourceFile, Candidate: filepath.Join(tempDir, "out.mkv"), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, "job-running", "job.json"), seed); err != nil {
		t.Fatal(err)
	}

	req := leaseReq("warm cache")
	req.Priority = "low"
	l, err := w.LeaseCreate(req)
	if err != nil || l.Status != leaseStatusWaiting {
		t.Fatalf("low create: %+v %v", l, err)
	}
	w.leaseTick()
	if got, _ := w.LeaseGet(l.ID); got.Status != leaseStatusWaiting {
		t.Fatalf("a busy node must not start a low lease: %s", got.Status)
	}
	if w.leaseHolds() {
		t.Fatal("a waiting lease holds nothing")
	}

	stub.kill("job-running")
	w.leaseTick()
	got, err := w.LeaseHeartbeat(l.ID, nil, 0, nil)
	if err != nil || got.Status != leaseStatusRunning || got.PreemptMode != "graceful" {
		t.Fatalf("idle node: %+v %v", got, err)
	}
	if !w.leaseHolds() {
		t.Fatal("a running low lease holds the queue")
	}
	if on, _ := w.freezeState(); on {
		t.Fatal("a low lease never freezes the encoders")
	}
	if _, err := w.LeaseFinish(l.ID, leaseStatusSucceeded, ptr(0)); err != nil {
		t.Fatal(err)
	}
}

// Without heartbeats a waiting low lease expires too, and never starts.
func TestWaitingLowLeaseExpires(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newLeaseWorker(t, 2, stub)
	seed := &JobRecord{ID: "job-running", Status: "running", PID: stub.occupy("job-running"), Source: sourceFile, Candidate: filepath.Join(tempDir, "out.mkv"), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, "job-running", "job.json"), seed); err != nil {
		t.Fatal(err)
	}
	req := leaseReq("warm cache")
	req.Priority, req.TTLSeconds = "low", 1
	l, err := w.LeaseCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	stub.kill("job-running")
	w.leaseTick()
	if got, err := w.LeaseGet(l.ID); !errors.Is(err, errLeaseEnded) || got.Status != leaseStatusLost {
		t.Fatalf("expired waiting lease: %+v %v", got, err)
	}
	if w.leaseHolds() {
		t.Fatal("it never ran")
	}
}

type fakeReporter struct {
	mu        sync.Mutex
	paused    bool
	down      bool
	cancelled bool
	gone      bool // the next heartbeat finds no such build (a 404)
	nextID    int
	registers []string
	// onRegister runs first in Register, outside the lock.
	onRegister func()
	beats      []fakeBeat
	finished   map[string]string
}

type fakeBeat struct {
	id       string
	progress string
	log      []string
	ttl      int
}

func (f *fakeReporter) Register(l *leaseRecord) (string, error) {
	if f.onRegister != nil {
		f.onRegister()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return "", errors.New("the server is unreachable")
	}
	if f.paused {
		return "", errLeasesPaused
	}
	f.nextID++
	id := "bld_srv" + string(rune('a'+f.nextID))
	f.registers = append(f.registers, l.Title+"/"+l.PreemptMode)
	return id, nil
}

func (f *fakeReporter) Heartbeat(id string, progress *string, ttl int, lines []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return "", errors.New("the server is unreachable")
	}
	b := fakeBeat{id: id, log: lines, ttl: ttl}
	if progress != nil {
		b.progress = *progress
	}
	f.beats = append(f.beats, b)
	if f.gone {
		f.gone = false
		return "ended", nil
	}
	if f.cancelled {
		return "cancelled", nil
	}
	return "", nil
}

func (f *fakeReporter) Finish(id, status string, exit *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("the server is unreachable")
	}
	if f.finished == nil {
		f.finished = map[string]string{}
	}
	f.finished[id] = status
	return nil
}

// Pause builds is decided by the server and refused before anything is held;
// an unreachable server never stops a build.
func TestLeasePausedRefusesAndUnreachableDoesNot(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{paused: true}
	w.leases.reporter = rep
	if _, err := w.LeaseCreate(leaseReq("compile")); !errors.Is(err, errLeasesPaused) {
		t.Fatalf("paused: %v", err)
	}
	if w.leaseHolds() || w.queueHeld() {
		t.Fatal("a refused lease holds nothing")
	}
	f, _ := w.readLeaseFile()
	if len(f.Leases) != 0 {
		t.Fatalf("a refused lease leaves no record: %+v", f.Leases)
	}

	rep.paused, rep.down = false, true
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil || l.Status != leaseStatusRunning || l.ServerID != "" {
		t.Fatalf("server down: %+v %v", l, err)
	}
	// Back up: the background pass registers it and then forwards.
	rep.mu.Lock()
	rep.down = false
	rep.mu.Unlock()
	w.leases.pending = nil
	w.leaseReportPass()
	got, _ := w.LeaseGet(l.ID)
	if got.ServerID == "" {
		t.Fatalf("the late registration did not happen: %+v", got)
	}
}

// The server learns the lease: registered at create (mode and title), the
// progress and output forwarded, the end finished. A lost lease is finished as
// failed. A cancel in the server ends the lease and the build is told on its
// next heartbeat.
func TestLeaseIsReportedToTheServer(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	req := leaseReq("compile")
	req.PreemptMode = "graceful"
	l, err := w.LeaseCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.registers) != 1 || rep.registers[0] != "compile/graceful" || l.ServerID == "" {
		t.Fatalf("registration: %v %+v", rep.registers, l)
	}
	if _, err := w.LeaseHeartbeat(l.ID, ptr("step 2"), 0, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	w.leaseReportPass()
	if len(rep.beats) != 1 || rep.beats[0].id != l.ServerID || rep.beats[0].progress != "step 2" || len(rep.beats[0].log) != 2 {
		t.Fatalf("forwarded: %+v", rep.beats)
	}
	if _, err := w.LeaseFinish(l.ID, leaseStatusFailed, ptr(7)); err != nil {
		t.Fatal(err)
	}
	w.leaseReportPass()
	if rep.finished[l.ServerID] != "failed" {
		t.Fatalf("finish: %v", rep.finished)
	}
	// A report that failed is retried, and only then is it settled.
	l2, _ := w.LeaseCreate(leaseReq("second"))
	rep.mu.Lock()
	rep.down = true
	rep.mu.Unlock()
	_, _ = w.LeaseFinish(l2.ID, leaseStatusSucceeded, ptr(0))
	w.leaseReportPass()
	if _, ok := rep.finished[l2.ServerID]; ok {
		t.Fatal("finish cannot have reached a server that is down")
	}
	rep.mu.Lock()
	rep.down = false
	rep.mu.Unlock()
	w.leaseReportPass()
	if rep.finished[l2.ServerID] != "succeeded" {
		t.Fatalf("finish not retried: %v", rep.finished)
	}
}

func TestLeaseCancelledInTheServerEndsTheLease(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.LeaseHeartbeat(l.ID, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	rep.mu.Lock()
	rep.cancelled = true
	rep.mu.Unlock()
	w.leaseReportPass()
	if w.leaseHolds() {
		t.Fatal("a build cancelled in the server must release the node")
	}
	got, err := w.LeaseHeartbeat(l.ID, nil, 0, nil)
	if !errors.Is(err, errLeaseEnded) || got.Status != leaseStatusCancelled {
		t.Fatalf("the build must learn it was cancelled: %+v %v", got, err)
	}
	w.leaseReportPass()
	if _, again := rep.finished[l.ServerID]; again {
		t.Fatal("the server cancelled it: nothing more to finish")
	}
}

func TestLeaseRejectsBadRequests(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	for name, req := range map[string]LeaseRequest{
		"no source":  {Title: "t"},
		"no title":   {Source: "s"},
		"bad mode":   {Source: "s", Title: "t", PreemptMode: "nuke"},
		"bad prio":   {Source: "s", Title: "t", Priority: "urgent"},
		"multi line": {Source: "s", Title: "a\nb"},
		"negative":   {Source: "s", Title: "t", TTLSeconds: -1},
	} {
		if _, err := w.LeaseCreate(req); err == nil {
			t.Errorf("%s: accepted", name)
		} else if _, ok := err.(*leaseInvalidError); !ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	if w.leaseHolds() {
		t.Fatal("nothing was granted")
	}
}

func leaseCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// The socket speaks the shape of the server's builds API, including the codes
// nbuild acts on.
func TestLeaseHTTPSpeaksTheBuildsAPI(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	w.cfg.BuildNode = "nodeA"
	h := w.leaseHandler()

	if code, out := leaseCall(t, h, "GET", "/v1/node", ""); code != 200 || out["node"] != "nodeA" {
		t.Fatalf("node: %d %v", code, out)
	}
	if code, out := leaseCall(t, h, "POST", "/api/maintenance/builds", `{"target":"other","source":"s","title":"t"}`); code != 409 || out["code"] != "wrong_node" {
		t.Fatalf("wrong node: %d %v", code, out)
	}
	if code, _ := leaseCall(t, h, "POST", "/api/maintenance/builds", `{"target":"nodeA"}`); code != 400 {
		t.Fatalf("invalid: %d", code)
	}
	code, out := leaseCall(t, h, "POST", "/api/maintenance/builds", `{"target":"nodeA","source":"s","title":"t","ttl_seconds":60}`)
	id, _ := out["id"].(string)
	if code != 200 || out["status"] != "running" || out["holds_transcode_node"] != true || len(id) < 5 || id[:4] != "bld_" {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out := leaseCall(t, h, "POST", "/api/maintenance/builds/"+id+"/heartbeat", `{"progress":"half","log":["x"]}`); code != 200 || out["progress"] != "half" {
		t.Fatalf("heartbeat: %d %v", code, out)
	}
	if code, _ := leaseCall(t, h, "POST", "/api/maintenance/builds/"+id+"/heartbeat", `{"log":[1]}`); code != 400 {
		t.Fatalf("bad log: %d", code)
	}
	if code, _ := leaseCall(t, h, "POST", "/api/maintenance/builds/bld_none/heartbeat", `{}`); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code, out := leaseCall(t, h, "POST", "/api/maintenance/builds/"+id+"/finish", `{"status":"failed","exit_code":2}`); code != 200 || out["status"] != "failed" {
		t.Fatalf("finish: %d %v", code, out)
	}
	code, out = leaseCall(t, h, "POST", "/api/maintenance/builds/"+id+"/heartbeat", `{}`)
	build, _ := out["build"].(map[string]any)
	if code != 409 || build["status"] != "failed" {
		t.Fatalf("heartbeat after the end: %d %v", code, out)
	}

	w.leases.reporter = &fakeReporter{paused: true}
	if code, out := leaseCall(t, h, "POST", "/api/maintenance/builds", `{"target":"nodeA","source":"s","title":"t"}`); code != 409 || out["code"] != "builds_paused" {
		t.Fatalf("paused: %d %v", code, out)
	}
}

// The real socket: private directory and socket, a stale socket from a dead
// worker is replaced, a live one is respected, and it answers over HTTP.
func TestLeaseSocketIsPrivateAndServes(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nbs")
	if err != nil {
		t.Skip("no /tmp")
	}
	defer os.RemoveAll(dir)
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	w.cfg.BuildNode = "nodeA"
	w.cfg.BuildSocket = filepath.Join(dir, "sub", "l.sock")

	// A stale socket file is cleaned up.
	if err := os.MkdirAll(filepath.Dir(w.cfg.BuildSocket), 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", w.cfg.BuildSocket)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.StartBuildLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(w.cfg.BuildSocket); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(w.cfg.BuildSocket)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("a directory the operator chose is not changed: %v %v", fi, err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", w.cfg.BuildSocket)
	}}}
	resp, err := client.Get("http://worker/v1/node")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["node"] != "nodeA" {
		t.Fatalf("node answer: %v", out)
	}
	// A second worker on the same socket refuses.
	if err := NewWorker(w.cfg).StartBuildLeases(ctx); err == nil {
		t.Fatal("a live socket must not be taken over")
	}
	// Without a build_node there is no socket at all.
	off := NewWorker(&WorkerConfig{StateDir: t.TempDir()})
	if off.LeaseSocketPath() != "" || off.StartBuildLeases(ctx) != nil {
		t.Fatal("no build_node, no leases")
	}
}
