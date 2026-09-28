package action

import "fmt"

// A new action/recipe must not silently restart an exhausted quality search.
// This gate only applies to new admissions, never tracking an accepted job.
func (e *Engine) reviewBenchmarkSearch(ec *ExecutionContext) *StepResult {
	if getBool(ec.State, "benchmark_search_reviewed") || e.deps.Store == nil {
		return nil
	}
	if getBool(ec.State, "benchmark_search_review_required") {
		switch ec.Decision {
		case "run_once_after_review":
			ec.Decision = ""
			ec.State["benchmark_search_reviewed"] = true
			delete(ec.State, "benchmark_search_review_required")
			return nil
		case "cancel", "reject":
			ec.Decision = ""
			return &StepResult{Status: StepCancelled, Outputs: map[string]any{"note": "Search stopped before submission; original unchanged."}}
		}
	}
	ids, err := e.deps.Store.UnsuccessfulBenchmarks(getString(ec.State, "original_sha256"), ec.InstanceID)
	if err != nil {
		return &StepResult{Status: StepFailed, Error: fmt.Sprintf("checking previous benchmark outcomes: %v", err)}
	}
	if len(ids) < 2 {
		return nil
	}
	ec.State["benchmark_search_review_required"] = true
	ec.State["previous_unsuccessful_benchmarks"] = ids
	return &StepResult{
		Status:           StepWaitingDecision,
		WaitingCondition: "benchmark_search_review",
		WaitingReason:    "Two benchmark rounds for these source bytes already found no acceptable candidate. Review their evidence before another round; changing CRF, recipe or action ID does not reset this limit. Direct encoding remains available with optimization disabled, without claiming a quality pass.",
		WaitingOptions: []WaitingOption{
			{Decision: "cancel", Description: "Stop this search and keep the original."},
			{Decision: "run_once_after_review", Description: "After reviewing the prior evidence, explicitly authorize this action's displayed recipe for one additional bounded benchmark. Do not choose this automatically."},
		},
	}
}
