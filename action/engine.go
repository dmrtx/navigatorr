package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// Engine manages declarative, persistent, multi-step actions.
type Engine struct {
	mu        sync.RWMutex
	deps      EngineDeps
	templates map[string]ActionTemplate
}

// NewEngine creates a new Action Engine.
func NewEngine(deps EngineDeps) *Engine {
	e := &Engine{
		deps:      deps,
		templates: make(map[string]ActionTemplate),
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

// Run creates a new action instance and begins step execution.
func (e *Engine) Run(ctx context.Context, actionName string, inputs map[string]any) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required for action engine")
	}

	tmpl, ok := e.GetTemplate(actionName)
	if !ok {
		return nil, fmt.Errorf("unknown action template: %s", actionName)
	}

	if inputs == nil {
		inputs = make(map[string]any)
	}

	instID := generateActionID(actionName)
	inputsJSON, _ := json.Marshal(inputs)

	inst := store.ActionInstance{
		ID:          instID,
		ActionName:  actionName,
		Status:      StatusPending,
		CurrentStep: 0,
		InputsJSON:  string(inputsJSON),
		OutputsJSON: "{}",
		StateJSON:   "{}",
	}

	if err := e.deps.Store.CreateActionInstance(inst); err != nil {
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

	return e.execute(ctx, &inst, ec, tmpl)
}

// Resume re-activates a paused or waiting action instance.
func (e *Engine) Resume(ctx context.Context, instanceID string, decision string, extraInputs map[string]any) (*ActionResult, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required for action engine")
	}

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

	// If already completed or terminal, return current state
	if inst.Status == StatusCompleted || inst.Status == StatusFailed || inst.Status == StatusCancelled {
		ec := parseExecutionContext(inst, e)
		return buildActionResult(inst, len(tmpl.Steps), ec), nil
	}

	ec := parseExecutionContext(inst, e)
	if decision != "" {
		ec.Decision = decision
	}
	if extraInputs != nil {
		for k, v := range extraInputs {
			ec.Inputs[k] = v
			ec.State[k] = v
		}
	}

	// Reset waiting state before re-entering
	inst.Status = StatusRunning
	inst.WaitingReason = ""
	inst.WaitingCondition = ""
	inst.WaitingOptionsJSON = "[]"
	_ = e.deps.Store.UpdateActionInstance(*inst)

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
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if strings.EqualFold(status, "all") {
		status = ""
	}

	instances, err := e.deps.Store.ListActionInstances(status, limit)
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

// Cancel marks an active action as cancelled.
func (e *Engine) Cancel(ctx context.Context, instanceID, reason string) (*ActionResult, error) {
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

	inst.Status = StatusCancelled
	inst.WaitingReason = reason
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		return nil, fmt.Errorf("updating action instance: %w", err)
	}

	tmpl, _ := e.GetTemplate(inst.ActionName)
	ec := parseExecutionContext(inst, e)
	return buildActionResult(inst, len(tmpl.Steps), ec), nil
}

