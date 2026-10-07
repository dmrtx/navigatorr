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
// snapshot rather than adding it again; benchmark and encode namespaces keep
// independent scopes and preserve coordinator measurements.
func mirrorWorkerPhaseCosts(ec *ExecutionContext, prefix string, costs map[string]transcode.PhaseCost) {
	if len(costs) == 0 {
		return
	}
	target := phaseCostMap(ec)
	for phase, cost := range costs {
		if cost.DurationMs < 0 || cost.Attempts <= 0 {
			continue
		}
		target[prefix+phase] = map[string]any{"duration_ms": cost.DurationMs, "attempts": cost.Attempts, "nas_read_bytes": cost.NASReadBytes, "nas_written_bytes": cost.NASWrittenBytes, "cache_hits": cost.CacheHits, "cache_misses": cost.CacheMisses, "active_compute_ms": nil, "provenance": "worker_measured", "timing_scope": "phase_wall; phases may overlap"}
	}
	if ec.Outputs != nil {
		ec.Outputs["phase_costs"] = target
	}
}
