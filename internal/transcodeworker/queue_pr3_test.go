package transcodeworker

// PR3 tests: authoritative persistent worker queue + strong transcode-submit
// idempotency. Deterministic without real ffmpeg: spawns are stubbed with
// fake PIDs plus an injected stub-consistent liveness function, so no test
// depends on the test binary's argv matching _internal_run and no signal can
// reach a real process.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

type stubSpawn struct {
	mu    sync.Mutex
	count int
	alive map[string]bool
	next  int
}

func newStubSpawn() *stubSpawn {
	return &stubSpawn{alive: map[string]bool{}, next: 1 << 30}
}

func (s *stubSpawn) fn(selfExe, configPath, jobID string) (int, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	// Fake PID in a range real PIDs can never occupy (pid_max <= 2^22 on
	// Linux, 99999 on macOS): Kill() on it is always ESRCH, so even an
	// accidental signal is harmless. The stable start token pairs with the
	// injected liveness below so stub-spawned jobs are never judged by the
	// test binary's argv.
	pid := s.next
	s.next++
	s.alive[jobID] = true
	return pid, fmt.Sprintf("stub-start-%d", pid), nil
}

func (s *stubSpawn) isAlive(job *JobRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive[job.ID]
}

// occupy marks jobID alive with a fake PID for seeded running occupants.
func (s *stubSpawn) occupy(jobID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid := s.next
	s.next++
	s.alive[jobID] = true
	return pid
}

// kill marks jobID dead, simulating runner exit without any daemon API call.
func (s *stubSpawn) kill(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.alive, jobID)
}

// install wires both the spawn stub and its consistent liveness function so
// tests never depend on the test binary's argv matching _internal_run.
func (s *stubSpawn) install(w *Worker) {
	w.SetTranscodeSpawner(s.fn)
	w.SetAliveFunc(s.isAlive)
}

func (s *stubSpawn) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func newPR3Worker(t *testing.T, maxJobs int, stub *stubSpawn) (*Worker, string, string) {
	t.Helper()
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("dummy-media"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: maxJobs,
		Quality:         65,
	}
	w := NewWorker(cfg)
	if stub != nil {
		stub.install(w)
	}
	return w, tempDir, sourceFile
}

func TestPR3_SubmitFullCapacityQueuedPersistedThenStarts(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	ctx := context.Background()

	// Occupy the single slot with a live running record (stub-consistent
	// liveness, not the test binary's PID).
	seed := &JobRecord{ID: "job-running", Status: "running", PID: stub.occupy("job-running"), Source: sourceFile, Candidate: filepath.Join(tempDir, "out1.mkv"), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-running", "job.json"), seed); err != nil {
		t.Fatal(err)
	}

	resp, err := w.Submit(ctx, SubmitRequest{ID: "job-queued-1", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "out2.mkv")}, "test-exe", "")
	if err != nil {
		t.Fatalf("submit under full capacity must succeed queued: %v", err)
	}
	if resp.Status != "queued" {
		t.Fatalf("expected queued, got %+v", resp)
	}
	if resp.Reused {
		t.Fatalf("new submit must not be marked reused")
	}
	loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-queued-1", "job.json"))
	if err != nil || loaded.Status != "queued" {
		t.Fatalf("persisted queued job missing: %+v err %v", loaded, err)
	}
	if loaded.PID != 0 {
		t.Fatalf("queued-behind-capacity must not spawn yet, PID=%d", loaded.PID)
	}
	if stub.n() != 0 {
		t.Fatalf("no spawn expected while full, got %d", stub.n())
	}

	// Free the slot: simulate runner exit (liveness off) then reconcile via
	// Cancel. The fake PID can never name a real process, so no signal can
	// escape to the test binary.
	stub.kill("job-running")
	if _, err := w.Cancel(ctx, "job-running"); err != nil {
		t.Fatal(err)
	}

	n, err := w.ScheduleQueued(ctx, "test-exe", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected scheduler to start 1 job, got %d", n)
	}
	if stub.n() != 1 {
		t.Fatalf("expected exactly one spawn after slot freed, got %d", stub.n())
	}
	after, _ := LoadJob(filepath.Join(tempDir, "jobs", "job-queued-1", "job.json"))
	if after.PID <= 1 {
		t.Fatalf("scheduled job must carry a spawn PID, got %+v", after)
	}
}

func TestPR3_GlobalConcurrencyCeiling(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	ctx := context.Background()
	mockExe := createMockWorkerScript(t, tempDir)

	// Occupy the global slot with a live benchmark (mock sleep process).
	breq := validWorkerBenchmarkRequest(sourceFile)
	breq.ID = "bench-occupant-1"
	bresp, err := w.BenchmarkSubmit(ctx, breq, mockExe, "")
	if err != nil {
		t.Fatalf("benchmark submit: %v", err)
	}
	if bresp.Status != "queued" && bresp.Status != "running" {
		t.Fatalf("unexpected benchmark status %+v", bresp)
	}

	// Transcode submit must queue without spawning and without exceeding the
	// single global slot.
	tresp, err := w.Submit(ctx, SubmitRequest{ID: "job-capped-1", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "cap.mkv")}, "test-exe", "")
	if err != nil {
		t.Fatalf("transcode submit must queue under benchmark occupancy: %v", err)
	}
	if tresp.Status != "queued" {
		t.Fatalf("expected queued, got %+v", tresp)
	}
	active, err := w.countActiveJobs("")
	if err != nil {
		t.Fatal(err)
	}
	if active > 1 {
		t.Fatalf("aggregate concurrency exceeds max_parallel_jobs: %d > 1", active)
	}
	// Cleanup: cancel benchmark (kills mock sleep) then schedule transcode.
	if _, err := w.BenchmarkCancel(ctx, "bench-occupant-1"); err != nil {
		t.Fatal(err)
	}
}

