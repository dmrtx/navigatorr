package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestFinalSizeRejectionPreservesSourceBeforePublication(t *testing.T) {
	for _, candidateBytes := range []int{85, 95, 110} {
		t.Run(fmt.Sprint(candidateBytes), func(t *testing.T) {
			dir := t.TempDir()
			source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), string(make([]byte, 100)))
			candidate := filepath.Join(dir, "external", "candidate.mkv")
			cfg := pr6b2Config(dir, func(cfg *WorkerConfig) { cfg.ExternalRoots = []string{filepath.Join(dir, "external")} })
			w := NewWorker(cfg)
			newStubSpawn().install(w)
			rec := &pr6b2Recorder{createOut: true}
			rec.install(w)
			w.SetRunFFmpeg(func(ctx context.Context, plan *ExecutionPlan, job *JobRecord, input, output, progress, log string) error {
				if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
					return err
				}
				return os.WriteFile(output, make([]byte, candidateBytes), 0600)
			})
			plan := ioOptPlan(t)
			plan.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: 15}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			published := 0
			w.SetFinalizeOutput(func(ctx context.Context, local, destination, id string) error {
				published++
				return FinalizeOutputAtomic(ctx, local, destination, id)
			})
			if _, err := w.Submit(context.Background(), SubmitRequest{ID: "guarded", SourcePath: source, CandidatePath: candidate, Plan: plan}, "unused", ""); err != nil {
				t.Fatal(err)
			}
			err := pr6b2Run(t, w, "guarded")
			job := pr6b1LoadJob(t, cfg.StateDir, "guarded")
			if candidateBytes > 85 {
				if err == nil || published != 0 || job.ReasonCode != "insufficient_savings" || job.FailureClassification != "policy_rejected" {
					t.Fatalf("invalid candidate was not rejected: %v published=%d job=%+v", err, published, job)
				}
			} else {
				if err != nil || published != 1 || job.Status != "completed" {
					t.Fatalf("exact 15%% boundary: %v job=%+v", err, job)
				}
			}
			info, err := os.Stat(source)
			if err != nil || info.Size() != 100 {
				t.Fatal("original changed", err)
			}
			for _, phase := range []string{"staging", "source_hash", "probe"} {
				if job.PhaseCosts[phase].Attempts != 1 {
					t.Fatalf("missing %s timing: %+v", phase, job.PhaseCosts)
				}
			}
			if candidateBytes == 85 {
				if job.PhaseCosts["publication"].NASWrittenBytes != nil || job.PhaseCosts["cleanup"].Attempts != 1 {
					t.Fatal("local write claimed NAS bytes or cleanup lost", job.PhaseCosts)
				}
			}
		})
	}
}

type slowObservedStore struct {
	ioOptCountingStore
	delay time.Duration
}

func (s *slowObservedStore) DownloadAtomic(ctx context.Context, source, local string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
	}
	return s.ioOptCountingStore.DownloadAtomic(ctx, source, local)
}

