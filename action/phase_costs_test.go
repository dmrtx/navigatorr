package action

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// A current worker reports active_compute_ms per phase; an older worker's
// payload has no such field and must stay null (unknown), never 0.
func TestWorkerActiveComputeIsMirroredAndOlderWorkerStaysNull(t *testing.T) {
	var current, older map[string]transcode.PhaseCost
	if err := json.Unmarshal([]byte(`{"encode":{"duration_ms":30000,"attempts":1,"active_compute_ms":95000}}`), &current); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"encode":{"duration_ms":30000,"attempts":1}}`), &older); err != nil {
		t.Fatal(err)
	}
	project := func(prefix string, costs map[string]transcode.PhaseCost) map[string]any {
		ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
		mirrorWorkerPhaseCosts(ec, prefix, costs)
		raw, _ := json.Marshal(ec.Outputs["phase_costs"])
		var projected map[string]map[string]any
		_ = json.Unmarshal(raw, &projected)
		return projected[prefix+"encode"]
	}
	if got := project("worker_", current); got["active_compute_ms"] != float64(95000) {
		t.Fatalf("measured CPU time not mirrored: %v", got)
	}
	got := project("worker_benchmark_", older)
	if v, ok := got["active_compute_ms"]; !ok || v != nil {
		t.Fatalf("older worker must project active_compute_ms as null, got %v (present=%v)", v, ok)
	}
	// The coordinator's own measurements stay CPU-unknown.
	ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
	now := time.Now()
	NewEngine(EngineDeps{Config: &config.Config{}}).recordCoordinatorPhase(ec, "coordinator_probe", now, now.Add(time.Second))
	raw, _ := json.Marshal(phaseCostMap(ec)["coordinator_probe"])
	var coordinator map[string]any
	_ = json.Unmarshal(raw, &coordinator)
	if v, ok := coordinator["active_compute_ms"]; !ok || v != nil {
		t.Fatalf("coordinator cost must keep active_compute_ms null: %s", raw)
	}
}

func TestWorkerObservationsMirrorPhaseCostsIntoOutputs(t *testing.T) {
	ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
	e := NewEngine(EngineDeps{Config: &config.Config{}})
	e.observeTranscodeStatus(ec, transcode.JobStatus{Status: transcode.StatusCompleted, JobTelemetry: transcode.JobTelemetry{PhaseCosts: map[string]transcode.PhaseCost{"encode": {DurationMs: 42, Attempts: 1}}}})
	costs, ok := ec.Outputs["phase_costs"].(map[string]any)
	if !ok || len(costs) != 1 {
		t.Fatalf("worker output evidence missing: %+v", ec.Outputs)
	}
	e.observeTranscodeStatus(ec, transcode.JobStatus{Status: transcode.StatusCompleted})
	if len(phaseCostMap(ec)) != 1 {
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

// The worker's per-step final validation costs (extract, timestamps,
// alignment, VMAF, CAMBI, SSIM, chroma) reach the action nested under its
// validation phase, with their wall time, attempts (validations), runs
// (windows) and CPU time. The phases beside each other are the same, and add
// up to the same CPU time, with or without the steps.
func TestWorkerValidationStepCostsAreMirrored(t *testing.T) {
	cpu := func(n int64) *int64 { return &n }
	steps := map[string]transcode.PhaseCost{
		"extract":    {DurationMs: 61000, Attempts: 1, Runs: 7, ActiveComputeMs: cpu(300000)},
		"timestamps": {DurationMs: 900, Attempts: 1, Runs: 7, ActiveComputeMs: cpu(700)},
		"alignment":  {DurationMs: 8000, Attempts: 1, Runs: 7, ActiveComputeMs: cpu(30000)},
		"vmaf":       {DurationMs: 250000, Attempts: 1, Runs: 7, ActiveComputeMs: cpu(500000)},
		"cambi":      {DurationMs: 260000, Attempts: 1, Runs: 7, ActiveComputeMs: cpu(700000)},
		"ssim":       {DurationMs: 40000, Attempts: 1, Runs: 7, ActiveComputeMs: cpu(90000)},
		"chroma":     {DurationMs: 30000, Attempts: 1, Runs: 2, ActiveComputeMs: cpu(60000)},
	}
	mirror := func(withSteps bool) map[string]map[string]any {
		validation := transcode.PhaseCost{DurationMs: 240000, Attempts: 1, ActiveComputeMs: cpu(1900000)}
		if withSteps {
			validation.Steps = steps
		}
		ec := &ExecutionContext{State: map[string]any{}, Outputs: map[string]any{}}
		e := NewEngine(EngineDeps{Config: &config.Config{}})
		e.observeTranscodeStatus(ec, transcode.JobStatus{Status: transcode.StatusCompleted, JobTelemetry: transcode.JobTelemetry{PhaseCosts: map[string]transcode.PhaseCost{
			"encode":     {DurationMs: 900000, Attempts: 1, ActiveComputeMs: cpu(7000000)},
			"validation": validation,
		}}})
		raw, _ := json.Marshal(ec.State["phase_costs"])
		var costs map[string]map[string]any
		if err := json.Unmarshal(raw, &costs); err != nil {
			t.Fatal(err)
		}
		return costs
	}
	total := func(costs map[string]map[string]any) (sum float64, phases []string) {
		for phase, c := range costs {
			phases = append(phases, phase)
			if v, ok := c["active_compute_ms"].(float64); ok {
				sum += v
			}
		}
		slices.Sort(phases)
		return sum, phases
	}
	with, without := mirror(true), mirror(false)
	withCPU, withPhases := total(with)
	withoutCPU, withoutPhases := total(without)
	if withCPU != withoutCPU || withCPU != 8900000 || !slices.Equal(withPhases, withoutPhases) {
		t.Fatalf("steps changed the phases: %v %v vs %v %v", withPhases, withCPU, withoutPhases, withoutCPU)
	}
	validation := with["worker_validation"]
	if validation["attempts"] != float64(1) || validation["active_compute_ms"] != float64(1900000) || validation["timing_scope"] != "phase_wall; phases may overlap" {
		t.Fatalf("validation %v", validation)
	}
	// A step's duration adds up its windows, which overlap: it says so, and
	// is never labelled as the phase's elapsed wall time.
	if cambi, _ := validation["steps"].(map[string]any)["cambi"].(map[string]any); cambi["timing_scope"] != "step_window_wall_sum; runs overlap" || cambi["duration_ms"].(float64) <= validation["duration_ms"].(float64) {
		t.Fatalf("cambi step %v", cambi)
	}
	mirrored, _ := validation["steps"].(map[string]any)
	if len(mirrored) != len(steps) {
		t.Fatalf("steps %v", mirrored)
	}
	for name, want := range steps {
		c, _ := mirrored[name].(map[string]any)
		if c["active_compute_ms"] != float64(*want.ActiveComputeMs) || c["attempts"] != float64(1) || c["runs"] != float64(want.Runs) || c["duration_ms"] != float64(want.DurationMs) || c["provenance"] != "worker_measured" {
			t.Fatalf("%s: %v", name, c)
		}
	}
	if _, ok := without["worker_validation"]["steps"]; ok {
		t.Fatal("a validation without steps mirrored some")
	}
	if _, ok := with["worker_encode"]["runs"]; ok {
		t.Fatal("a phase without runs mirrored them")
	}
}