func TestPR3_PersistedQueuedScheduledAfterRestart(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	ctx := context.Background()

	seed := &JobRecord{ID: "job-running", Status: "running", PID: stub.occupy("job-running"), Source: sourceFile, Candidate: filepath.Join(tempDir, "o1.mkv"), CreatedAt: time.Now().UTC()}
	_ = SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-running", "job.json"), seed)
	if _, err := w.Submit(ctx, SubmitRequest{ID: "job-restart-1", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "o2.mkv")}, "test-exe", ""); err != nil {
		t.Fatal(err)
	}

	// Simulate daemon/worker restart with a fresh Worker over the same dir,
	// sharing the stub's liveness view (same machine, same runners).
	w2 := NewWorker(&WorkerConfig{StateDir: filepath.Join(tempDir, "jobs"), AllowedRoots: []string{tempDir}, MaxParallelJobs: 1})
	stub.install(w2)
	// Free slot then run startup scheduler.
	stub.kill("job-running")
	if _, err := w2.Cancel(ctx, "job-running"); err != nil {
		t.Fatal(err)
	}
	n, err := w2.ScheduleQueued(ctx, "test-exe", "")
	if err != nil || n != 1 {
		t.Fatalf("restart scheduler must start persisted queued job: n=%d err=%v", n, err)
	}
	if stub.n() != 1 {
		t.Fatalf("expected one spawn after restart, got %d", stub.n())
	}
}

func TestPR3_SameKeySameDigestReusesNoDoubleSpawn(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	cand := filepath.Join(tempDir, "same.mkv")

	r1, err := w.Submit(ctx, SubmitRequest{ID: "job-a1", SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: "key-same-1"}, "test-exe", "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w.Submit(ctx, SubmitRequest{ID: "job-a2", SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: "key-same-1"}, "test-exe", "")
	if err != nil {
		t.Fatalf("same key/same digest must reuse, got err %v", err)
	}
	if r2.ID != r1.ID {
		t.Fatalf("expected same job ID %q, got %q", r1.ID, r2.ID)
	}
	if stub.n() != 1 {
		t.Fatalf("expected one spawn total, got %d", stub.n())
	}
	// No second durable job directory for the alias ID.
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", "job-a2")); !os.IsNotExist(err) {
		t.Fatalf("alias submit must not create a second job dir")
	}
}

func TestPR3_SameKeyDifferentDigestConflict(t *testing.T) {
	stub := newStubSpawn()
	w, _, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	tempDir := filepath.Dir(sourceFile)

	if _, err := w.Submit(ctx, SubmitRequest{ID: "job-c1", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "c1.mkv"), IdempotencyKey: "key-conf-1"}, "test-exe", ""); err != nil {
		t.Fatal(err)
	}
	_, err := w.Submit(ctx, SubmitRequest{ID: "job-c2", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "c2-different.mkv"), IdempotencyKey: "key-conf-1"}, "test-exe", "")
	if err == nil || !IsIdempotencyConflict(err) {
		t.Fatalf("expected deterministic idempotency conflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "idempotency_conflict") {
		t.Fatalf("conflict must carry idempotency_conflict substring: %v", err)
	}

	// HTTP mapping: same scenario over POST /v1/jobs => 409 structured error.
	srv := NewServer(w, "test-exe", "", "")
	payload, _ := json.Marshal(SubmitRequest{ID: "job-c3", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "c3-other.mkv"), IdempotencyKey: "key-conf-1"})
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("HTTP conflict must be 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || !strings.Contains(env["error"], "idempotency_conflict") {
		t.Fatalf("409 must carry structured idempotency_conflict error: %s", rec.Body.String())
	}
}

func TestPR3_ConcurrentSameKeyOneSpawn(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	cand := filepath.Join(tempDir, "conc.mkv")

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	resps := make([]SubmitResponse, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("job-conc-%d", i)
			r, err := w.Submit(ctx, SubmitRequest{ID: id, SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: "key-conc-1"}, "test-exe", "")
			resps[i] = r
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submit %d failed: %v", i, err)
		}
	}
	first := resps[0].ID
	for i, r := range resps {
		if r.ID != first {
			t.Fatalf("submit %d returned %q, want single job %q", i, r.ID, first)
		}
	}
	if stub.n() != 1 {
		t.Fatalf("concurrent same-key submits must yield one spawn, got %d", stub.n())
	}
}

func TestPR3_QueuedCancelNeverSpawns(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	ctx := context.Background()

	seed := &JobRecord{ID: "job-running", Status: "running", PID: stub.occupy("job-running"), Source: sourceFile, Candidate: filepath.Join(tempDir, "o1.mkv"), CreatedAt: time.Now().UTC()}
	_ = SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-running", "job.json"), seed)
	if _, err := w.Submit(ctx, SubmitRequest{ID: "job-qcancel", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "qc.mkv")}, "test-exe", ""); err != nil {
		t.Fatal(err)
	}
	cres, err := w.Cancel(ctx, "job-qcancel")
	if err != nil || cres.Status != "cancelled" {
		t.Fatalf("queued cancel failed: %+v err %v", cres, err)
	}
	// Free the slot and run the scheduler: cancelled job must never spawn.
	stub.kill("job-running")
	if _, err := w.Cancel(ctx, "job-running"); err != nil {
		t.Fatal(err)
	}
	before := stub.n()
	n, err := w.ScheduleQueued(ctx, "test-exe", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || stub.n() != before {
		t.Fatalf("cancelled queued job must never spawn (started=%d spawns=%d)", n, stub.n())
	}
	st, _ := w.Status(ctx, "job-qcancel")
	if st.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %q", st.Status)
	}
}

