package transcodeworker

import (
	"context"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
// reports its wall time summed over its runs, one attempt (this validation),
// the windows that ran it as Runs, and CPU time only where a child was
// measured.
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
	costs := steps.phaseSteps()
	vmaf, align := costs[validationStepVMAF], costs[validationStepAlignment]
	if vmaf.Attempts != 1 || vmaf.Runs != 2 || vmaf.ActiveComputeMs == nil || *vmaf.ActiveComputeMs < 0 {
		t.Fatalf("vmaf step %+v", vmaf)
	}
	if align.Attempts != 1 || align.Runs != 1 || align.DurationMs < 5 || align.ActiveComputeMs != nil {
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
	if (&validationSteps{}).phaseSteps() != nil {
		t.Fatal("a validation that ran no step reported steps")
	}
}

// Child CPU time is summed at full resolution and rounded to milliseconds
// once: many sub-millisecond children add up instead of each rounding to 0.
func TestCPUTimeIsRoundedOnceNotPerChild(t *testing.T) {
	meter := &cpuMeter{}
	steps := &validationSteps{}
	ctx := withValidationSteps(withCPUMeter(context.Background(), meter), steps)
	stepCtx, end := beginValidationStep(ctx, validationStepTimestamps)
	var total time.Duration
	var perChildMs int64
	for range 40 {
		cmd := exec.Command("/usr/bin/true")
		if err := runCharged(stepCtx, cmd); err != nil {
			t.Fatal(err)
		}
		d := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
		total += d
		perChildMs += d.Milliseconds()
	}
	end()
	if got := *meter.activeComputeMs(); got != total.Milliseconds() {
		t.Fatalf("phase CPU %d ms, children summed %v", got, total)
	}
	if got := *steps.phaseSteps()[validationStepTimestamps].ActiveComputeMs; got != total.Milliseconds() {
		t.Fatalf("step CPU %d ms, children summed %v", got, total)
	}
	if total.Milliseconds() < perChildMs {
		t.Fatal("rounding once lost time")
	}
}

// A new execution of a phase adds to the earlier ones. A counter only one
// side measured stays known (nil+x = x, x+nil = x); it is unknown only when
// neither measured it. Steps accumulate by name, Runs add up, and Attempts
// count executions.
func TestAccumulatePhaseCostKeepsTheKnownSide(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	value := func(p *int64) string {
		if p == nil {
			return "nil"
		}
		return strconv.FormatInt(*p, 10)
	}
	for _, c := range []struct {
		name           string
		previous, next *int64
		want           string
	}{
		{"both unknown", nil, nil, "nil"},
		{"only the retry measured", nil, n(300), "300"},
		{"only the first attempt measured", n(200), nil, "200"},
		{"both measured", n(200), n(300), "500"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := accumulatePhaseCost(
				transcode.PhaseCost{DurationMs: 10, Attempts: 1, ActiveComputeMs: c.previous, NASReadBytes: c.previous},
				transcode.PhaseCost{DurationMs: 5, Attempts: 1, ActiveComputeMs: c.next, NASReadBytes: c.next},
			)
			if value(got.ActiveComputeMs) != c.want || value(got.NASReadBytes) != c.want || got.DurationMs != 15 || got.Attempts != 2 {
				t.Fatalf("got cpu=%s nas=%s %+v, want %s", value(got.ActiveComputeMs), value(got.NASReadBytes), got, c.want)
			}
		})
	}
	first := transcode.PhaseCost{DurationMs: 100, Attempts: 1, Steps: map[string]transcode.PhaseCost{
		validationStepVMAF:    {DurationMs: 60, Attempts: 1, Runs: 7, ActiveComputeMs: n(500)},
		validationStepExtract: {DurationMs: 30, Attempts: 1, Runs: 7},
	}}
	retry := transcode.PhaseCost{DurationMs: 80, Attempts: 1, Steps: map[string]transcode.PhaseCost{
		validationStepVMAF:  {DurationMs: 50, Attempts: 1, Runs: 6, ActiveComputeMs: n(400)},
		validationStepCAMBI: {DurationMs: 40, Attempts: 1, Runs: 6, ActiveComputeMs: n(300)},
	}}
	got := accumulatePhaseCost(first, retry)
	vmaf, extract, cambi := got.Steps[validationStepVMAF], got.Steps[validationStepExtract], got.Steps[validationStepCAMBI]
	if got.Attempts != 2 || vmaf.Attempts != 2 || vmaf.Runs != 13 || vmaf.DurationMs != 110 || *vmaf.ActiveComputeMs != 900 {
		t.Fatalf("vmaf %+v in %+v", vmaf, got)
	}
	if extract.Runs != 7 || extract.ActiveComputeMs != nil || cambi.Runs != 6 || *cambi.ActiveComputeMs != 300 {
		t.Fatalf("steps %+v", got.Steps)
	}
	if first.Steps[validationStepVMAF].Runs != 7 {
		t.Fatal("accumulating changed the earlier cost")
	}
}