// execute runs steps sequentially with persistence, idempotency, and wait handling.
func (e *Engine) execute(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, tmpl ActionTemplate) (*ActionResult, error) {
	totalSteps := len(tmpl.Steps)

	// Fetch previously completed steps for idempotency
	loggedSteps, _ := e.deps.Store.GetActionSteps(inst.ID)
	completedStepIndices := make(map[int]bool)
	for _, ls := range loggedSteps {
		if ls.Status == string(StepCompleted) || ls.Status == string(StepSkipped) {
			completedStepIndices[ls.StepIndex] = true
		}
	}

	for stepIdx := inst.CurrentStep; stepIdx < totalSteps; stepIdx++ {
		// Respect context cancellation
		if err := ctx.Err(); err != nil {
			inst.Status = StatusFailed
			inst.ErrorJSON = fmt.Sprintf(`{"error": %q}`, err.Error())
			_ = e.deps.Store.UpdateActionInstance(*inst)
			return buildActionResult(inst, totalSteps, ec), err
		}

		// Idempotency: if step already finished in previous run, skip re-execution
		if completedStepIndices[stepIdx] {
			continue
		}

		step := tmpl.Steps[stepIdx]
		inst.Status = StatusRunning
		inst.CurrentStep = stepIdx
		_ = e.deps.Store.UpdateActionInstance(*inst)

		start := time.Now()
		res, err := step.Run(ctx, ec)
		durationMs := time.Since(start).Milliseconds()

		// Handle step failure
		if err != nil || res.Status == StepFailed {
			errStr := ""
			if err != nil {
				errStr = err.Error()
			} else {
				errStr = res.Error
			}

			inpJSON, _ := json.Marshal(ec.Inputs)
			_ = e.deps.Store.LogActionStep(store.ActionStepLog{
				InstanceID: inst.ID,
				StepIndex:  stepIdx,
				StepName:   step.Name,
				Primitive:  step.Name,
				InputsJSON: string(inpJSON),
				Status:     string(StepFailed),
				Error:      errStr,
				DurationMs: durationMs,
			})

			inst.Status = StatusFailed
			inst.ErrorJSON = fmt.Sprintf(`{"step": %q, "error": %q}`, step.Name, errStr)
			inst.StateJSON = toJSON(ec.State)
			_ = e.deps.Store.UpdateActionInstance(*inst)

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
			inpJSON, _ := json.Marshal(ec.Inputs)
			outJSON, _ := json.Marshal(res.Outputs)

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

			inst.Status = StatusWaitingExternal
			inst.CurrentStep = stepIdx
			inst.WaitingReason = res.WaitingReason
			inst.WaitingCondition = res.WaitingCondition
			inst.StateJSON = toJSON(ec.State)
			inst.OutputsJSON = toJSON(ec.Outputs)
			_ = e.deps.Store.UpdateActionInstance(*inst)

			return buildActionResult(inst, totalSteps, ec), nil
		}

		// Handle decision waiting (e.g. LLM confirmation on trade-offs)
		if res.Status == StepWaitingDecision {
			mergeMap(ec.State, res.Outputs)
			inpJSON, _ := json.Marshal(ec.Inputs)
			outJSON, _ := json.Marshal(res.Outputs)

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

			inst.Status = StatusWaitingDecision
			inst.CurrentStep = stepIdx
			inst.WaitingReason = res.WaitingReason
			inst.WaitingOptionsJSON = toJSON(res.WaitingOptions)
			inst.StateJSON = toJSON(ec.State)
			inst.OutputsJSON = toJSON(ec.Outputs)
			_ = e.deps.Store.UpdateActionInstance(*inst)

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

		inst.CurrentStep = stepIdx + 1
		inst.StateJSON = toJSON(ec.State)
		inst.OutputsJSON = toJSON(ec.Outputs)
		_ = e.deps.Store.UpdateActionInstance(*inst)
	}

	// All steps finished
	inst.Status = StatusCompleted
	inst.CurrentStep = totalSteps
	inst.StateJSON = toJSON(ec.State)
	inst.OutputsJSON = toJSON(ec.Outputs)
	_ = e.deps.Store.UpdateActionInstance(*inst)

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

	return &ActionResult{
		ID:               inst.ID,
		ActionName:       inst.ActionName,
		Status:           inst.Status,
		CurrentStep:      inst.CurrentStep,
		TotalSteps:       totalSteps,
		Inputs:           ec.Inputs,
		Outputs:          ec.Outputs,
		State:            ec.State,
		WaitingReason:    inst.WaitingReason,
		WaitingCondition: inst.WaitingCondition,
		WaitingOptions:   waitingOptions,
		Error:            errStr,
		CreatedAt:        inst.CreatedAt,
		UpdatedAt:        inst.UpdatedAt,
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
