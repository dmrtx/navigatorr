package action

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// CoordinatorPhaseCost counts only returned synchronous invocations. It is
// elapsed wall time (including synchronous I/O), not CPU time. Human/external
// waits between invocations and uncheckpointed process interruptions are not
// measured. Nested phases overlap; these values are never an operation total.
type CoordinatorPhaseCost struct {
	transcode.PhaseCost
	Provenance      string `json:"provenance"`
	TimingScope     string `json:"timing_scope"`
	ActiveComputeMs *int64 `json:"active_compute_ms"`
	FirstStartedAt  string `json:"first_started_at"`
	LastFinishedAt  string `json:"last_finished_at"`
}

func phaseCostMap(ec *ExecutionContext) map[string]any {
	if ec.State == nil {
		ec.State = map[string]any{}
	}
	costs, ok := ec.State["phase_costs"].(map[string]any)
	if !ok {
		costs = map[string]any{}
		ec.State["phase_costs"] = costs
	}
	return costs
}

func (e *Engine) recordCoordinatorPhase(ec *ExecutionContext, phase string, start, finish time.Time) {
	// A backwards clock cannot establish an interval: omit it instead of inventing
	// zero work. Missing completed observations remain unknown after restart.
	if finish.Before(start) {
		return
	}
	costs := phaseCostMap(ec)
	var cost CoordinatorPhaseCost
	raw, _ := json.Marshal(costs[phase])
	_ = json.Unmarshal(raw, &cost)
	cost.DurationMs += finish.Sub(start).Milliseconds()
	cost.Attempts++
	cost.Provenance = "coordinator_measured"
	cost.TimingScope = "returned_invocation_wall; excludes between-invocation waits; phases may overlap"
	if cost.FirstStartedAt == "" {
		cost.FirstStartedAt = start.UTC().Format(time.RFC3339Nano)
	}
	cost.LastFinishedAt = finish.UTC().Format(time.RFC3339Nano)
	costs[phase] = cost
	if ec.Outputs != nil {
		ec.Outputs["phase_costs"] = costs
	}
}

func (e *Engine) measureCoordinatorPhase(phase string, run func(context.Context, *ExecutionContext) (StepResult, error)) func(context.Context, *ExecutionContext) (StepResult, error) {
	return func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		start := e.now()
		result, err := run(ctx, ec)
		e.recordCoordinatorPhase(ec, phase, start, e.now())
		if result.Outputs == nil {
			result.Outputs = map[string]any{}
		}
		if costs, ok := ec.State["phase_costs"]; ok {
			result.Outputs["phase_costs"] = costs
		}
		return result, err
	}
}

// Worker costs are cumulative snapshots. Polling replaces the same source's
// snapshot rather than adding it again; namespaces keep independent scopes
// and preserve coordinator measurements.
func mirrorWorkerPhaseCosts(ec *ExecutionContext, prefix string, costs map[string]transcode.PhaseCost) {
	if len(costs) == 0 {
		return
	}
	target := phaseCostMap(ec)
	for phase, cost := range costs {
		if cost.DurationMs < 0 || cost.Attempts <= 0 {
			continue
		}
		target[prefix+phase] = workerPhaseCost(cost, workerPhaseTimingScope)
	}
	if ec.Outputs != nil {
		ec.Outputs["phase_costs"] = target
	}
}

const (
	workerPhaseTimingScope = "phase_wall; phases may overlap"
	// A step's duration is the wall time of each of its runs (windows)
	// added up. Runs overlap, so with more than one it is not elapsed time
	// and can exceed its phase's.
	workerStepRunsTimingScope = "step_window_wall_sum; runs overlap"
	// A step that ran once (or did not count its runs) took this wall time.
	workerStepTimingScope = "step_wall; inside its phase"
)

// stepTimingScope is the scope of one step's duration.
func stepTimingScope(step transcode.PhaseCost) string {
	if step.Runs > 1 {
		return workerStepRunsTimingScope
	}
	return workerStepTimingScope
}

// workerPhaseCost is one mirrored worker cost. Its steps (final validation's
// extract, VMAF, CAMBI...) stay nested under it: they are part of the phase,
// so the phases listed side by side never count them twice.
func workerPhaseCost(cost transcode.PhaseCost, timingScope string) map[string]any {
	out := map[string]any{"duration_ms": cost.DurationMs, "attempts": cost.Attempts, "nas_read_bytes": cost.NASReadBytes, "nas_written_bytes": cost.NASWrittenBytes, "cache_hits": cost.CacheHits, "cache_misses": cost.CacheMisses, "active_compute_ms": cost.ActiveComputeMs, "provenance": "worker_measured", "timing_scope": timingScope}
	if cost.Runs > 0 {
		out["runs"] = cost.Runs
	}
	if len(cost.Steps) > 0 {
		steps := make(map[string]any, len(cost.Steps))
		for name, step := range cost.Steps {
			if step.DurationMs < 0 || step.Attempts <= 0 {
				continue
			}
			steps[name] = workerPhaseCost(step, stepTimingScope(step))
		}
		if len(steps) > 0 {
			out["steps"] = steps
		}
	}
	return out
}
