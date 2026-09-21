package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Engine manages declarative, persistent, multi-step actions.
type Engine struct {
	mu             sync.RWMutex
	deps           EngineDeps
	templates      map[string]ActionTemplate
	reconcilerOnce sync.Once
	reconcilerDone chan struct{}
}

// NewEngine creates a new Action Engine.
func NewEngine(deps EngineDeps) *Engine {
	e := &Engine{
		deps:           deps,
		templates:      make(map[string]ActionTemplate),
		reconcilerDone: make(chan struct{}),
	}
	e.registerBuiltinTemplates()
	return e
}

// Deps returns the engine dependencies.
func (e *Engine) Deps() EngineDeps {
	return e.deps
}

// AllowDestructive reports whether destructive actions are globally allowed.
func (e *Engine) AllowDestructive() bool {
	return e.deps.Config != nil && e.deps.Config.AllowDestructive
}

// RegisterTemplate registers an action template.
func (e *Engine) RegisterTemplate(template ActionTemplate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.templates[template.Name] = template
}

// GetTemplate returns an action template by name.
func (e *Engine) GetTemplate(name string) (ActionTemplate, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	t, ok := e.templates[name]
	return t, ok
}

// ListTemplates returns all registered action template names and descriptions.
func (e *Engine) ListTemplates() []map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	res := make([]map[string]string, 0, len(e.templates))
	for _, t := range e.templates {
		res = append(res, map[string]string{
			"name":        t.Name,
			"description": t.Description,
		})
	}
	return res
}

