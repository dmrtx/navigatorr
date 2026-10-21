package action

import (
	"context"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

// The CRF sample search is gone. Actions that ran it stay readable from their
// stored state, but the engine no longer executes them: one that is not
// finished when the engine next meets it is ended as failed, and nothing it
// had planned touches the media.

// removedSearchMessage is why such an action ended.
const removedSearchMessage = "This operation used the removed sample search; start it again to use the current recipe. Original preserved."

// removedBenchmarkAction is the sample test action: it has no template any
// more, and a new request for it is refused as an input error.
const removedBenchmarkAction = "benchmark_transcode"

// removedBenchmarkMessage refuses a new benchmark_transcode request.
const removedBenchmarkMessage = "sample tests were removed: the sample search no longer picks encode settings. Run transcode_media with a goal (mode) instead"

// removedSearchAction reports an action that belongs to the removed search:
// a sample test, a goal frozen under policy v2 or v3, a search-tuned request
// (priority, shared calibration, a metric) or a fixed-profile encode of an
// optimizing profile.
func removedSearchAction(inst *store.ActionInstance, ec *ExecutionContext) bool {
	if inst.ActionName == removedBenchmarkAction {
		return true
	}
	if inst.ActionName != "transcode_media" && inst.ActionName != "transcode_batch" {
		return false
	}
	for _, key := range searchOnlyInputs {
		if inputSet(ec.Inputs[key]) {
			return true
		}
	}
	if snapshot := frozenPolicyOf(ec.State["mode_policy"]); snapshot != nil {
		return snapshot.Version != ModePolicyV4
	}
	// A fixed profile that optimized is the only other way to have searched.
	return getString(ec.Inputs, "mode") == "" && getBool(ec.State, "optimization_enabled")
}

// retireRemovedSearch ends a removed-search action as failed with
// removedSearchMessage, then stops any worker job it still owns. The action
// is ended first, as Cancel does, so nothing more of it is admitted even when
// the worker cannot confirm the stop.
func (e *Engine) retireRemovedSearch(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext) (*ActionResult, error) {
	tmpl, _ := e.GetTemplate(inst.ActionName)
	step := ""
	if inst.CurrentStep >= 0 && inst.CurrentStep < len(tmpl.Steps) {
		step = tmpl.Steps[inst.CurrentStep].Name
	}
	inst.Status = StatusFailed
	inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON = "", "", "[]"
	inst.ErrorJSON = fmt.Sprintf(`{"step": %q, "error": %q}`, step, removedSearchMessage)
	if err := e.updateInstance(ctx, inst); err != nil {
		return nil, err
	}
	_ = e.deps.Store.LogActionEnriched("action_failed", "", inst.ID, inst.InputsJSON, "step="+step+" error="+removedSearchMessage, removedSearchMessage, inst.ID, 0)
	result := buildActionResult(inst, len(tmpl.Steps), ec)
	if err := e.cancelRemoteActionWork(ctx, ec); err != nil {
		return result, fmt.Errorf("action %s ended, but its worker job was not confirmed stopped: %w", inst.ID, err)
	}
	return result, nil
}

// frozenPolicyOf reads a stored mode policy snapshot without judging it. It is
// nil when the state holds none or it cannot be read.
func frozenPolicyOf(raw any) *ModePolicySnapshot {
	if raw == nil {
		return nil
	}
	var p ModePolicySnapshot
	if !reencode(raw, &p) || strings.TrimSpace(p.Version) == "" {
		return nil
	}
	return &p
}

// stepPlanEncode plans a goal's first encode: a quality ladder's first rung,
// or the size budget's encode. A fixed profile has nothing to plan.
func (e *Engine) stepPlanEncode(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getString(ec.Inputs, "mode") == "" || getBool(ec.State, "skip_transcode") {
		return StepResult{Status: StepSkipped}, nil
	}
	if e.deps.Transcode == nil {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, nil
	}
	if getString(ec.State, "original_sha256") == "" {
		return StepResult{Status: StepFailed, Error: "original integrity baseline missing (fail closed)"}, nil
	}
	cleanPath := getString(ec.State, "resolved_path")
	if cleanPath == "" {
		cleanPath = getString(ec.Inputs, "path")
	}
	if snapshot, err := freezeModePolicy(ec); err == nil {
		if result, ended := endUnreachableSizeBudget(ec, snapshot, getSourceReport(ec.State["source_report"]), getInt64(ec.State, "original_size"), siblingEvidenceOf(ec.State)); ended {
			return result, nil
		}
	}
	if result, handled := e.startSizeBudgetLadder(ctx, ec, cleanPath); handled {
		return result, nil
	}
	if result, handled := e.startQualityLadder(ctx, ec, cleanPath); handled {
		return result, nil
	}
	return StepResult{Status: StepFailed, Error: "this goal's recipe could not be planned (fail closed)"}, nil
}

// stepRetired is the step the sample search's wait used to occupy. The step
// list keeps its positions because stored actions point into it by index.
func stepRetired(context.Context, *ExecutionContext) (StepResult, error) {
	return StepResult{Status: StepSkipped}, nil
}