func TestPR3_FreshSubmitSpawnsExactlyOnceNoDeadlock(t *testing.T) {
	// Regression for flock self-deadlock: Submit releases the per-job lock
	// before the scheduler re-acquires it. A fresh under-capacity submit must
	// return promptly with exactly one spawn.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	type res struct {
		resp SubmitResponse
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := w.Submit(context.Background(), SubmitRequest{
			ID: "job-fresh-1", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "fresh.mkv"),
		}, "test-exe", "")
		ch <- res{r, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("fresh submit failed: %v", got.err)
		}
		if got.resp.Status != "queued" || got.resp.Reused {
			t.Fatalf("expected new queued submit, got %+v", got.resp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Submit did not return within 10s (job-lock self-deadlock: scheduler re-acquired a held flock)")
	}
	if stub.n() != 1 {
		t.Fatalf("expected exactly one spawn, got %d", stub.n())
	}
	loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-fresh-1", "job.json"))
	if err != nil || loaded.PID <= 1 || !strings.HasPrefix(loaded.ProcessStartTime, "stub-start-") {
		t.Fatalf("expected stub-spawned identity, got %+v err %v", loaded, err)
	}
}

func TestPR3_SchedulerAutonomouslyDrainsOnFreedSlot(t *testing.T) {
	// The daemon-owned scheduler must notice freed capacity and start queued
	// jobs with no further submit/status request, including while Navigatorr
	// is disconnected. The slot is freed here by direct disk mutation
	// (simulating runner exit), never via a daemon API call.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seed := &JobRecord{ID: "job-occ", Status: "running", PID: stub.occupy("job-occ"), Source: sourceFile, Candidate: filepath.Join(tempDir, "occ.mkv"), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-occ", "job.json"), seed); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Submit(ctx, SubmitRequest{ID: "job-drain-1", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "drain.mkv")}, "test-exe", ""); err != nil {
		t.Fatal(err)
	}
	stop := w.RunScheduler(ctx, "test-exe", "", 20*time.Millisecond)
	defer stop()

	// A tick while full must not spawn.
	time.Sleep(60 * time.Millisecond)
	if stub.n() != 0 {
		t.Fatalf("no spawn expected while slot occupied, got %d", stub.n())
	}

	// Runner exits: direct record mutation only, no Submit/Status/Cancel call.
	occ, err := LoadJob(filepath.Join(tempDir, "jobs", "job-occ", "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	stub.kill("job-occ")
	occ.Status = "completed"
	occ.FinishedAt = time.Now().UTC()
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-occ", "job.json"), occ); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-drain-1", "job.json"))
		if err == nil && loaded.PID > 1 && stub.n() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduler did not autonomously start queued job (spawns=%d job=%+v)", stub.n(), loaded)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Stop is idempotent and terminates the ticker.
	stop()
	stop()
}