// Catalog returns the discovery catalog for all registered action workflows.
func (e *Engine) Catalog() []ActionCatalogEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()

	entries := make([]ActionCatalogEntry, 0, len(e.templates))
	for _, t := range e.templates {
		steps := make([]string, 0, len(t.Steps))
		for _, s := range t.Steps {
			steps = append(steps, s.Name)
		}
		reqInputs := t.RequiredInputs
		if reqInputs == nil {
			reqInputs = []string{}
		}
		optInputs := t.OptionalInputs
		if optInputs == nil {
			optInputs = []string{}
		}
		entries = append(entries, ActionCatalogEntry{
			Name:            t.Name,
			Version:         t.Version,
			Description:     t.Description,
			RequiredInputs:  reqInputs,
			OptionalInputs:  optInputs,
			Steps:           steps,
			Destructive:     t.Destructive,
			ImmutableInputs: t.ImmutableInputs,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// Run creates a new action instance and begins step execution.
// If an idempotencyKey is provided and an active (non-terminal) instance exists with the same
// actionName + idempotencyKey, the existing action is returned instead of creating a new one.
func (e *Engine) Run(ctx context.Context, actionName string, inputs map[string]any, idempotencyKeys ...string) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required for action engine")
	}

	tmpl, ok := e.GetTemplate(actionName)
	if !ok {
		return nil, fmt.Errorf("unknown action template: %s", actionName)
	}

	inputCopy := make(map[string]any, len(inputs))
	for key, value := range inputs {
		inputCopy[key] = value
	}
	inputs = inputCopy

	var idempotencyKey string
	if len(idempotencyKeys) > 0 {
		idempotencyKey = strings.TrimSpace(idempotencyKeys[0])
	}
	if idempotencyKey == "" {
		if k, ok := inputs["idempotency_key"].(string); ok {
			idempotencyKey = strings.TrimSpace(k)
		}
	}

	if actionName == "promote_transcode_candidate" {
		var err error
		idempotencyKey, err = promotionIdempotency(inputs)
		if err != nil {
			return nil, err
		}
		if existing, err := e.deps.Store.FindActionByIdempotencyKey(actionName, idempotencyKey); err != nil {
			return nil, err
		} else if existing != nil {
			return e.existingPromotion(ctx, existing, tmpl, inputs)
		}
	}
	// Idempotency check: if non-terminal action with same name and key exists, return it
	if idempotencyKey != "" {
		if existing, err := e.deps.Store.FindActiveActionByIdempotencyKey(actionName, idempotencyKey); err == nil && existing != nil {
			ec := parseExecutionContext(existing, e)
			return buildActionResult(existing, len(tmpl.Steps), ec), nil
		}
	}

	instID := generateActionID(actionName)
	inputsJSON, _ := json.Marshal(inputs)

	inst := store.ActionInstance{
		ID:             instID,
		ActionName:     actionName,
		Status:         StatusPending,
		CurrentStep:    0,
		InputsJSON:     string(inputsJSON),
		OutputsJSON:    "{}",
		StateJSON:      "{}",
		IdempotencyKey: idempotencyKey,
	}

	if err := e.deps.Store.CreateActionInstance(inst); err != nil {
		// A concurrent caller can win the unique idempotency key between the
		// lookup and INSERT. Return that same workflow, never another promotion.
		if actionName == "promote_transcode_candidate" {
			if existing, lookupErr := e.deps.Store.FindActionByIdempotencyKey(actionName, idempotencyKey); lookupErr == nil && existing != nil {
				return e.existingPromotion(ctx, existing, tmpl, inputs)
			}
		}
		return nil, fmt.Errorf("creating action instance: %w", err)
	}

	ec := &ExecutionContext{
		InstanceID: instID,
		ActionName: actionName,
		Inputs:     inputs,
		State:      make(map[string]any),
		Outputs:    make(map[string]any),
		Engine:     e,
	}

	claimCtx, release, err := e.claimExecution(ctx, inst.ID, true)
	if err != nil {
		return nil, err
	}
	defer release()
	stored, err := e.deps.Store.GetActionInstance(inst.ID)
	if err != nil {
		return nil, err
	}
	return e.execute(claimCtx, stored, ec, tmpl)
}

// Retry re-runs a failed action from its last safe step without repeating confirmed side effects.
func (e *Engine) Retry(ctx context.Context, instanceID string) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required for action engine")
	}

	ctx, release, err := e.claimExecution(ctx, instanceID, true)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(instanceID)
	if err != nil {
		return nil, fmt.Errorf("getting action instance %s: %w", instanceID, err)
	}
	if inst == nil {
		return nil, fmt.Errorf("action instance not found: %s", instanceID)
	}

	if inst.Status != StatusFailed {
		return nil, fmt.Errorf("only failed actions can be retried (current status: %s)", inst.Status)
	}

	tmpl, ok := e.GetTemplate(inst.ActionName)
	if !ok {
		return nil, fmt.Errorf("unknown action template: %s", inst.ActionName)
	}

	// The durable checkpoint is authoritative. Audit logs can be missing after
	// a crash and must never advance execution past state that was not saved.
	resumeStep := inst.CurrentStep
	if resumeStep < 0 || resumeStep >= len(tmpl.Steps) {
		return nil, fmt.Errorf("invalid retry checkpoint %d", resumeStep)
	}
	ec := parseExecutionContext(inst, e)
	resumeStep, err = e.prepareRemoteRetry(ctx, inst, ec, tmpl, resumeStep)
	if err != nil {
		return nil, err
	}
	inst.StateJSON, inst.OutputsJSON = toJSON(ec.State), toJSON(ec.Outputs)

	inst.CurrentStep = resumeStep
	inst.Status = StatusRunning
	inst.ErrorJSON = ""
	if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
		return nil, saveErr
	}

	// Record retry in audit log
	_ = e.deps.Store.LogActionEnriched(
		"action_retry",
		"",
		inst.ID,
		inst.InputsJSON,
		fmt.Sprintf("retrying failed action %s from step %d (%s)", inst.ID, resumeStep, tmpl.Steps[resumeStep].Name),
		"",
		inst.ID,
		0,
	)

	return e.execute(ctx, inst, ec, tmpl)
}

// Resume re-activates a paused or waiting action instance.
func (e *Engine) Resume(ctx context.Context, instanceID string, decision string, extraInputs map[string]any) (*ActionResult, error) {
	return e.resume(ctx, instanceID, decision, extraInputs, false)
}

