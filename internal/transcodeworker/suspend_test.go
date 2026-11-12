package transcodeworker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// A harmless long-running child: it counts to a file, so a frozen one is seen
// to stop counting and a continued one to count again.
func counterCmd(ctx context.Context, file string) *exec.Cmd {
	script := `i=0; while :; do i=$((i+1)); echo $i > "` + file + `.tmp" && mv "` + file + `.tmp" "` + file + `"; sleep 0.02; done`
	return exec.CommandContext(ctx, "sh", "-c", script)
}

func readCount(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func procState(pid int) string {
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// frozenFor asserts the counter does not move for a while.
func frozenFor(t *testing.T, file string) {
	t.Helper()
	before := readCount(t, file)
	time.Sleep(250 * time.Millisecond)
	if after := readCount(t, file); after != before {
		t.Fatalf("child kept running while suspended: %d -> %d", before, after)
	}
}

func TestSuspendFreezesAndContinuesATrackedChild(t *testing.T) {
	ctl := newSuspendControl()
	ctx := withSuspendControl(context.Background(), ctl)
	file := filepath.Join(t.TempDir(), "count")
	cmd := counterCmd(ctx, file)
	done := make(chan error, 1)
	go func() { done <- runCharged(ctx, cmd) }()
	waitFor(t, "the child to count", func() bool { return readCount(t, file) > 3 })

	ctl.apply(true)
	time.Sleep(100 * time.Millisecond) // anything already in flight lands
	frozenFor(t, file)
	if s := procState(cmd.Process.Pid); !strings.HasPrefix(s, "T") {
		t.Fatalf("child state = %q, want stopped", s)
	}
	frozen := readCount(t, file)

	ctl.apply(false)
	waitFor(t, "the child to continue where it was", func() bool { return readCount(t, file) > frozen+3 })
	if readCount(t, file) < frozen {
		t.Fatal("the count went back: the child restarted instead of continuing")
	}

	cmd.Cancel() // ends the loop
	if err := <-done; err == nil {
		t.Fatal("a killed child must report its end")
	}
}

// A child that leads its own process group (the x265 pass, the quality
// measures) is frozen with the whole group.
func TestSuspendFreezesAChildsWholeGroup(t *testing.T) {
	ctl := newSuspendControl()
	ctx := withSuspendControl(context.Background(), ctl)
	file := filepath.Join(t.TempDir(), "count")
	cmd := counterCmd(ctx, file)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	done := make(chan error, 1)
	go func() { done <- runCharged(ctx, cmd) }()
	waitFor(t, "the child to count", func() bool { return readCount(t, file) > 3 })

	ctl.apply(true)
	time.Sleep(100 * time.Millisecond)
	frozenFor(t, file)
	ctl.apply(false)
	frozen := readCount(t, file)
	waitFor(t, "the group to continue", func() bool { return readCount(t, file) > frozen+3 })
	_ = cmd.Cancel()
	<-done
}

// Nothing starts while suspended; the child starts when the node resumes.
func TestSuspendHoldsAChildBackUntilResume(t *testing.T) {
	ctl := newSuspendControl()
	ctl.apply(true)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := withSuspendControl(parent, ctl)
	file := filepath.Join(t.TempDir(), "count")
	cmd := counterCmd(ctx, file)
	done := make(chan error, 1)
	go func() { done <- runCharged(ctx, cmd) }()
	time.Sleep(250 * time.Millisecond)
	ctl.mu.Lock()
	started := len(ctl.children)
	ctl.mu.Unlock()
	if started != 0 || readCount(t, file) != 0 {
		t.Fatal("a child started while the node was suspended")
	}
	ctl.apply(false)
	waitFor(t, "the held child to start", func() bool { return readCount(t, file) > 2 })
	cancel()
	<-done
}

// A cancelled job ends even while its child is frozen (SIGKILL reaches a
// stopped process), and one waiting to start is released by the cancel.
func TestSuspendDoesNotBlockCancellation(t *testing.T) {
	ctl := newSuspendControl()
	parent, cancel := context.WithCancel(context.Background())
	ctx := withSuspendControl(parent, ctl)
	file := filepath.Join(t.TempDir(), "count")
	cmd := counterCmd(ctx, file)
	done := make(chan error, 1)
	go func() { done <- runCharged(ctx, cmd) }()
	waitFor(t, "the child to count", func() bool { return readCount(t, file) > 3 })
	ctl.apply(true)
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled child must report an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a frozen child outlived its cancelled context")
	}

	waiting := exec.CommandContext(ctx, "sleep", "30")
	if err := runCharged(ctx, waiting); !errors.Is(err, context.Canceled) {
		t.Fatalf("a start waiting on a suspended node must end with the cancel, got %v", err)
	}
}

// Short probes with their own deadline and the HTTP server's own children are
// outside the freeze: they must not turn a pause into a timeout.
func TestSuspendLeavesDeadlineProbesAndServerChildrenAlone(t *testing.T) {
	ctl := newSuspendControl()
	ctl.apply(true)
	ctx := withSuspendControl(context.Background(), ctl)

	// compact_chain-style probe: exec with a deadline, not through runCharged.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(probeCtx, "sh", "-c", "echo probe").Output(); err != nil || strings.TrimSpace(string(out)) != "probe" {
		t.Fatalf("a deadline probe was held by the suspend: %q %v", out, err)
	}
	// The server process has no control on its context: runCharged runs as before.
	if err := runCharged(context.Background(), exec.Command("true")); err != nil {
		t.Fatalf("a child outside any runner: %v", err)
	}
}