func TestPR3_ServerStartSchedulerWiring(t *testing.T) {
	// Nil worker: no-op, no panic, idempotent stop.
	nilSrv := NewServer(nil, "test-exe", "", "")
	stopNil := nilSrv.StartScheduler(context.Background(), time.Millisecond)
	if stopNil == nil {
		t.Fatal("expected non-nil stop func")
	}
	stopNil()
	stopNil()

	// Server-owned scheduler performs the startup sweep: a PID-0 queued job
	// persisted before (re)start is discovered and spawned with no requests.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 4, stub)
	seed := &JobRecord{
		ID: "job-srv-queued", Status: "queued", Source: sourceFile,
		Candidate: filepath.Join(tempDir, "srv.mkv"), CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-srv-queued", "job.json"), seed); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(w, "test-exe", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := srv.StartScheduler(ctx, 20*time.Millisecond)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-srv-queued", "job.json"))
		if err == nil && loaded.PID > 1 && stub.n() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server scheduler did not start persisted queued job (spawns=%d)", stub.n())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPR3_LegacyEmptyDigestSameSpecBackfills(t *testing.T) {
	// Legacy record (no key/digest) resubmitted with the identical spec must
	// safely backfill key+digest+resolved plan and reuse with a single spawn.
	// job.json must stay consistent with its execution digest.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	cand := filepath.Join(tempDir, "legacy.mkv")

	legacy := &JobRecord{
		ID: "job-legacy-1", Status: "queued", Source: sourceFile,
		Candidate: cand, Profile: "hevc-vt", CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-legacy-1", "job.json"), legacy); err != nil {
		t.Fatal(err)
	}
	resp, err := w.Submit(ctx, SubmitRequest{ID: "job-legacy-1", SourcePath: sourceFile, CandidatePath: cand}, "test-exe", "")
	if err != nil {
		t.Fatalf("same-spec legacy resubmit must reuse, got err %v", err)
	}
	if !resp.Reused || resp.ID != "job-legacy-1" {
		t.Fatalf("expected reused job-legacy-1, got %+v", resp)
	}
	if stub.n() != 1 {
		t.Fatalf("expected exactly one spawn, got %d", stub.n())
	}
	loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-legacy-1", "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IdempotencyKey != "job-legacy-1" || !strings.HasPrefix(loaded.ExecutionSpecDigest, "sha256:") {
		t.Fatalf("expected backfilled key+digest, got %+v", loaded)
	}
	if loaded.Plan == nil {
		t.Fatalf("backfilled legacy record must carry resolved effective plan, got nil Plan: %+v", loaded)
	}
	if !strings.HasPrefix(loaded.Plan.PlanDigest, "sha256:") {
		t.Fatalf("backfilled plan must carry canonical PlanDigest, got %+v", loaded.Plan)
	}
	if pd, err := transcode.DigestPlan(loaded.Plan); err != nil || pd != loaded.Plan.PlanDigest {
		t.Fatalf("backfilled PlanDigest must be valid: recomputed=%s stored=%s err=%v", pd, loaded.Plan.PlanDigest, err)
	}
	want, err := transcode.DigestTranscodeExecutionSpec(sourceFile, cand, "hevc-vt", loaded.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExecutionSpecDigest != want {
		t.Fatalf("backfilled digest must equal the persisted spec digest: got %s want %s", loaded.ExecutionSpecDigest, want)
	}
	// Second identical submit: still one spawn total.
	if _, err := w.Submit(ctx, SubmitRequest{ID: "job-legacy-1", SourcePath: sourceFile, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("second same-spec submit must reuse, got %v", err)
	}
	if stub.n() != 1 {
		t.Fatalf("no additional spawn expected, got %d", stub.n())
	}
}

func TestPR3_LegacySameIDExplicitKeyBackfillsPlan(t *testing.T) {
	// Same-ID legacy path (distinct from strong key scan when callers use an
	// explicit key differing from the job ID): same-spec resubmit must
	// backfill IdempotencyKey + ExecutionSpecDigest + resolved Plan atomically
	// and reuse with a single spawn; job.json stays digest-consistent.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	cand := filepath.Join(tempDir, "legacy-sameid.mkv")

	legacy := &JobRecord{
		ID: "job-legacy-sameid", Status: "queued", Source: sourceFile,
		Candidate: cand, Profile: "hevc-vt", CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", legacy.ID, "job.json"), legacy); err != nil {
		t.Fatal(err)
	}
	const explicitKey = "custom-key-sameid-1"
	resp, err := w.Submit(ctx, SubmitRequest{ID: legacy.ID, SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: explicitKey}, "test-exe", "")
	if err != nil {
		t.Fatalf("same-spec same-ID legacy resubmit must reuse, got err %v", err)
	}
	if !resp.Reused || resp.ID != legacy.ID {
		t.Fatalf("expected reused %q, got %+v", legacy.ID, resp)
	}
	if stub.n() != 1 {
		t.Fatalf("expected exactly one spawn, got %d", stub.n())
	}
	loaded, err := LoadJob(filepath.Join(tempDir, "jobs", legacy.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IdempotencyKey != explicitKey || !strings.HasPrefix(loaded.ExecutionSpecDigest, "sha256:") {
		t.Fatalf("expected backfilled key+digest, got %+v", loaded)
	}
	if loaded.Plan == nil {
		t.Fatalf("backfilled same-ID legacy must carry resolved plan, got nil: %+v", loaded)
	}
	if !strings.HasPrefix(loaded.Plan.PlanDigest, "sha256:") {
		t.Fatalf("backfilled plan must carry canonical PlanDigest, got %+v", loaded.Plan)
	}
	if pd, err := transcode.DigestPlan(loaded.Plan); err != nil || pd != loaded.Plan.PlanDigest {
		t.Fatalf("backfilled PlanDigest must be valid: recomputed=%s stored=%s err=%v", pd, loaded.Plan.PlanDigest, err)
	}
	want, err := transcode.DigestTranscodeExecutionSpec(sourceFile, cand, "hevc-vt", loaded.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExecutionSpecDigest != want {
		t.Fatalf("backfilled digest must equal persisted spec digest: got %s want %s", loaded.ExecutionSpecDigest, want)
	}
	// Second identical submit with same explicit key: still one spawn.
	if _, err := w.Submit(ctx, SubmitRequest{ID: legacy.ID, SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: explicitKey}, "test-exe", ""); err != nil {
		t.Fatalf("second same-spec submit must reuse, got %v", err)
	}
	if stub.n() != 1 {
		t.Fatalf("no additional spawn expected, got %d", stub.n())
	}
}

func TestPR3_LegacyEmptyDigestChangedSpecConflicts(t *testing.T) {
	cases := []struct {
		name     string
		wantHTTP bool
	}{
		{name: "changed candidate", wantHTTP: true},
		{name: "changed plan"},
		{name: "changed profile"},
	}
	for ci, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubSpawn()
			w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
			ctx := context.Background()
			baseCand := filepath.Join(tempDir, "base.mkv")
			basePlan := validExecutorPlan(t)

			legacy := &JobRecord{
				ID: fmt.Sprintf("job-leg-%d", ci), Status: "queued", Source: sourceFile,
				Candidate: baseCand, Profile: "hevc-vt", Plan: basePlan, CreatedAt: time.Now().UTC(),
			}
			if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", legacy.ID, "job.json"), legacy); err != nil {
				t.Fatal(err)
			}

			cand := baseCand
			var plan *transcode.Plan
			switch tc.name {
			case "changed candidate":
				cand = filepath.Join(tempDir, "changed.mkv")
			case "changed plan":
				p2 := validExecutorPlan(t)
				p2.Quality = 70
				if _, err := fixPlanDigest(p2); err != nil {
					t.Fatal(err)
				}
				plan = p2
			case "changed profile":
				plan = basePlan
			}

			var reqProfile string
			var reqPlan *transcode.Plan
			switch tc.name {
			case "changed candidate":
				reqProfile, reqPlan = "hevc-vt", basePlan
			case "changed plan":
				reqProfile, reqPlan = "hevc-vt", plan
			case "changed profile":
				reqProfile, reqPlan = "other-profile", basePlan
			}
			_, err := w.Submit(ctx, SubmitRequest{ID: legacy.ID, SourcePath: sourceFile, CandidatePath: cand, Profile: reqProfile, Plan: reqPlan}, "test-exe", "")
			if err == nil || !IsIdempotencyConflict(err) {
				t.Fatalf("legacy changed spec must conflict, got %v", err)
			}
			if stub.n() != 0 {
				t.Fatalf("conflict must never spawn, got %d", stub.n())
			}
			// Legacy record untouched: still queued PID-0, no adopted digest.
			after, lerr := LoadJob(filepath.Join(tempDir, "jobs", legacy.ID, "job.json"))
			if lerr != nil {
				t.Fatal(lerr)
			}
			if after.Status != "queued" || after.PID != 0 || after.ExecutionSpecDigest != "" || after.IdempotencyKey != "" {
				t.Fatalf("legacy record must be untouched by conflict, got %+v", after)
			}
			if after.Plan == nil || after.Plan.PlanDigest != basePlan.PlanDigest {
				t.Fatalf("legacy plan must be untouched by conflict, got %+v want digest %s", after.Plan, basePlan.PlanDigest)
			}
			// Digest recomputed from persisted (untouched) job must NOT equal
			// the conflicting request digest (proves no blind adoption).
			afterDigest, err := transcode.DigestTranscodeExecutionSpec(after.Source, after.Candidate, after.Profile, after.Plan)
			if err != nil {
				t.Fatal(err)
			}
			reqResolved, err := ResolveWorkerPlan(reqProfile, reqPlan)
			if err != nil {
				t.Fatal(err)
			}
			reqDigest, err := transcode.DigestTranscodeExecutionSpec(sourceFile, cand, reqProfile, reqResolved)
			if err != nil {
				t.Fatal(err)
			}
			if afterDigest == reqDigest {
				t.Fatalf("untouched legacy digest must differ from conflicting request digest")
			}

			if tc.wantHTTP {
				srv := NewServer(w, "test-exe", "", "")
				payload, _ := json.Marshal(SubmitRequest{ID: legacy.ID, SourcePath: sourceFile, CandidatePath: cand, Profile: reqProfile, Plan: reqPlan})
				rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
				if rec.Code != http.StatusConflict {
					t.Fatalf("HTTP changed-spec legacy must be 409, got %d (%s)", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "idempotency_conflict") {
					t.Fatalf("409 must carry idempotency_conflict: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestPR3_LegacySameIDExplicitKeyChangedSpecConflictsUntouched(t *testing.T) {
	// Same-ID path with an explicit key differing from the job ID: a changed
	// spec must deterministically conflict and leave the legacy record
	// completely untouched (no key/digest/plan adoption, still queued PID-0).
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	baseCand := filepath.Join(tempDir, "base-sameid.mkv")
	legacy := &JobRecord{
		ID: "job-leg-sameid", Status: "queued", Source: sourceFile,
		Candidate: baseCand, Profile: "hevc-vt", Plan: nil, CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", legacy.ID, "job.json"), legacy); err != nil {
		t.Fatal(err)
	}
	const explicitKey = "custom-key-conflict-1"
	changedCand := filepath.Join(tempDir, "changed-sameid.mkv")
	_, err := w.Submit(ctx, SubmitRequest{ID: legacy.ID, SourcePath: sourceFile, CandidatePath: changedCand, IdempotencyKey: explicitKey}, "test-exe", "")
	if err == nil || !IsIdempotencyConflict(err) {
		t.Fatalf("same-ID changed spec must conflict, got %v", err)
	}
	if stub.n() != 0 {
		t.Fatalf("conflict must never spawn, got %d", stub.n())
	}
	after, err := LoadJob(filepath.Join(tempDir, "jobs", legacy.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "queued" || after.PID != 0 || after.ExecutionSpecDigest != "" || after.IdempotencyKey != "" {
		t.Fatalf("same-ID legacy must be untouched by conflict, got %+v", after)
	}
	if after.Plan != nil {
		t.Fatalf("profile-only legacy plan must stay nil after conflict, got %+v", after.Plan)
	}
}

func TestPR3_StrongKeyCancelledAliasReusesCancelledNoSpawn(t *testing.T) {
	// Minimum guarantee: same-key alias resubmit of an already-cancelled job
	// must return reused cancelled and never requeue/spawn.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	cand := filepath.Join(tempDir, "cancel-alias.mkv")
	const key = "key-cancel-alias-1"

	r1, err := w.Submit(ctx, SubmitRequest{ID: "job-orig-cancel", SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: key}, "test-exe", "")
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID != "job-orig-cancel" {
		t.Fatalf("unexpected first submit %+v", r1)
	}
	if stub.n() != 1 {
		t.Fatalf("expected one spawn for first submit, got %d", stub.n())
	}
	if _, err := w.Cancel(ctx, "job-orig-cancel"); err != nil {
		t.Fatal(err)
	}
	before := stub.n()
	r2, err := w.Submit(ctx, SubmitRequest{ID: "job-alias-cancel", SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: key}, "test-exe", "")
	if err != nil {
		t.Fatalf("alias resubmit of cancelled job must reuse, got err %v", err)
	}
	if !r2.Reused || r2.ID != "job-orig-cancel" {
		t.Fatalf("expected reused job-orig-cancel, got %+v", r2)
	}
	if r2.Status != "cancelled" {
		t.Fatalf("expected reused cancelled, got %+v", r2)
	}
	if stub.n() != before {
		t.Fatalf("cancelled alias resubmit must never spawn (before=%d after=%d)", before, stub.n())
	}
	loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-orig-cancel", "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "cancelled" {
		t.Fatalf("expected cancelled persisted, got %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", "job-alias-cancel")); !os.IsNotExist(err) {
		t.Fatalf("alias submit must not create a second job dir")
	}
}

func TestPR3_StrongKeyCancelVsAliasRaceKeepsCancelled(t *testing.T) {
	// Concurrent queued Cancel vs same-key alias resubmit must end cancelled
	// with no spawn. Submit holds capLock+jobLock with reload-under-lock;
	// Cancel holds jobLock; lock ordering prevents lost-update overwriting
	// cancelled with stale queued.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	ctx := context.Background()

	seed := &JobRecord{ID: "job-running", Status: "running", PID: stub.occupy("job-running"), Source: sourceFile, Candidate: filepath.Join(tempDir, "o1.mkv"), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", "job-running", "job.json"), seed); err != nil {
		t.Fatal(err)
	}
	cand := filepath.Join(tempDir, "race-cancel.mkv")
	const key = "key-race-cancel-1"
	if _, err := w.Submit(ctx, SubmitRequest{ID: "job-orig-race", SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: key}, "test-exe", ""); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var cancelErr error
	var aliasResp SubmitResponse
	var aliasErr error
	go func() {
		defer wg.Done()
		<-start
		_, cancelErr = w.Cancel(ctx, "job-orig-race")
	}()
	go func() {
		defer wg.Done()
		<-start
		aliasResp, aliasErr = w.Submit(ctx, SubmitRequest{ID: "job-alias-race", SourcePath: sourceFile, CandidatePath: cand, IdempotencyKey: key}, "test-exe", "")
	}()
	close(start)
	wg.Wait()
	if cancelErr != nil {
		t.Fatalf("cancel failed: %v", cancelErr)
	}
	if aliasErr != nil {
		t.Fatalf("alias resubmit failed: %v", aliasErr)
	}
	if !aliasResp.Reused || aliasResp.ID != "job-orig-race" {
		t.Fatalf("expected reused job-orig-race, got %+v", aliasResp)
	}
	loaded, err := LoadJob(filepath.Join(tempDir, "jobs", "job-orig-race", "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "cancelled" {
		t.Fatalf("race must end cancelled, got %+v alias=%+v", loaded, aliasResp)
	}
	if stub.n() != 0 {
		t.Fatalf("cancelled race must never spawn, got %d", stub.n())
	}
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", "job-alias-race")); !os.IsNotExist(err) {
		t.Fatalf("alias race must not create a second job dir")
	}
	stub.kill("job-running")
	if _, err := w.Cancel(ctx, "job-running"); err != nil {
		t.Fatal(err)
	}
}

func TestPR3_BackfillPersistenceFailureFailsClosed(t *testing.T) {
	// Legacy backfill establishes strong idempotency: if persisting the
	// backfilled key/digest/plan fails, Submit must fail closed, not report
	// success/reused. Uses filesystem permissions: pre-created .lock stays
	// openable under a read-only job dir while temp-file creation fails.
	if os.Geteuid() == 0 {
		t.Skip("skipping permission-based backfill failure test as root (permissions bypassed)")
	}
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	cand := filepath.Join(tempDir, "backfill-fail.mkv")
	jobID := "job-backfill-fail"
	jobDir := filepath.Join(tempDir, "jobs", jobID)
	legacy := &JobRecord{
		ID: jobID, Status: "queued", Source: sourceFile,
		Candidate: cand, Profile: "hevc-vt", CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, ".lock"), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jobDir, 0555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(jobDir, 0755) }()
	_, err := w.Submit(ctx, SubmitRequest{ID: jobID, SourcePath: sourceFile, CandidatePath: cand}, "test-exe", "")
	if err == nil {
		t.Fatal("backfill persistence failure must fail closed, got nil error")
	}
	if !strings.Contains(err.Error(), "persisting idempotency backfill failed") {
		t.Fatalf("expected persisting idempotency backfill failure, got %v", err)
	}
	if stub.n() != 0 {
		t.Fatalf("failed backfill must never spawn, got %d", stub.n())
	}
	_ = os.Chmod(jobDir, 0755)
	after, lerr := LoadJob(filepath.Join(jobDir, "job.json"))
	if lerr != nil {
		t.Fatal(lerr)
	}
	if after.IdempotencyKey != "" || after.ExecutionSpecDigest != "" || after.Plan != nil {
		t.Fatalf("failed backfill must leave legacy record without adopted key/digest/plan, got %+v", after)
	}
}

func TestPR3_StatusLeavesQueuedStalePIDUntouched(t *testing.T) {
	// Status must not persist queued-stale normalization (no jobLock here).
	// Ownership: scheduler/locked Submit own the reset under capLock+jobLock.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()
	const stalePID = 1<<30 + 7777
	const staleStart = "stub-start-stale"
	rec := &JobRecord{
		ID: "job-queued-stale", Status: "queued", Source: sourceFile,
		Candidate: filepath.Join(tempDir, "stale.mkv"), Profile: "hevc-vt",
		PID: stalePID, ProcessStartTime: staleStart, CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", rec.ID, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	got, err := w.Status(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" {
		t.Fatalf("stale queued status must report queued, got %+v", got)
	}
	after, err := LoadJob(filepath.Join(tempDir, "jobs", rec.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "queued" || after.PID != stalePID || after.ProcessStartTime != staleStart {
		t.Fatalf("Status must leave durable queued stale PID untouched, got %+v", after)
	}
	// Scheduler owns normalization: it can later reset under lock and start.
	n, err := w.ScheduleQueued(ctx, "test-exe", "")
	if err != nil || n != 1 {
		t.Fatalf("scheduler must start stale-queued job under lock: n=%d err=%v", n, err)
	}
	if stub.n() != 1 {
		t.Fatalf("expected one spawn from scheduler, got %d", stub.n())
	}
	started, err := LoadJob(filepath.Join(tempDir, "jobs", rec.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if started.PID <= 1 || started.Status != "queued" {
		t.Fatalf("scheduled job must carry spawn PID as queued, got %+v", started)
	}
}

func TestPR3_CountActiveLeavesQueuedStalePIDUntouched(t *testing.T) {
	// countActiveJobs is accounting only: queued stale PID counts as
	// non-active and must not rewrite durable state (would race Cancel).
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 1, stub)
	const stalePID = 1<<30 + 8888
	const staleStart = "stub-start-stale-count"
	rec := &JobRecord{
		ID: "job-queued-stale-count", Status: "queued", Source: sourceFile,
		Candidate: filepath.Join(tempDir, "stale-count.mkv"), Profile: "hevc-vt",
		PID: stalePID, ProcessStartTime: staleStart, CreatedAt: time.Now().UTC(),
	}
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", rec.ID, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	active, err := w.countActiveJobs("")
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("queued stale PID must be non-active, got %d", active)
	}
	after, err := LoadJob(filepath.Join(tempDir, "jobs", rec.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "queued" || after.PID != stalePID || after.ProcessStartTime != staleStart {
		t.Fatalf("countActiveJobs must leave durable queued stale PID untouched, got %+v", after)
	}
}

func TestPR3_MalformedJobJSONFailsClosed(t *testing.T) {
	// A present-but-corrupt job.json must fail submits definitively instead of
	// silently skipping the scan and duplicating.
	stub := newStubSpawn()
	w, tempDir, sourceFile := newPR3Worker(t, 8, stub)
	ctx := context.Background()

	badDir := filepath.Join(tempDir, "jobs", "job-corrupt-1")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "job.json"), []byte(`{invalid json`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := w.Submit(ctx, SubmitRequest{ID: "job-new-after-corrupt", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "new.mkv")}, "test-exe", "")
	if err == nil {
		t.Fatal("submit with corrupt durable state must fail closed, got nil error")
	}
	if IsIdempotencyConflict(err) {
		t.Fatalf("scan failure is a definitive error, not a conflict: %v", err)
	}
	if stub.n() != 0 {
		t.Fatalf("failed-closed submit must never spawn, got %d", stub.n())
	}
	if _, serr := os.Stat(filepath.Join(tempDir, "jobs", "job-new-after-corrupt")); !os.IsNotExist(serr) {
		t.Fatal("failed-closed submit must not create a new job directory")
	}

	// HTTP mapping stays a definitive non-2xx (never uncertain client-side).
	srv := NewServer(w, "test-exe", "", "")
	payload, _ := json.Marshal(SubmitRequest{ID: "job-new-after-corrupt", SourcePath: sourceFile, CandidatePath: filepath.Join(tempDir, "new.mkv")})
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", string(payload), "")
	if rec.Code < 400 {
		t.Fatalf("corrupt state must yield an error status, got %d", rec.Code)
	}
}

func TestPR3_HTTPExecutorSendsIdempotencyAnd409Definitive(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := newHTTPTestServer(t, &mu, &got)
	defer srv.Close()

	exec, err := transcode.NewHTTPExecutor(transcode.HTTPConfig{
		BaseURL: srv.URL, RequestTimeout: 5000000000, SubmitTimeout: 5000000000,
		PathMappings: []transcode.PathMapping{{Local: "/local/media", Remote: "/Volumes/media"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := validExecutorPlan(t)
	job, err := exec.Submit(context.Background(), transcode.Request{
		ID: "job-http-1", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv", Profile: "hevc-vt", Plan: plan,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if job.ID != "job-http-1" {
		t.Fatalf("job id %q", job.ID)
	}
	mu.Lock()
	if got["idempotency_key"] != "job-http-1" {
		t.Errorf("executor must send default idempotency_key, got %v", got)
	}
	digest, _ := got["execution_spec_digest"].(string)
	mu.Unlock()
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("executor must send canonical execution_spec_digest, got %v", got)
	}
	want, err := transcode.DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "hevc-vt", plan)
	if err != nil {
		t.Fatal(err)
	}
	if digest != want {
		t.Fatalf("digest mismatch: got %s want %s", digest, want)
	}

	// 409 conflict is definitive: typed HTTPError, never UncertainError.
	conflictSrv := newHTTPConflictServer(t)
	defer conflictSrv.Close()
	exec2, _ := transcode.NewHTTPExecutor(transcode.HTTPConfig{
		BaseURL: conflictSrv.URL, RequestTimeout: 5000000000, SubmitTimeout: 5000000000,
		PathMappings: []transcode.PathMapping{{Local: "/local/media", Remote: "/Volumes/media"}},
	})
	_, err = exec2.Submit(context.Background(), transcode.Request{
		ID: "job-http-2", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/other.mkv",
	})
	var herr *transcode.HTTPError
	if err == nil || !asHTTPError(err, &herr) || herr.StatusCode != http.StatusConflict {
		t.Fatalf("conflict must be typed 409, got %v", err)
	}
	if transcode.IsTransportUncertain(err) {
		t.Fatalf("complete 409 must never be UncertainError: %v", err)
	}
	if !strings.Contains(err.Error(), "idempotency_conflict") {
		t.Fatalf("409 must preserve conflict classifier: %v", err)
	}
}

func TestPR3_SSHCompatibilitySendsIdempotency(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "stdin.json")
	script := fmt.Sprintf(`
input=$(cat)
echo "$input" > %s
echo "$input" | grep -q "source_path" || exit 1
echo '{"id": "job-ssh-1", "status": "queued"}'
exit 0
`, capture)
	fakeSSH := createFakeSSHBinaryForPR3(t, script)
	cfg := transcode.SSHConfig{
		Host: "test.host", Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []transcode.PathMapping{{Local: "/local/media", Remote: "/Volumes/media"}},
	}
	exec, err := transcode.NewSSHExecutor(cfg, transcode.WithSSHBinary(fakeSSH))
	if err != nil {
		t.Fatal(err)
	}
	plan := validExecutorPlan(t)
	if _, err := exec.Submit(context.Background(), transcode.Request{
		ID: "job-ssh-1", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv", Profile: "hevc-vt", Plan: plan,
	}); err != nil {
		t.Fatalf("ssh submit: %v", err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("ssh stdin json: %v (%s)", err, raw)
	}
	if payload["idempotency_key"] != "job-ssh-1" {
		t.Errorf("ssh must send default idempotency_key, got %v", payload)
	}
	digest, _ := payload["execution_spec_digest"].(string)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("ssh must send canonical digest, got %v", payload)
	}

	// Legacy caller without plan still works (digest omitted, worker computes).
	capture2 := filepath.Join(dir, "stdin2.json")
	script2 := fmt.Sprintf(`
input=$(cat)
echo "$input" > %s
echo '{"id": "job-ssh-legacy", "status": "queued"}'
exit 0
`, capture2)
	fakeSSH2 := createFakeSSHBinaryForPR3(t, script2)
	exec2, _ := transcode.NewSSHExecutor(transcode.SSHConfig{
		Host: "test.host", Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []transcode.PathMapping{{Local: "/local/media", Remote: "/Volumes/media"}},
	}, transcode.WithSSHBinary(fakeSSH2))
	if _, err := exec2.Submit(context.Background(), transcode.Request{
		ID: "job-ssh-legacy", SourcePath: "/local/media/a.mkv", CandidatePath: "/local/media/b.mkv",
	}); err != nil {
		t.Fatalf("legacy ssh submit must remain compatible: %v", err)
	}
}

func TestPR3_HTTPSubmitValidationGuardsIntact(t *testing.T) {
	w, tempDir, sourceFile := newPR3Worker(t, 8, nil)
	srv := NewServer(w, "test-exe", "", "")
	// Unknown field still rejected (no smuggling surface for new fields).
	bad := `{"id": "job-x", "source_path": "` + sourceFile + `", "candidate_path": "` + filepath.Join(tempDir, "x.mkv") + `", "ffmpeg_args": ["-crf", "20"]}`
	rec := doRequest(t, srv, http.MethodPost, "/v1/jobs", bad, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields must stay 400, got %d", rec.Code)
	}
	// Oversized body still 400/413 bounded.
	huge := strings.Repeat("a", (1<<20)+10)
	rec = doRequest(t, srv, http.MethodPost, "/v1/jobs", huge, "")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body must be rejected, got %d", rec.Code)
	}
}

func TestPR3_ExecutionSpecDigestCanonical(t *testing.T) {
	p1 := validExecutorPlan(t)
	p2 := validExecutorPlan(t)
	d1, err := transcode.DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "hevc-vt", p1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := transcode.DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "HEVC-VT ", p2)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("profile normalization must be canonical: %s vs %s", d1, d2)
	}
	p2.Quality = 70
	if _, err := fixPlanDigest(p2); err != nil {
		t.Fatal(err)
	}
	d3, err := transcode.DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "hevc-vt", p2)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Fatalf("plan changes must alter the digest")
	}
	d4, err := transcode.DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/other.mkv", "hevc-vt", p1)
	if err != nil {
		t.Fatal(err)
	}
	if d4 == d1 {
		t.Fatalf("candidate changes must alter the digest")
	}
}

// --- helpers (PR3 test-local to avoid clashing with existing test helpers) ---

func validExecutorPlan(t *testing.T) *transcode.Plan {
	t.Helper()
	p := &transcode.Plan{
		Container: "mkv", VideoCodec: "hevc_videotoolbox", Quality: 65,
		AudioMode: "copy", SubtitleMode: "preserve",
		PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true,
		RecipeVersion: "test-1.0.0",
		RecipeDigest:  "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Resilience:    transcode.ResiliencePlan{MaxAttempts: 1},
	}
	d, err := transcode.DigestPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	p.PlanDigest = d
	return p
}

func fixPlanDigest(p *transcode.Plan) (string, error) {
	cp := *p
	cp.PlanDigest = ""
	d, err := transcode.DigestPlan(&cp)
	if err != nil {
		return "", err
	}
	p.PlanDigest = d
	return d, nil
}

func newHTTPTestServer(t *testing.T, mu *sync.Mutex, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/jobs" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*got = body
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "job-http-1", "status": "queued"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "not found"}`))
	}))
}

func newHTTPConflictServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"id": "job-http-2", "status": "", "error": "idempotency_conflict: key \"job-http-2\" already persists job \"job-http-1\" with different execution_spec_digest"}`))
	}))
}

func asHTTPError(err error, target **transcode.HTTPError) bool {
	for err != nil {
		if h, ok := err.(*transcode.HTTPError); ok {
			*target = h
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func createFakeSSHBinaryForPR3(t *testing.T, scriptContent string) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "ssh")
	content := "#!/bin/sh\n" + scriptContent + "\n"
	if err := os.WriteFile(binPath, []byte(content), 0755); err != nil {
		t.Fatalf("fake ssh: %v", err)
	}
	return binPath
}

var _ = time.Second