func (e *Engine) resume(ctx context.Context, instanceID string, decision string, extraInputs map[string]any, automatic bool) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required for action engine")
	}

	ctx, release, err := e.claimExecution(ctx, instanceID, !automatic)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(instanceID)
	if err != nil {
		return nil, fmt.Errorf("getting action instance %s: %w", instanceID, err)
	}
	if inst == nil {
		return nil, fmt.Errorf("action instance not found: %s", instanceID)
	}

	tmpl, ok := e.GetTemplate(inst.ActionName)
	if !ok {
		return nil, fmt.Errorf("unknown action template: %s", inst.ActionName)
	}

	if len(extraInputs) != 0 && tmpl.ImmutableInputs {
		return nil, fmt.Errorf("%s inputs are immutable after action creation; resume accepts only a decision", inst.ActionName)
	}

	// If already completed or terminal, return current state
	if inst.Status == StatusCompleted || inst.Status == StatusFailed || inst.Status == StatusCancelled {
		ec := parseExecutionContext(inst, e)
		return buildActionResult(inst, len(tmpl.Steps), ec), nil
	}

	ec := parseExecutionContext(inst, e)
	if (automatic && !e.shouldReconcile(inst, tmpl, ec)) || (inst.Status == StatusWaitingDecision && decision == "") {
		return buildActionResult(inst, len(tmpl.Steps), ec), nil
	}
	if decision != "" {
		ec.Decision = decision
	}
	if extraInputs != nil {
		for k, v := range extraInputs {
			ec.Inputs[k] = v
			ec.State[k] = v
		}
	}

	inst.InputsJSON = toJSON(ec.Inputs)
	// Reset waiting state before re-entering
	inst.Status = StatusRunning
	inst.WaitingReason = ""
	inst.WaitingCondition = ""
	inst.WaitingOptionsJSON = "[]"
	if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
		return nil, saveErr
	}

	return e.execute(ctx, inst, ec, tmpl)
}

// Status returns the current status and step log of an action instance.
func (e *Engine) Status(ctx context.Context, instanceID string) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}

	inst, err := e.deps.Store.GetActionInstance(instanceID)
	if err != nil {
		return nil, fmt.Errorf("getting action instance: %w", err)
	}
	if inst == nil {
		return nil, fmt.Errorf("action instance not found: %s", instanceID)
	}

	tmpl, _ := e.GetTemplate(inst.ActionName)
	totalSteps := len(tmpl.Steps)

	ec := parseExecutionContext(inst, e)
	return buildActionResult(inst, totalSteps, ec), nil
}

// List returns action instances matching the optional status filter.
func (e *Engine) List(ctx context.Context, status string, limit int) ([]ActionResult, error) {
	return e.ListPaged(ctx, status, limit, 0)
}

// ListPaged returns action instances matching the optional status filter with offset pagination.
func (e *Engine) ListPaged(ctx context.Context, status string, limit, offset int) ([]ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if strings.EqualFold(status, "all") {
		status = ""
	}

	instances, err := e.deps.Store.ListActionInstancesPaged(status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("listing action instances: %w", err)
	}

	res := make([]ActionResult, 0, len(instances))
	for _, inst := range instances {
		tmpl, _ := e.GetTemplate(inst.ActionName)
		totalSteps := len(tmpl.Steps)
		ec := parseExecutionContext(&inst, e)
		res = append(res, *buildActionResult(&inst, totalSteps, ec))
	}
	return res, nil
}