func TestSuspendRecordIsDurableAndHoldsTheQueue(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, nil)
	if w.Suspended() || w.queueHeld() {
		t.Fatal("a fresh worker is not suspended")
	}
	if err := w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	// A restart builds a new Worker on the same state directory.
	restarted := NewWorker(w.cfg)
	if !restarted.Suspended() || !restarted.queueHeld() || restarted.Held() {
		t.Fatalf("suspend must survive a restart and hold the queue without touching the hold: suspended=%v queueHeld=%v hold=%v", restarted.Suspended(), restarted.queueHeld(), restarted.Held())
	}
	if err := w.SetSuspend(true); err != nil {
		t.Fatalf("repeating is fine: %v", err)
	}
	if err := os.WriteFile(w.suspendPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !w.Suspended() {
		t.Fatal("an unreadable record must suspend")
	}
	if err := w.SetSuspend(false); err != nil || w.Suspended() || w.queueHeld() {
		t.Fatalf("resume: %v", err)
	}
	if err := w.SetSuspend(false); err != nil {
		t.Fatalf("resuming twice is fine: %v", err)
	}
}

// Nothing queued starts while suspended, and the queue goes on after the resume.
func TestSuspendedNodeStartsNoQueuedJob(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	if err := w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	started, err := w.scheduleQueuedLocked(context.Background(), "self", "")
	if err != nil || started != 0 {
		t.Fatalf("a suspended node started %d jobs (%v)", started, err)
	}
}