func TestSyntheticObservabilityBaseline(t *testing.T) {
	// All bytes and delays below belong to temporary synthetic files/fakes.
	// This records transfer/search latency; it cannot prove production capacity.
	dir := t.TempDir()
	root := filepath.Join(dir, "fake-nas")
	source := filepath.Join(root, "source.mkv")
	content := make([]byte, 1024*1024)
	for i := range content {
		content[i] = byte(i)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	store := &slowObservedStore{ioOptCountingStore: ioOptCountingStore{root: root, content: content}, delay: 20 * time.Millisecond}
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 2})
	w.mediaStore = store
	costA := transcode.PhaseCost{}
	start := time.Now()
	cached, err := w.ensureSourceCached(context.WithValue(context.Background(), sourceCostKey{}, &costA), source, digest)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	costB := transcode.PhaseCost{}
	start = time.Now()
	again, err := w.ensureSourceCached(context.WithValue(context.Background(), sourceCostKey{}, &costB), source, digest)
	if err != nil {
		t.Fatal(err)
	}
	reused := time.Since(start)
	if cached != again || store.downloads != 1 || costA.NASReadBytes == nil || *costA.NASReadBytes != int64(len(content)) || costA.CacheMisses == nil || *costA.CacheMisses != 1 || costB.CacheHits == nil || *costB.CacheHits != 1 || costB.NASReadBytes == nil || *costB.NASReadBytes != 0 {
		t.Fatalf("cache measurements: %+v %+v downloads=%d", costA, costB, store.downloads)
	}
	// Encoder plans do not key source cache: same source bytes remain reusable
	// by different plans. Candidate/metric evidence remains separately bound.
	planA, planB := ioOptPlan(t), ioOptPlan(t)
	planB.Quality++
	a, _ := transcode.DigestPlan(planA)
	b, _ := transcode.DigestPlan(planB)
	if a == b {
		t.Fatal("fixture plans identical")
	}
	for _, id := range []string{"benchmark", "encode"} {
		if _, err := w.acquireCachedSourceLease(context.Background(), cached, filepath.Join(dir, id, "input.mkv")); err != nil {
			t.Fatal(err)
		}
	}
	store.content[0] ^= 1
	sum = sha256.Sum256(store.content)
	newDigest := hex.EncodeToString(sum[:])
	costC := transcode.PhaseCost{}
	changed, err := w.ensureSourceCached(context.WithValue(context.Background(), sourceCostKey{}, &costC), source, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	if changed == cached || store.downloads != 2 || costC.CacheMisses == nil || *costC.CacheMisses != 1 {
		t.Fatalf("changed bytes did not invalidate: %+v downloads=%d", costC, store.downloads)
	}
	t.Logf("synthetic cache: bytes=%d injected_io_delay=20ms initial_wall=%s reuse_wall=%s downloads=2(after content invalidation) benchmark_encode_leases=2", len(content), first, reused)
	// Eight final candidate rejections, separately identified; this is a size
	// guard microbaseline, not eight FFmpeg searches or a season impossibility.
	searchStart := time.Now()
	for i := 0; i < 8; i++ {
		if err := transcode.ValidateFinalSize(100, 95, &transcode.SizePolicy{MinSavingsPercent: 15}); err == nil {
			t.Fatal("nonqualifying candidate accepted")
		}
	}
	t.Logf("eight synthetic size rejections: wall=%s measured_candidates=8 full_encodes=0 configured_search_budget=not_applicable", time.Since(searchStart))
}

func TestEightSyntheticSearchesRecordBoundedNoWinnerCost(t *testing.T) {
	dir := t.TempDir()
	source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), string(make([]byte, 100)))
	probe := strings.ReplaceAll(sdr8BitProbeJSON, `"50000000"`, `"100"`)
	mockFFmpeg, mockProbe, _ := setupMockToolsWithQualitySizes(t, dir, probe)
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, FFmpeg: mockFFmpeg, FFprobe: mockProbe, MaxParallelJobs: 2})
	runner := &ProductionBenchmarkRunner{}
	runner.SetMetricsHook(func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error {
		for i, c := range record.Candidates {
			evidence.CandidateMetrics = append(evidence.CandidateMetrics, BenchmarkCandidateMetricAggregate{CandidateID: c.ID, CandidateIndex: i, MetricType: optimization.MetricTypeVMAF, Aggregate: optimization.MetricAggregate{MetricType: optimization.MetricTypeVMAF, Valid: true, MeanScore: 96.5, MinScore: 96.5, MaxScore: 96.5, SampleScores: []optimization.SampleScore{{SampleIndex: 0, Score: 96.5, Valid: true}}}})
		}
		return nil
	})
	w.benchmarkRunner = runner
	batchStart := time.Now()
	var searchTotal float64
	for i := 0; i < 8; i++ {
		record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: fmt.Sprintf("bench-no-winner-%d", i), Status: "queued", Source: source, Metric: "vmaf", Mode: "size", SearchBudgetSeconds: 2, RunToken: "fixture-token", CreatedAt: time.Now().UTC(), Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 5, DurationSeconds: 10}}, Candidates: []transcode.BenchmarkCandidate{{ID: "large", Quality: 70}}}
		if err := SaveBenchmarkAtomic(filepath.Join(w.cfg.StateDir, record.ID, "benchmark.json"), record); err != nil {
			t.Fatal(err)
		}
		if err := w.InternalBenchmark(context.Background(), record.ID, record.RunToken); err != nil {
			t.Fatal(err)
		}
		result, err := LoadBenchmark(filepath.Join(w.cfg.StateDir, record.ID, "benchmark.json"))
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "completed" || result.Evidence == nil || result.Evidence.Decision == nil || result.Evidence.Decision.Winner != nil || result.SearchSeconds == nil || *result.SearchSeconds <= 0 || *result.SearchSeconds > 2 || result.SearchStartedAt.IsZero() {
			t.Fatalf("bounded no-winner evidence missing: %+v", result)
		}
		searchTotal += *result.SearchSeconds
		t.Logf("source=%d no_winner reason=%s source_bytes=100 measured_sample_bytes=%d search_wall_seconds=%.6f budget_seconds=2 remaining_seconds=%.6f", i, result.Evidence.Decision.DecisionReason, result.Evidence.CandidateSamples[0].SizeBytes, *result.SearchSeconds, 2-*result.SearchSeconds)
	}
	t.Logf("eight fake-process searches: batch_wall=%s sum_search_wall_seconds=%.6f independent_sources=8 configured_budget_seconds=16 production_encodes=0", time.Since(batchStart), searchTotal)
}