// Cancel marks an active action as cancelled and propagates cancellation to
// remote transcode work. A transcode_batch cancellation cascades to every
// non-terminal child action so already-admitted worker jobs are stopped too.
func (e *Engine) Cancel(ctx context.Context, instanceID, reason string) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}

	requestCtx := ctx
	ctx, release, err := e.claimExecution(ctx, instanceID, true)
	if err != nil {
		return nil, err
	}
	released := false
	releaseLease := func() {
		if !released {
			released = true
			release()
		}
	}
	defer releaseLease()

	inst, err := e.deps.Store.GetActionInstance(instanceID)
	if err != nil {
		return nil, fmt.Errorf("getting action instance: %w", err)
	}
	if inst == nil {
		return nil, fmt.Errorf("action instance not found: %s", instanceID)
	}

	inst.Status = StatusCancelled
	inst.WaitingReason = reason
	inst.WaitingCondition = ""
	inst.WaitingOptionsJSON = "[]"
	if err := e.updateInstance(ctx, inst); err != nil {
		return nil, fmt.Errorf("updating action instance: %w", err)
	}

	tmpl, _ := e.GetTemplate(inst.ActionName)
	ec := parseExecutionContext(inst, e)
	result := buildActionResult(inst, len(tmpl.Steps), ec)

	// Persist the parent/local cancellation before contacting the worker. This
	// closes admission immediately: no new child submit may start after cancel
	// is confirmed locally, even if the remote cancellation later needs
	// reconciliation.
	var cancelErrors []string
	if err := e.cancelRemoteActionWork(ctx, ec); err != nil {
		cancelErrors = append(cancelErrors, err.Error())
	}

	// A batch used to deliberately leave accepted children running. That makes
	// "cancel the batch" operationally misleading and can leave the worker slots
	// occupied by work the caller explicitly abandoned. Collect the children
	// while the parent lease is held, then release it before recursively
	// cancelling children so child execution/admission cannot deadlock on the
	// parent lease.
	var childIDs []string
	if inst.ActionName == "transcode_batch" {
		items, lerr := e.deps.Store.ListTranscodeBatchItems(instanceID)
		if lerr != nil {
			cancelErrors = append(cancelErrors, fmt.Sprintf("listing batch children: %v", lerr))
		} else {
			seen := make(map[string]struct{})
			for _, item := range items {
				if item.ChildActionID != "" {
					if _, ok := seen[item.ChildActionID]; !ok {
						seen[item.ChildActionID] = struct{}{}
						childIDs = append(childIDs, item.ChildActionID)
					}
					continue
				}
				// Fail-safe for a partially persisted batch relation: if a worker
				// job identity was saved but its child action ID was not, still stop
				// the remote job rather than orphaning active work.
				if item.JobID != "" && e.deps.Transcode != nil {
					if cerr := e.cancelTranscodeJobConfirmed(ctx, item.JobID); cerr != nil {
						cancelErrors = append(cancelErrors, fmt.Sprintf("cancelling orphan batch job %s: %v", item.JobID, cerr))
					}
				}
			}
		}
	}

	releaseLease()

	for _, childID := range childIDs {
		child, gerr := e.deps.Store.GetActionInstanceIfExists(childID)
		if gerr != nil {
			cancelErrors = append(cancelErrors, fmt.Sprintf("reading child action %s: %v", childID, gerr))
			continue
		}
		if child == nil || child.Status == StatusCompleted || child.Status == StatusFailed || child.Status == StatusCancelled {
			continue
		}
		childReason := "parent transcode batch " + instanceID + " cancelled"
		if strings.TrimSpace(reason) != "" {
			childReason += ": " + reason
		}
		if _, cerr := e.Cancel(requestCtx, childID, childReason); cerr != nil {
			cancelErrors = append(cancelErrors, fmt.Sprintf("cancelling child %s: %v", childID, cerr))
		}
	}

	if len(cancelErrors) > 0 {
		return result, fmt.Errorf("action %s was cancelled locally, but remote cancellation was not fully confirmed: %s", instanceID, strings.Join(cancelErrors, "; "))
	}
	return result, nil
}