// The runner follows the durable record: it freezes its child when the record
// appears, reports itself suspended in its heartbeat, and continues the child
// when the record goes away.
func TestRunnerWatchesTheRecordAndReportsInTheHeartbeat(t *testing.T) {
	oldPoll := suspendPollInterval
	suspendPollInterval = 10 * time.Millisecond
	defer func() { suspendPollInterval = oldPoll }()

	w, _, _ := newPR3Worker(t, 2, nil)
	jobDir := filepath.Join(w.cfg.StateDir, "job1")
	jobFile := filepath.Join(jobDir, "job.json")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveJobAtomic(jobFile, &JobRecord{ID: "job1", Status: "running", PID: os.Getpid(), DurationSec: 100}); err != nil {
		t.Fatal(err)
	}
	w.isAlive = func(*JobRecord) bool { return true }

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	ctl := newSuspendControl()
	w.watchSuspend(runCtx, ctl)
	ctx := withSuspendControl(runCtx, ctl)
	file := filepath.Join(t.TempDir(), "count")
	cmd := counterCmd(ctx, file)
	done := make(chan error, 1)
	go func() { done <- runCharged(ctx, cmd) }()
	waitFor(t, "the child to count", func() bool { return readCount(t, file) > 3 })

	if err := w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the runner to freeze its child", ctl.isFrozen)
	time.Sleep(100 * time.Millisecond)
	frozenFor(t, file)
	if err := w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	job, _ := LoadJob(jobFile)
	if !job.Suspended || job.ProgressIsStale {
		t.Fatalf("heartbeat while frozen: suspended=%v stale=%v", job.Suspended, job.ProgressIsStale)
	}
	if got := telemetryFor(job, time.Now()); !got.Suspended {
		t.Fatal("the status must say the job is suspended")
	}
	if active, frozen, err := w.suspendCounts(); err != nil || active != 1 || frozen != 1 {
		t.Fatalf("counts = %d active, %d frozen (%v)", active, frozen, err)
	}

	if err := w.SetSuspend(false); err != nil {
		t.Fatal(err)
	}
	frozenAt := readCount(t, file)
	waitFor(t, "the child to continue", func() bool { return readCount(t, file) > frozenAt+3 })
	if err := w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	job, _ = LoadJob(jobFile)
	if job.Suspended {
		t.Fatal("heartbeat after the resume must clear suspended")
	}
	if active, frozen, _ := w.suspendCounts(); active != 1 || frozen != 0 {
		t.Fatalf("counts after resume = %d active, %d frozen", active, frozen)
	}
	_ = cmd.Process.Kill()
	<-done
}

// A record of a finished job never reads as suspended.
func TestSuspendedIsNotReportedForAFinishedJob(t *testing.T) {
	job := &JobRecord{ID: "done", Status: "completed"}
	job.Suspended = true
	if telemetryFor(job, time.Now()).Suspended {
		t.Fatal("a completed job is not suspended")
	}
}

// The size watch decides on what is written and encoded: a frozen encode
// repeats the same reading, so it can never be refused for standing still.
func TestSizeWatchDoesNotRefuseAFrozenEncode(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mkv")
	progress := filepath.Join(dir, "progress.txt")
	if err := os.WriteFile(out, make([]byte, 20_000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(progress, []byte("out_time_us=30000000\nprogress=continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	watch := &encodeSizeWatch{sourceBytes: 100_000, durationSec: 100, policy: &transcode.SizePolicy{MinSavingsPercent: 15}, rule: qualityModeProjection, outputPath: out, progressPath: progress}
	for i := 0; i < 5; i++ {
		if r := watch.check(); r != nil {
			t.Fatalf("reading %d of a frozen encode was refused: %v", i, r)
		}
	}
}

func TestSuspendEndpoint(t *testing.T) {
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	if resp := doRequest(t, srv, "GET", "/v1/suspend", "", "secret"); resp.Code != 200 || !strings.Contains(resp.Body.String(), `"suspend":false`) || !strings.Contains(resp.Body.String(), `"frozen":0`) {
		t.Fatalf("get: %d %s", resp.Code, resp.Body.String())
	}
	if resp := doRequest(t, srv, "PUT", "/v1/suspend", `{"suspend":true}`, "secret"); resp.Code != 200 || !worker.Suspended() || worker.Held() {
		t.Fatalf("put true: %d suspended=%v", resp.Code, worker.Suspended())
	}
	if resp := doRequest(t, srv, "PUT", "/v1/suspend", `{"suspend":true,"extra":1}`, "secret"); resp.Code != 400 {
		t.Fatalf("unknown fields must be refused: %d", resp.Code)
	}
	if resp := doRequest(t, srv, "PUT", "/v1/suspend", `{}`, "secret"); resp.Code != 400 {
		t.Fatalf("a missing flag must be refused: %d", resp.Code)
	}
	if resp := doRequest(t, srv, "PUT", "/v1/suspend", `{"suspend":false}`, "secret"); resp.Code != 200 || worker.Suspended() {
		t.Fatalf("put false: %d", resp.Code)
	}
	if resp := doRequest(t, srv, "PUT", "/v1/suspend", `{"suspend":true}`, "wrong"); resp.Code != 401 && resp.Code != 403 {
		t.Fatalf("auth: %d", resp.Code)
	}
}
