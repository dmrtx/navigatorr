package transcodeworker

import (
	"context"
	"math/rand"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Seven windows three at a time ran 3+3+1 on a third of the job's threads
// each. A pass now takes the job's threads split between the windows still to
// finish: a third while three or more remain, a half for the last two, all of
// them for the last one. At full concurrency a pass gets exactly what it got
// before.
func TestWindowThreadsGiveTheLastWindowsTheIdleThreads(t *testing.T) {
	withHostCPUs(t, 12)
	w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 1}}
	w.videoInUseFn = func() (int, bool) { return 1, true }
	concurrency := w.finalValidationConcurrency(7)
	if concurrency != 3 {
		t.Fatalf("concurrency %d", concurrency)
	}
	threads := w.newWindowThreads(concurrency, 7)
	want := []int{4, 4, 4, 4, 4, 6, 12}
	for finished, n := range want {
		if got := threads.pass(); got != n {
			t.Fatalf("after %d windows: %d threads, want %d", finished, got, n)
		}
		if finished == 0 && n != w.metricThreads(concurrency) {
			t.Fatalf("a pass at full concurrency gets %d, before %d", n, w.metricThreads(concurrency))
		}
		threads.done()
	}
	// An explicit metric_threads fixes every pass.
	fixed := (&Worker{cfg: &WorkerConfig{MaxParallelJobs: 1, MetricThreads: 2}}).newWindowThreads(3, 7)
	for range 7 {
		if got := fixed.pass(); got != 2 {
			t.Fatalf("explicit metric_threads gave %d", got)
		}
		fixed.done()
	}
}

// However windows finish, the passes running at any moment never hold more
// than the job's threads together, including after a failure stops the
// windows that were not started.
func TestWindowThreadsNeverExceedTheJobsThreads(t *testing.T) {
	for _, tc := range []struct {
		budget, concurrency, windows, failAt int
	}{
		{12, 3, 7, -1},
		{12, 3, 7, 4},
		{10, 3, 9, -1},
		{8, 2, 5, -1},
		{3, 3, 7, -1},
	} {
		for seed := range 20 {
			threads := &windowThreads{budget: tc.budget, concurrency: tc.concurrency, windows: tc.windows}
			var mu sync.Mutex
			inUse, peak := 0, 0
			rng := rand.New(rand.NewSource(int64(seed)))
			var delays [][3]time.Duration
			for range tc.windows {
				delays = append(delays, [3]time.Duration{time.Duration(rng.Intn(3)) * time.Millisecond, time.Duration(rng.Intn(4)) * time.Millisecond, time.Duration(rng.Intn(4)) * time.Millisecond})
			}
			measure := func(_ context.Context, pos int) finalWindowResult {
				defer threads.done()
				time.Sleep(delays[pos][0])
				if pos == tc.failAt {
					return finalWindowResult{code: "quality_final_sample_extract_failed"}
				}
				for _, d := range delays[pos][1:] {
					n := threads.pass()
					mu.Lock()
					inUse += n
					peak = max(peak, inUse)
					mu.Unlock()
					time.Sleep(d)
					mu.Lock()
					inUse -= n
					mu.Unlock()
				}
				return finalWindowResult{}
			}
			if _, _, err := runFinalWindows(context.Background(), tc.windows, tc.concurrency, measure); err != nil {
				t.Fatal(err)
			}
			if peak > tc.budget {
				t.Fatalf("%+v seed %d: passes held %d threads together, budget %d", tc, seed, peak, tc.budget)
			}
		}
	}
}

// Each step's children charge both the phase's meter and the step, and a step
// reports its wall time summed over its runs, how many windows ran it, and CPU
// time only where a child was measured.
func TestValidationStepsChargeEachStep(t *testing.T) {
	meter := &cpuMeter{}
	steps := &validationSteps{}
	ctx := withValidationSteps(withCPUMeter(context.Background(), meter), steps)
	for range 2 {
		stepCtx, end := beginValidationStep(ctx, validationStepVMAF)
		if err := runCharged(stepCtx, exec.Command("/bin/sh", "-c", "i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done")); err != nil {
			t.Fatal(err)
		}
		end()
	}
	_, end := beginValidationStep(ctx, validationStepAlignment)
	time.Sleep(5 * time.Millisecond)
	end()
	costs := steps.phaseCosts()
	vmaf, align := costs[validationStepVMAF], costs[validationStepAlignment]
	if vmaf.Attempts != 2 || vmaf.ActiveComputeMs == nil || *vmaf.ActiveComputeMs < 0 {
		t.Fatalf("vmaf step %+v", vmaf)
	}
	if align.Attempts != 1 || align.DurationMs < 5 || align.ActiveComputeMs != nil {
		t.Fatalf("a step that ran no child must report unknown CPU: %+v", align)
	}
	if meter.activeComputeMs() == nil || *meter.activeComputeMs() < *vmaf.ActiveComputeMs {
		t.Fatal("the phase meter lost the step's children")
	}
	if len(costs) != 2 {
		t.Fatalf("steps that never ran reported: %v", costs)
	}
	// Without steps on the context, beginning one is a no-op.
	plain, end := beginValidationStep(context.Background(), validationStepCAMBI)
	end()
	if plain != context.Background() {
		t.Fatal("a step began without validation steps")
	}
}

// Step costs are stored on the job in one write, keep their own durations and
// window counts, and add up over a second validation (a resumed job).
func TestAddPhaseCostsAccumulatesStepCosts(t *testing.T) {
	jobDir := t.TempDir()
	jobFile := filepath.Join(jobDir, "job.json")
	job := &JobRecord{ID: "steps", Status: "running"}
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	w := &Worker{cfg: &WorkerConfig{}}
	cpu := func(n int64) *int64 { return &n }
	for range 2 {
		w.addPhaseCosts(jobDir, jobFile, job, map[string]transcode.PhaseCost{
			validationStepVMAF:      {DurationMs: 1000, Attempts: 7, ActiveComputeMs: cpu(5000)},
			validationStepAlignment: {DurationMs: 30, Attempts: 7},
		})
	}
	stored, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	vmaf, align := stored.PhaseCosts[validationStepVMAF], stored.PhaseCosts[validationStepAlignment]
	if vmaf.DurationMs != 2000 || vmaf.Attempts != 14 || vmaf.ActiveComputeMs == nil || *vmaf.ActiveComputeMs != 10000 {
		t.Fatalf("vmaf step %+v", vmaf)
	}
	if align.DurationMs != 60 || align.Attempts != 14 || align.ActiveComputeMs != nil {
		t.Fatalf("alignment step %+v", align)
	}
	if job.PhaseCosts[validationStepVMAF].Attempts != 14 {
		t.Fatal("the in-memory job missed the stored costs")
	}
}