// cancelRemoteActionWork cancels every remote identity owned by one action.
// It recovers a transcode job ID from the batch-item relation when an older or
// partially-persisted child row does not carry job_id in its state.
func (e *Engine) cancelRemoteActionWork(ctx context.Context, ec *ExecutionContext) error {
	if e.deps.Transcode == nil || ec == nil {
		return nil
	}
	var errs []string

	benchID := getString(ec.State, "benchmark_job_id")
	if benchID == "" {
		benchID = getString(ec.Outputs, "benchmark_job_id")
	}
	if benchID != "" && !getBool(ec.State, "benchmark_done") {
		if err := e.cancelBenchmarkJobConfirmed(ctx, benchID); err != nil {
			errs = append(errs, fmt.Sprintf("benchmark %s: %v", benchID, err))
		}
	}

	jobID := getString(ec.State, "job_id")
	if jobID == "" {
		jobID = getString(ec.Outputs, "job_id")
	}
	if jobID == "" {
		jobID = getString(ec.State, "external_reference")
	}
	if jobID == "" {
		jobID = getString(ec.Outputs, "external_reference")
	}
	if jobID == "" && e.deps.Store != nil && ec.InstanceID != "" {
		item, err := e.deps.Store.FindTranscodeBatchItemByChildActionID(ec.InstanceID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("recovering worker job identity: %v", err))
		} else if item != nil {
			jobID = item.JobID
		}
	}
	if jobID != "" {
		if err := e.cancelTranscodeJobConfirmed(ctx, jobID); err != nil {
			errs = append(errs, fmt.Sprintf("transcode %s: %v", jobID, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// cancelTranscodeJobConfirmed sends cancel once. Only when the HTTP result is
// transport-uncertain do we reconcile with Status before deciding whether a
// second idempotent cancel is needed. This preserves the existing no-blind-
// retry rule while making cancellation operationally reliable.
func (e *Engine) cancelTranscodeJobConfirmed(ctx context.Context, jobID string) error {
	err := e.deps.Transcode.Cancel(ctx, jobID)
	if err == nil {
		return nil
	}
	if !transcode.IsTransportUncertain(err) {
		return err
	}

	st, serr := e.deps.Transcode.Status(ctx, jobID)
	if serr != nil {
		return fmt.Errorf("cancel result uncertain (%v); status reconciliation failed: %w", err, serr)
	}
	switch st.Status {
	case transcode.StatusCancelled, transcode.StatusCompleted, transcode.StatusFailed:
		return nil
	case transcode.StatusQueued, transcode.StatusRunning:
		if rerr := e.deps.Transcode.Cancel(ctx, jobID); rerr != nil {
			return fmt.Errorf("cancel result uncertain; worker still reports %s and confirmed retry failed: %w", st.Status, rerr)
		}
		return nil
	default:
		return fmt.Errorf("cancel result uncertain; worker returned unexpected status %q", st.Status)
	}
}

func (e *Engine) cancelBenchmarkJobConfirmed(ctx context.Context, jobID string) error {
	err := e.deps.Transcode.BenchmarkCancel(ctx, jobID)
	if err == nil {
		return nil
	}
	if !transcode.IsTransportUncertain(err) {
		return err
	}

	st, serr := e.deps.Transcode.BenchmarkStatus(ctx, jobID)
	if serr != nil {
		return fmt.Errorf("benchmark cancel result uncertain (%v); status reconciliation failed: %w", err, serr)
	}
	switch st.Status {
	case transcode.StatusCancelled, transcode.StatusCompleted, transcode.StatusFailed:
		return nil
	case transcode.StatusQueued, transcode.StatusRunning:
		if rerr := e.deps.Transcode.BenchmarkCancel(ctx, jobID); rerr != nil {
			return fmt.Errorf("benchmark cancel result uncertain; worker still reports %s and confirmed retry failed: %w", st.Status, rerr)
		}
		return nil
	default:
		return fmt.Errorf("benchmark cancel result uncertain; worker returned unexpected status %q", st.Status)
	}
}

// execute runs steps sequentially with persistence, idempotency, and wait handling.
func (e *Engine) execute(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, tmpl ActionTemplate) (*ActionResult, error) {
	totalSteps := len(tmpl.Steps)

	// current_step and state are one durable checkpoint; step logs are audit
	// records, never recovery authority.

	for stepIdx := inst.CurrentStep; stepIdx < totalSteps; stepIdx++ {
		// Respect context cancellation
		if err := ctx.Err(); err != nil {
			if tmpl.AutoReconcile && len(ec.State) > 0 {
				inst.Status = StatusWaitingExternal
				inst.CurrentStep = stepIdx
				inst.WaitingCondition = "worker_reconciling"
				inst.WaitingReason = "Coordinator interrupted; workflow will reconcile automatically"
				e.scheduleReconcile(ec, inst.WaitingCondition)
				inst.StateJSON, inst.OutputsJSON = toJSON(ec.State), toJSON(ec.State)
			} else {
				inst.Status = StatusFailed
				inst.ErrorJSON = fmt.Sprintf(`{"error": %q}`, err.Error())
			}
			if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
				return nil, saveErr
			}
			return buildActionResult(inst, totalSteps, ec), err
		}

		step := tmpl.Steps[stepIdx]
		inst.Status = StatusRunning
		inst.CurrentStep = stepIdx
		inst.StateJSON = toJSON(ec.State)
		inst.OutputsJSON = toJSON(ec.Outputs)
		if err := e.updateInstance(ctx, inst); err != nil {
			return nil, err
		}

		start := time.Now()
		res, err := step.Run(ctx, ec)
		inst.InputsJSON = toJSON(ec.Inputs)
		durationMs := time.Since(start).Milliseconds()

		if step.Name == "validate_result" || step.Name == "accept_result" {
			ec.State["coordinator_validation_duration_ms"] = getInt64(ec.State, "coordinator_validation_duration_ms") + durationMs
			ec.State["validation_duration_ms"] = getInt64(ec.State, "coordinator_validation_duration_ms") + getInt64(ec.State, "worker_validation_duration_ms")
			if res.Outputs == nil {
				res.Outputs = make(map[string]any)
			}
			res.Outputs["validation_duration_ms"] = ec.State["validation_duration_ms"]
			res.Outputs["coordinator_validation_duration_ms"] = ec.State["coordinator_validation_duration_ms"]
		}
		if ctx.Err() != nil && tmpl.AutoReconcile {
			// A caller timeout or service shutdown suspends the coordinator; only
			// action_cancel is allowed to cancel a remote job.
			err = nil
			res.Status = StepWaitingExternal
			res.WaitingCondition = "worker_reconciling"
			res.WaitingReason = "Coordinator request interrupted; workflow will reconcile automatically"
		}

		// Handle step failure
		if err != nil || res.Status == StepFailed {
			errStr := ""
			if err != nil {
				errStr = err.Error()
			} else {
				errStr = res.Error
			}

			if len(res.Outputs) > 0 {
				mergeMap(ec.State, res.Outputs)
			}
			ec.Outputs = ec.State

			inpJSON, _ := json.Marshal(ec.Inputs)
			outJSON, _ := json.Marshal(res.Outputs)

			inst.Status = StatusFailed
			inst.ErrorJSON = fmt.Sprintf(`{"step": %q, "error": %q}`, step.Name, errStr)
			inst.StateJSON = toJSON(ec.State)
			inst.OutputsJSON = toJSON(ec.State)
			if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
				return nil, saveErr
			}
			_ = e.deps.Store.LogActionStep(store.ActionStepLog{
				InstanceID:  inst.ID,
				StepIndex:   stepIdx,
				StepName:    step.Name,
				Primitive:   step.Name,
				InputsJSON:  string(inpJSON),
				OutputsJSON: string(outJSON),
				Status:      string(StepFailed),
				Error:       errStr,
				DurationMs:  durationMs,
			})

			_ = e.deps.Store.LogActionEnriched(
				"action_failed",
				"",
				inst.ID,
				string(inpJSON),
				fmt.Sprintf("step=%s error=%s", step.Name, errStr),
				errStr,
				inst.ID,
				durationMs,
			)

			return buildActionResult(inst, totalSteps, ec), nil
		}

		// Handle external waiting (e.g. torrent downloading)
		if res.Status == StepWaitingExternal {
			mergeMap(ec.State, res.Outputs)
			mergeMap(ec.Outputs, res.Outputs)
			inpJSON, _ := json.Marshal(ec.Inputs)
			outJSON, _ := json.Marshal(res.Outputs)

			e.scheduleReconcile(ec, res.WaitingCondition)
			inst.Status = StatusWaitingExternal
			inst.CurrentStep = stepIdx
			inst.WaitingReason = res.WaitingReason
			inst.WaitingCondition = res.WaitingCondition
			inst.StateJSON = toJSON(ec.State)
			inst.OutputsJSON = toJSON(ec.Outputs)
			if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
				return nil, saveErr
			}
			_ = e.deps.Store.LogActionStep(store.ActionStepLog{
				InstanceID:  inst.ID,
				StepIndex:   stepIdx,
				StepName:    step.Name,
				Primitive:   step.Name,
				InputsJSON:  string(inpJSON),
				OutputsJSON: string(outJSON),
				Status:      string(StepWaitingExternal),
				DurationMs:  durationMs,
			})

			return buildActionResult(inst, totalSteps, ec), nil
		}

		// Handle decision waiting (e.g. LLM confirmation on trade-offs)
		if res.Status == StepWaitingDecision {
			mergeMap(ec.State, res.Outputs)
			mergeMap(ec.Outputs, res.Outputs)
			inpJSON, _ := json.Marshal(ec.Inputs)
			outJSON, _ := json.Marshal(res.Outputs)

			delete(ec.State, "next_poll_at")
			delete(ec.Outputs, "next_poll_at")
			inst.Status = StatusWaitingDecision
			inst.CurrentStep = stepIdx
			inst.WaitingReason = res.WaitingReason
			inst.WaitingOptionsJSON = toJSON(res.WaitingOptions)
			inst.StateJSON = toJSON(ec.State)
			inst.OutputsJSON = toJSON(ec.Outputs)
			if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
				return nil, saveErr
			}
			_ = e.deps.Store.LogActionStep(store.ActionStepLog{
				InstanceID:  inst.ID,
				StepIndex:   stepIdx,
				StepName:    step.Name,
				Primitive:   step.Name,
				InputsJSON:  string(inpJSON),
				OutputsJSON: string(outJSON),
				Status:      string(StepWaitingDecision),
				DurationMs:  durationMs,
			})

			return buildActionResult(inst, totalSteps, ec), nil
		}

		// Step completed or skipped
		mergeMap(ec.State, res.Outputs)
		mergeMap(ec.Outputs, res.Outputs)

		inpJSON, _ := json.Marshal(ec.Inputs)
		outJSON, _ := json.Marshal(res.Outputs)
		stepStatus := StepCompleted
		if res.Status == StepSkipped {
			stepStatus = StepSkipped
		}

		inst.CurrentStep = stepIdx + 1
		inst.StateJSON = toJSON(ec.State)
		inst.OutputsJSON = toJSON(ec.Outputs)
		if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
			return nil, saveErr
		}
		_ = e.deps.Store.LogActionStep(store.ActionStepLog{
			InstanceID:  inst.ID,
			StepIndex:   stepIdx,
			StepName:    step.Name,
			Primitive:   step.Name,
			InputsJSON:  string(inpJSON),
			OutputsJSON: string(outJSON),
			Status:      string(stepStatus),
			DurationMs:  durationMs,
		})
	}

	// All steps finished
	delete(ec.State, "next_poll_at")
	delete(ec.Outputs, "next_poll_at")
	if getBool(ec.State, "transcode_done") {
		ec.State["transcode_status"] = "completed"
		ec.Outputs["transcode_status"] = "completed"
	}
	inst.WaitingReason, inst.WaitingCondition, inst.WaitingOptionsJSON = "", "", "[]"
	inst.Status = StatusCompleted
	inst.CurrentStep = totalSteps
	inst.StateJSON = toJSON(ec.State)
	inst.OutputsJSON = toJSON(ec.Outputs)
	if saveErr := e.updateInstance(ctx, inst); saveErr != nil {
		return nil, saveErr
	}

	_ = e.deps.Store.LogActionEnriched(
		"action_completed",
		"",
		inst.ID,
		inst.InputsJSON,
		inst.OutputsJSON,
		"",
		inst.ID,
		0,
	)

	return buildActionResult(inst, totalSteps, ec), nil
}