// Steps live inside the validation phase's cost, so the job's phases are the
// same with or without them and a sum over the phases counts final validation
// once. They survive a second validation (a resumed job) and the telemetry
// the coordinator reads, whose validation duration comes from timestamps.
func TestValidationStepsDoNotChangeTheJobsPhaseTotals(t *testing.T) {
	jobDir := t.TempDir()
	jobFile := filepath.Join(jobDir, "job.json")
	start := time.Now().Add(-time.Minute).UTC()
	job := &JobRecord{ID: "steps", Status: "running", CreatedAt: start, StartedAt: start}
	job.ValidationStartedAt = start
	job.ValidationFinishedAt = start.Add(30 * time.Second)
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	w := &Worker{cfg: &WorkerConfig{}}
	n := func(v int64) *int64 { return &v }
	for range 2 {
		w.persistPhaseCost(jobDir, jobFile, job, "validation", time.Now(), transcode.PhaseCost{ActiveComputeMs: n(5000), Steps: map[string]transcode.PhaseCost{
			validationStepVMAF:      {DurationMs: 1000, Attempts: 1, Runs: 7, ActiveComputeMs: n(3000)},
			validationStepAlignment: {DurationMs: 30, Attempts: 1, Runs: 7},
		}})
	}
	w.persistPhaseCost(jobDir, jobFile, job, "encode", time.Now(), transcode.PhaseCost{ActiveComputeMs: n(9000)})
	stored, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, costs := range map[string]map[string]transcode.PhaseCost{"stored": stored.PhaseCosts, "telemetry": telemetryFor(stored, time.Now()).PhaseCosts} {
		var cpu int64
		phases := []string{}
		for phase, cost := range costs {
			phases = append(phases, phase)
			if cost.ActiveComputeMs != nil {
				cpu += *cost.ActiveComputeMs
			}
		}
		if cpu != 19000 {
			t.Fatalf("%s: phases %v sum to %d ms of CPU, want the phases' own 19000", name, phases, cpu)
		}
		// The stored record counts both validations; the telemetry counts the
		// phase as one attempt (its duration is the timestamps'), and its
		// steps the same way.
		attempts := map[string]int{"stored": 2, "telemetry": 1}[name]
		validation := costs["validation"]
		if validation.Attempts != attempts || len(validation.Steps) != 2 {
			t.Fatalf("%s: validation %+v", name, validation)
		}
		vmaf, align := validation.Steps[validationStepVMAF], validation.Steps[validationStepAlignment]
		if align.Attempts != attempts || vmaf.DurationMs != 2000 || vmaf.Attempts != attempts || vmaf.Runs != 14 || vmaf.ActiveComputeMs == nil || *vmaf.ActiveComputeMs != 6000 {
			t.Fatalf("%s: vmaf step %+v", name, vmaf)
		}
		if align.DurationMs != 60 || align.Runs != 14 || align.ActiveComputeMs != nil {
			t.Fatalf("%s: alignment step %+v", name, align)
		}
		for phase := range costs {
			if strings.HasPrefix(phase, "validation_") {
				t.Fatalf("%s: a step is a phase of its own: %v", name, phases)
			}
		}
	}
	if telemetryFor(stored, time.Now()).PhaseCosts["validation"].DurationMs != 30000 {
		t.Fatal("validation's duration no longer comes from its timestamps")
	}
	if job.PhaseCosts["validation"].Steps[validationStepVMAF].Runs != 14 || stored.PhaseCosts["validation"].Steps[validationStepVMAF].Attempts != 2 {
		t.Fatal("the in-memory job missed the stored costs")
	}
}

// Telemetry counts a step as one attempt without dropping any of its fields,
// and a step that recorded no attempt is not promoted to one. The job's own
// record keeps its attempts.
func TestOneAttemptStepsKeepsEveryField(t *testing.T) {
	cpu := int64(900)
	hits, misses := int64(3), int64(1)
	steps := map[string]transcode.PhaseCost{
		validationStepVMAF:   {DurationMs: 500, Attempts: 2, Runs: 14, CacheHits: &hits, CacheMisses: &misses, ActiveComputeMs: &cpu},
		validationStepChroma: {DurationMs: 40, Runs: 1},
	}
	got := oneAttemptSteps(steps)
	vmaf := got[validationStepVMAF]
	if vmaf.Attempts != 1 || vmaf.DurationMs != 500 || vmaf.Runs != 14 || vmaf.CacheHits != &hits || vmaf.CacheMisses != &misses || vmaf.ActiveComputeMs != &cpu {
		t.Fatalf("vmaf %+v", vmaf)
	}
	if chroma := got[validationStepChroma]; chroma.Attempts != 0 || chroma.DurationMs != 40 || chroma.Runs != 1 {
		t.Fatalf("chroma %+v", chroma)
	}
	if steps[validationStepVMAF].Attempts != 2 {
		t.Fatal("the job's own steps changed")
	}
	if oneAttemptSteps(nil) != nil {
		t.Fatal("no steps became some")
	}
}
