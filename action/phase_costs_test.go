package action

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
)

func decodeCoordinatorCost(t *testing.T, raw any) CoordinatorPhaseCost {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var cost CoordinatorPhaseCost
	if err = json.Unmarshal(b, &cost); err != nil {
		t.Fatal(err)
	}
	return cost
}

func TestCoordinatorPhaseCostsPersistAcrossRestartWithoutCountingDecisionWait(t *testing.T) {
	st := setupTestStore(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	inventoryCalls := 0
	register := func(e *Engine) {
		e.RegisterTemplate(ActionTemplate{Name: "measured_flow", Steps: []StepDefinition{
			{Name: "inventory", Run: e.measureCoordinatorPhase("coordinator_inventory", func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
				inventoryCalls++
				now = now.Add(2 * time.Second)
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"inventory": "frozen"}}, nil
			})},
			{Name: "promotion", Run: e.measureCoordinatorPhase("coordinator_promotion", func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
				if ec.Decision == "" {
					now = now.Add(3 * time.Second)
					return StepResult{Status: StepWaitingDecision, WaitingCondition: "review", WaitingReason: "Review synthetic promotion", WaitingOptions: []WaitingOption{{Decision: "approve", Description: "Approve fixture"}}}, nil
				}
				now = now.Add(4 * time.Second)
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"promoted": true}}, nil
			})},
		}})
	}
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Now: func() time.Time { return now }})
	register(e)
	waiting, err := e.Run(context.Background(), "measured_flow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != StatusWaitingDecision {
		t.Fatalf("measurement changed lifecycle: %+v", waiting)
	}
	before := phaseCostMap(&ExecutionContext{State: waiting.State})
	inventory := decodeCoordinatorCost(t, before["coordinator_inventory"])
	if inventory.DurationMs != 2000 || inventory.Attempts != 1 || inventory.ActiveComputeMs != nil || inventory.NASReadBytes != nil {
		t.Fatalf("false or missing evidence: %+v", inventory)
	}
	now = now.Add(24 * time.Hour)
	restarted := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Now: func() time.Time { return now }})
	register(restarted)
	completed, err := restarted.Resume(context.Background(), waiting.ID, "approve", nil)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != StatusCompleted || inventoryCalls != 1 {
		t.Fatalf("restart replayed work or changed lifecycle: %s calls=%d", completed.Status, inventoryCalls)
	}
	costs := phaseCostMap(&ExecutionContext{State: completed.State})
	promotion := decodeCoordinatorCost(t, costs["coordinator_promotion"])
	if promotion.DurationMs != 7000 || promotion.Attempts != 2 {
		t.Fatalf("human wait counted or persisted attempt lost: %+v", promotion)
	}
	if promotion.FirstStartedAt != "2026-10-07T00:00:02Z" || promotion.LastFinishedAt != "2026-10-08T00:00:09Z" {
		t.Fatalf("observation bounds changed on restart: %+v", promotion)
	}
	if decodeCoordinatorCost(t, costs["coordinator_inventory"]).DurationMs != 2000 {
		t.Fatal("inventory measurement replayed")
	}
}

func TestCoordinatorPhaseWrapperPreservesErrorsAndUnknownClockEvidence(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	e := NewEngine(EngineDeps{Config: &config.Config{}, Now: func() time.Time { return now }})
	ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
	expected := errors.New("fixture error")
	run := e.measureCoordinatorPhase("coordinator_inventory", func(context.Context, *ExecutionContext) (StepResult, error) {
		now = now.Add(time.Second)
		return StepResult{Status: StepFailed, Error: "failed fixture", Outputs: map[string]any{"kept": true}}, expected
	})
	result, err := run(context.Background(), ec)
	if err != expected || result.Status != StepFailed || result.Error != "failed fixture" || result.Outputs["kept"] != true {
		t.Fatal("instrumentation changed effect result")
	}
	cost := decodeCoordinatorCost(t, phaseCostMap(ec)["coordinator_inventory"])
	if cost.DurationMs != 1000 {
		t.Fatal("failed invocation not measured")
	}
	e.recordCoordinatorPhase(ec, "untrustworthy", now, now.Add(-time.Second))
	if _, ok := phaseCostMap(ec)["untrustworthy"]; ok {
		t.Fatal("unknown clock fabricated zero duration")
	}
}