// Helpers

func generateActionID(actionName string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	cleanName := strings.ReplaceAll(strings.ToLower(actionName), "_", "-")
	return fmt.Sprintf("act-%s-%x", cleanName, hex.EncodeToString(b))
}

func parseExecutionContext(inst *store.ActionInstance, e *Engine) *ExecutionContext {
	inputs := make(map[string]any)
	if inst.InputsJSON != "" {
		_ = json.Unmarshal([]byte(inst.InputsJSON), &inputs)
	}

	state := make(map[string]any)
	if inst.StateJSON != "" {
		_ = json.Unmarshal([]byte(inst.StateJSON), &state)
	}

	outputs := make(map[string]any)
	if inst.OutputsJSON != "" {
		_ = json.Unmarshal([]byte(inst.OutputsJSON), &outputs)
	}

	return &ExecutionContext{
		InstanceID: inst.ID,
		ActionName: inst.ActionName,
		Inputs:     inputs,
		State:      state,
		Outputs:    outputs,
		Engine:     e,
	}
}

func buildActionResult(inst *store.ActionInstance, totalSteps int, ec *ExecutionContext) *ActionResult {
	var waitingOptions []WaitingOption
	if inst.WaitingOptionsJSON != "" {
		_ = json.Unmarshal([]byte(inst.WaitingOptionsJSON), &waitingOptions)
	}

	errStr := ""
	if inst.ErrorJSON != "" {
		var errMap map[string]any
		if err := json.Unmarshal([]byte(inst.ErrorJSON), &errMap); err == nil {
			if msg, ok := errMap["error"].(string); ok {
				errStr = msg
			}
		}
		if errStr == "" {
			errStr = inst.ErrorJSON
		}
	}

	var durationMs int64
	if inst.CreatedAt != "" && inst.UpdatedAt != "" {
		if tStart, err1 := time.Parse(time.RFC3339, inst.CreatedAt); err1 == nil {
			if tEnd, err2 := time.Parse(time.RFC3339, inst.UpdatedAt); err2 == nil {
				if inst.Status != StatusCompleted && inst.Status != StatusFailed && inst.Status != StatusCancelled {
					tEnd = time.Now()
				}
				durationMs = tEnd.Sub(tStart).Milliseconds()
				if durationMs < 0 {
					durationMs = 0
				}
			}
		}
	}

	return &ActionResult{
		ID:                   inst.ID,
		ActionName:           inst.ActionName,
		Status:               inst.Status,
		CurrentStep:          inst.CurrentStep,
		TotalSteps:           totalSteps,
		Inputs:               ec.Inputs,
		Outputs:              ec.Outputs,
		State:                ec.State,
		WaitingReason:        inst.WaitingReason,
		WaitingCondition:     inst.WaitingCondition,
		WaitingOptions:       waitingOptions,
		Error:                errStr,
		IdempotencyKey:       inst.IdempotencyKey,
		DurationMs:           durationMs,
		WallDurationMs:       durationMs,
		QueueDurationMs:      optionalDuration(ec.State, "queue_duration_ms"),
		EncodeDurationMs:     optionalDuration(ec.State, "encode_duration_ms"),
		ValidationDurationMs: optionalDuration(ec.State, "validation_duration_ms"),
		ReconcileLagMs:       optionalDuration(ec.State, "reconcile_lag_ms"),
		CreatedAt:            inst.CreatedAt,
		UpdatedAt:            inst.UpdatedAt,
	}
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func mergeMap(dest, src map[string]any) {
	for k, v := range src {
		dest[k] = v
	}
}
