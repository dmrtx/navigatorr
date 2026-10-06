package action

import (
	"context"
	"fmt"

	"github.com/jakenesler/navigatorr/store"
)

// Commands are durable receipts, not media jobs. HTTP admission never waits for
// an execution lease, NAS IO, or a worker. The service reconciler owns execution.
func (e *Engine) registerMaintenanceCommand() {
	e.RegisterTemplate(ActionTemplate{Name: "maintenance_command", Version: 1, ImmutableInputs: true, AutoReconcile: true,
		Steps: []StepDefinition{{Name: "apply_command", Run: e.applyMaintenanceCommand}}})
}

func (e *Engine) QueueMaintenanceCommand(ctx context.Context, inputs map[string]any, key string) (*ActionResult, error) {
	id := getString(inputs, "id")
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil || inst == nil || inst.ActionName == "maintenance_command" {
		return nil, fmt.Errorf("job is unavailable")
	}
	r, err := e.Enqueue(WithOrigin(ctx, "web"), "maintenance_command", inputs, "web-command:"+key)
	if err == nil {
		e.WakeReconciler()
	}
	return r, err
}

func (e *Engine) applyMaintenanceCommand(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	id, kind := getString(ec.Inputs, "id"), getString(ec.Inputs, "kind")
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	if (kind == "retry" && RequiresWorker(inst.ActionName)) || kind == "resume" && ResumeRequiresWorker(inst, getString(ec.Inputs, "decision")) || kind == "candidate" && getString(ec.Inputs, "decision") == "accept_loss" || kind == "reconfigure" {
		if err := e.CheckWorkerAdmission(ctx); err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, nil
		}
	}
	var result *ActionResult
	switch kind {
	case "resume":
		result, err = e.Resume(ctx, id, getString(ec.Inputs, "decision"), nil)
	case "retry":
		if inst.Status != store.ActionStatusFailed {
			result, err = e.Status(ctx, id)
		} else {
			result, err = e.Retry(ctx, id)
		}
	case "cancel":
		result, err = e.Cancel(ctx, id, getString(ec.Inputs, "reason"))
	case "candidate":
		result, err = e.ResumeReviewedCandidate(ctx, id, getString(ec.Inputs, "candidate_id"), getString(ec.Inputs, "decision_version"), getString(ec.Inputs, "decision"))
	case "clean":
		result, err = e.CleanTranscodeBackup(ctx, id)
	case "discard_duplicate":
		result, err = e.DiscardDuplicateTranscodeBackup(ctx, id)
	case "discard":
		result, err = e.DiscardTranscodeBackup(ctx, id)
	case "reconfigure":
		settings, ok := ec.Inputs["settings"].(map[string]any)
		if !ok {
			return StepResult{Status: StepFailed, Error: "invalid settings"}, nil
		}
		result, err = e.ReconfigureBatch(ctx, id, getString(ec.Inputs, "scope"), getString(ec.Inputs, "candidate_id"), getString(ec.Inputs, "selection_version"), getString(ec.Inputs, "decision_version"), ec.InstanceID, settings)
	default:
		err = fmt.Errorf("unsupported maintenance command")
	}
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"id": id, "job_status": result.Status}}, nil
}