func TestWorkerPhaseSnapshotsDoNotDuplicatePollsOrOverwriteCoordinatorCosts(t *testing.T) {
	ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
	e := NewEngine(EngineDeps{Config: &config.Config{}})
	now := time.Now()
	e.recordCoordinatorPhase(ec, "coordinator_probe", now, now.Add(time.Second))
	cost := transcode.PhaseCost{DurationMs: 2500, Attempts: 1}
	for range 3 {
		mirrorWorkerPhaseCosts(ec, "worker_", map[string]transcode.PhaseCost{"probe": cost})
	}
	mirrorWorkerPhaseCosts(ec, "worker_benchmark_", map[string]transcode.PhaseCost{"probe": {DurationMs: 1500, Attempts: 1}})
	costs := phaseCostMap(ec)
	worker := costs["worker_probe"].(map[string]any)
	if worker["duration_ms"] != int64(2500) || worker["attempts"] != 1 {
		t.Fatal("poll snapshots added repeatedly")
	}
	raw, _ := json.Marshal(worker)
	var projected map[string]any
	_ = json.Unmarshal(raw, &projected)
	if projected["nas_read_bytes"] != nil || projected["active_compute_ms"] != nil {
		t.Fatal("unknown I/O or CPU cost fabricated")
	}
	if decodeCoordinatorCost(t, costs["coordinator_probe"]).DurationMs != 1000 {
		t.Fatal("worker replaced coordinator measurement")
	}
	if len(costs) != 3 {
		t.Fatalf("phase scopes collided: %+v", costs)
	}
}

func TestWorkerAndBenchmarkObservationsMirrorPhaseCostsIntoOutputs(t *testing.T) {
	ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
	e := NewEngine(EngineDeps{Config: &config.Config{}})
	e.observeTranscodeStatus(ec, transcode.JobStatus{Status: transcode.StatusCompleted, JobTelemetry: transcode.JobTelemetry{PhaseCosts: map[string]transcode.PhaseCost{"encode": {DurationMs: 42, Attempts: 1}}}})
	e.observeBenchmarkStatus(ec, transcode.BenchmarkStatus{Status: transcode.StatusCompleted, SearchBudgetSeconds: 600, PhaseCosts: map[string]transcode.PhaseCost{"samples": {DurationMs: 12, Attempts: 1}}})
	costs, ok := ec.Outputs["phase_costs"].(map[string]any)
	if !ok || len(costs) != 2 {
		t.Fatalf("worker output evidence missing: %+v", ec.Outputs)
	}
	if ec.State["search_budget_seconds"] != 600 {
		t.Fatal("known search cap omitted")
	}
	e.observeTranscodeStatus(ec, transcode.JobStatus{Status: transcode.StatusCompleted})
	if len(phaseCostMap(ec)) != 2 {
		t.Fatal("older worker fabricated or erased measurement")
	}
}

func TestCoordinatorMeasurementPreservesPolicySkipOutputs(t *testing.T) {
	e := NewEngine(EngineDeps{Config: &config.Config{}})
	ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
	measured := e.measureCoordinatorPhase("coordinator_preflight", func(context.Context, *ExecutionContext) (StepResult, error) {
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": "unsupported_source", "original_intact": true}}, nil
	})
	result, err := measured(context.Background(), ec)
	if err != nil || result.Status != StepCompleted || result.Outputs["skipped"] != true || result.Outputs["original_intact"] != true || result.Outputs["reason_code"] != "unsupported_source" {
		t.Fatalf("measurement changed preservation policy result: %+v %v", result, err)
	}
	if _, ok := result.Outputs["phase_costs"]; !ok {
		t.Fatal("skipped policy lost phase measurement outputs")
	}
}
