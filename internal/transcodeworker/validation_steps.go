package transcodeworker

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// windowThreads hands each metric pass of final validation (alignment, VMAF,
// CAMBI, chroma) its threads when the pass starts. The job's metric threads
// are what concurrency windows got each before, times concurrency; a pass
// gets them split between the windows still to finish, at most concurrency of
// them. Windows run 3+3+1 for seven, so the last one used to run on a third of
// the job's threads while the rest sat idle; now it gets them all.
//
// The split never exceeds the job's threads: the windows still to finish only
// fall, so every pass running now started with a divisor at least the number
// of windows running now, and those passes together hold at most the whole.
// Thread counts do not change libvmaf or psnr results. An explicit
// metric_threads fixes every pass, as before.
type windowThreads struct {
	budget, concurrency, windows int
	fixed                        bool
	finished                     atomic.Int64
}

func (w *Worker) newWindowThreads(concurrency, windows int) *windowThreads {
	perPass := w.metricThreads(concurrency)
	return &windowThreads{budget: perPass * max(concurrency, 1), concurrency: max(concurrency, 1), windows: windows, fixed: w != nil && w.cfg != nil && w.cfg.MetricThreads > 0}
}

// pass is the threads of a metric pass starting now.
func (t *windowThreads) pass() int {
	if t.fixed {
		return t.budget / t.concurrency
	}
	share := min(t.concurrency, t.windows-int(t.finished.Load()))
	return max(1, t.budget/max(share, 1))
}

// done marks one window finished, measured or not.
func (t *windowThreads) done() { t.finished.Add(1) }

// The steps of final validation whose cost is reported, each summed over the
// windows that ran it. They are the steps of the worker's validation phase
// cost (PhaseCost.Steps), never phases of their own.
const (
	validationStepExtract    = "extract"
	validationStepTimestamps = "timestamps"
	validationStepAlignment  = "alignment"
	validationStepVMAF       = "vmaf"
	validationStepCAMBI      = "cambi"
	validationStepSSIM       = "ssim"
	validationStepChroma     = "chroma"
)

// validationSteps sums the wall time and the children's CPU time of each step
// of final validation over its windows, so the cost of one step (the CAMBI
// pass, say) can be weighed against the rest. Windows run concurrently, so
// step wall times overlap and are not a total; CPU time is each child's own
// rusage, as for cpuMeter. It rides on the validation context.
type validationSteps struct {
	mu    sync.Mutex
	steps map[string]*validationStepCost
}

// Times are summed at full resolution and rounded to milliseconds once, so a
// step of many short children (a probe per window) loses nothing per child.
type validationStepCost struct {
	wall, cpu   time.Duration
	cpuMeasured bool
	runs        int
}

type validationStepsKey struct{}

// validationStepRun is the step a child runs for, on that child's context.
type validationStepRun struct {
	steps *validationSteps
	name  string
}

type validationStepRunKey struct{}

func withValidationSteps(ctx context.Context, s *validationSteps) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, validationStepsKey{}, s)
}

// beginValidationStep starts one run of step name for the validation steps on
// ctx, if any. Children run under the returned context charge their CPU time
// to the step as well as to the phase's meter; end records the run's wall time.
func beginValidationStep(ctx context.Context, name string) (stepCtx context.Context, end func()) {
	s, ok := ctx.Value(validationStepsKey{}).(*validationSteps)
	if !ok || s == nil {
		return ctx, func() {}
	}
	start := time.Now()
	return context.WithValue(ctx, validationStepRunKey{}, &validationStepRun{steps: s, name: name}), func() {
		s.add(name, func(c *validationStepCost) {
			c.wall += time.Since(start)
			c.runs++
		})
	}
}

// chargeValidationStep charges a finished child to the step on ctx, if any.
func chargeValidationStep(ctx context.Context, ps *os.ProcessState) {
	run, ok := ctx.Value(validationStepRunKey{}).(*validationStepRun)
	if !ok || run == nil || ps == nil {
		return
	}
	d := ps.UserTime() + ps.SystemTime()
	run.steps.add(run.name, func(c *validationStepCost) {
		c.cpu += d
		c.cpuMeasured = true
	})
}

func (s *validationSteps) add(name string, update func(*validationStepCost)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.steps == nil {
		s.steps = map[string]*validationStepCost{}
	}
	c := s.steps[name]
	if c == nil {
		c = &validationStepCost{}
		s.steps[name] = c
	}
	update(c)
}

// phaseSteps are the steps that ran, for the validation phase's cost: wall
// time summed over windows, one attempt (this validation), Runs the windows
// that ran the step, and CPU time only where a child was measured.
func (s *validationSteps) phaseSteps() map[string]transcode.PhaseCost {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]transcode.PhaseCost, len(s.steps))
	for name, c := range s.steps {
		if c.runs == 0 {
			continue
		}
		cost := transcode.PhaseCost{DurationMs: c.wall.Milliseconds(), Attempts: 1, Runs: c.runs}
		if c.cpuMeasured {
			cost.ActiveComputeMs = knownCount(c.cpu.Milliseconds())
		}
		out[name] = cost
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