type blockedObservedStore struct {
	ioOptCountingStore
	started chan struct{}
	release chan struct{}
}

func (s *blockedObservedStore) DownloadAtomic(ctx context.Context, source, local string) error {
	close(s.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	return s.ioOptCountingStore.DownloadAtomic(ctx, source, local)
}
func TestSlowSyntheticIOLeavesIndependentSlotAndSchedulerObservable(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "fake-nas")
	store := &blockedObservedStore{ioOptCountingStore: ioOptCountingStore{root: root, content: make([]byte, 1024)}, started: make(chan struct{}), release: make(chan struct{})}
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 2})
	w.mediaStore = store
	spawn := newStubSpawn()
	spawn.install(w)
	slow := &JobRecord{ID: "slow", Status: "running", PID: spawn.occupy("slow"), Source: filepath.Join(root, "source.mkv"), Candidate: filepath.Join(root, "candidate.mkv"), Plan: ioOptPlan(t), CreatedAt: time.Now().UTC(), StagingState: string(StagingStatePending), StagedInputPath: filepath.Join(dir, "work", "slow", "input.mkv"), EffectiveInputPath: filepath.Join(dir, "work", "slow", "input.mkv")}
	slowDir := filepath.Join(w.cfg.StateDir, slow.ID)
	slowFile := filepath.Join(slowDir, "job.json")
	if err := SaveJobAtomic(slowFile, slow); err != nil {
		t.Fatal(err)
	}
	fast := &JobRecord{ID: "independent", Status: "queued", Source: filepath.Join(root, "independent.mkv"), Candidate: filepath.Join(root, "independent-candidate.mkv"), Plan: ioOptPlan(t), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, fast.ID, "job.json"), fast); err != nil {
		t.Fatal(err)
	}
	operational, err := resolveOperationalForExecution(slow)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := w.ensureStaged(ctx, slowDir, slowFile, slow, operational); finished <- err }()
	<-store.started
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	start := time.Now()
	w.runSchedulerSweeps(ctx, "unused", "")
	reconcileWall := time.Since(start)
	observation := w.SchedulerObservation(time.Now())
	total, used, _ := w.slotSnapshot("")
	if observation.Health != "ok" || observation.Sweeps["queue"].StartedJobs != 1 || total != 2 || used != 2 {
		t.Fatalf("blocked I/O prevented independent admission: %+v slots=%d/%d", observation, used, total)
	}
	pending, err := LoadJob(slowFile)
	if err != nil || pending.Phase != "reading_source" {
		t.Fatal("staging progress not independently observable", pending, err)
	}
	select {
	case err := <-finished:
		t.Fatal("blocked source escaped before release", err)
	default:
	}
	close(store.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	t.Logf("controlled slow I/O: blocked_transfer_bytes=1024 scheduler_wall=%s slots=%d/%d independently_admitted=1 phase=reading_source injected_wait_until_release=true", reconcileWall, used, total)
}

type resumedCostRunner struct{ deadline time.Time }

func (r *resumedCostRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	r.deadline, _ = ctx.Deadline()
	return ctx.Err()
}
func TestResumedSearchDoesNotRefundUnmeasuredPriorCost(t *testing.T) {
	dir := t.TempDir()
	source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), "synthetic source")
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, DisableSourceCache: true})
	runner := &resumedCostRunner{}
	w.benchmarkRunner = runner
	started := time.Now().UTC().Add(-time.Second)
	priorInvocation := 0.01
	record := &BenchmarkRecord{ID: "bench-resumed-cost", Status: "queued", RunToken: "fixture-token", Source: source, SearchStartedAt: started, SearchBudgetSeconds: 2, SearchSeconds: &priorInvocation, CreatedAt: started}
	path := filepath.Join(w.cfg.StateDir, record.ID, "benchmark.json")
	if err := SaveBenchmarkAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalBenchmark(context.Background(), record.ID, record.RunToken); err != nil {
		t.Fatal(err)
	}
	result, err := LoadBenchmark(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.SearchSeconds != nil || !result.SearchStartedAt.Equal(started) || !runner.deadline.Equal(started.Add(2*time.Second)) {
		t.Fatalf("prior unmeasured cost refunded or budget reset: %+v deadline=%v", result, runner.deadline)
	}
}
