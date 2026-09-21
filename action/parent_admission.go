package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// admissionLease holds a narrow lease on a child's parent action for the
// duration of one remote admission (policy re-read + persisted submit outcome).
// It is intentionally only taken when a child is about to submit; ordinary
// polls, status reads and hashes never take it, so sibling work is not
// serialized.
//
// ctx is the admission context used for persistence and HTTP: for an acquired
// lease it is the parent lease context (so loss of the parent renewal cancels
// the child mutation), and for an inherited lease it is the enclosing child
// context. A standalone workflow keeps its own context.
type admissionLease struct {
	parentID  string
	owner     string
	ctx       context.Context
	release   func()
	inherited bool
}

// Close releases the lease. An inherited parent lease belongs to an enclosing
// execution and must not be released by the child.
func (l *admissionLease) Close() {
	if l == nil || l.release == nil || l.inherited {
		return
	}
	l.release()
}

// Context returns the context that must be used for admission mutations and
// remote calls.
func (l *admissionLease) Context(fallback context.Context) context.Context {
	if l != nil && l.ctx != nil {
		return l.ctx
	}
	return fallback
}

func leaseOwnerFromContext(ctx context.Context, actionID string) (string, bool) {
	if ctx == nil || actionID == "" {
		return "", false
	}
	owner, ok := ctx.Value(actionLeaseOwnerKey{actionID: actionID}).(string)
	return owner, ok && owner != ""
}

// resolveParentBatchID returns the durable transcode_batch parent of a child
// action. known is true whenever a relationship exists, even if the parent row
// cannot be read afterwards; callers treat that as fail-closed. Identifiers are
// never inferred by splitting separator characters.
func (e *Engine) resolveParentBatchID(ec *ExecutionContext) (parentID string, known bool, err error) {
	if ec == nil {
		return "", false, nil
	}
	explicit := explicitParentLink(ec)
	realID, found, err := e.realParentRelation(ec)
	if err != nil {
		return "", true, err
	}
	if found {
		if explicit != "" && explicit != realID {
			return "", true, fmt.Errorf("child %s parent link %q does not match its transcode_batch item relation %q (fail closed)", ec.InstanceID, explicit, realID)
		}
		return realID, true, nil
	}
	if explicit != "" {
		return explicit, true, nil
	}
	return "", false, nil
}

func explicitParentLink(ec *ExecutionContext) string {
	if v := getString(ec.Inputs, "parent_action_id"); v != "" {
		return v
	}
	return getString(ec.State, "parent_action_id")
}

// realParentRelation recovers the parent from the persisted batch item, first
// via the direct child_action_id column, then via the exact constructed child
// idempotency key. This covers children created before child_action_id was
// persisted and pre-existing (legacy) rows that never carried a parent input.
func (e *Engine) realParentRelation(ec *ExecutionContext) (string, bool, error) {
	if e.deps.Store == nil {
		return "", false, nil
	}
	if item, err := e.deps.Store.FindTranscodeBatchItemByChildActionID(ec.InstanceID); err != nil {
		return "", true, err
	} else if item != nil {
		return item.BatchID, true, nil
	}
	key, err := e.childIdempotencyKey(ec.InstanceID)
	if err != nil {
		return "", true, err
	}
	if strings.HasPrefix(key, "batch-") {
		item, err := e.deps.Store.FindTranscodeBatchItemByChildKey(key)
		if err != nil {
			return "", true, err
		}
		if item != nil {
			return item.BatchID, true, nil
		}
	}
	return "", false, nil
}

// childIdempotencyKey resolves the child's persisted idempotency key. A child
// row that does not exist yet yields an empty key (auxiliary lookup); real
// store errors are returned so a legacy child is never silently treated as
// parentless.
func (e *Engine) childIdempotencyKey(instanceID string) (string, error) {
	if e.deps.Store == nil || instanceID == "" {
		return "", nil
	}
	inst, err := e.deps.Store.GetActionInstanceIfExists(instanceID)
	if err != nil {
		return "", fmt.Errorf("resolving child idempotency key for %s: %w", instanceID, err)
	}
	if inst == nil {
		return "", nil
	}
	return inst.IdempotencyKey, nil
}

// parentPolicy reports the durable control state of a parent batch. Any read
// error or a non-batch parent is returned as an error so callers fail closed.
// A parent waiting for a user decision blocks *new* admissions reversibly while
// accepted jobs keep being tracked.
func (e *Engine) parentPolicy(parentID string) (string, error) {
	if e.deps.Store == nil {
		return "", errors.New("store is required to evaluate the parent policy (fail closed)")
	}
	parent, err := e.deps.Store.GetActionInstance(parentID)
	if err != nil || parent == nil {
		return "", fmt.Errorf("parent action %s is unavailable (fail closed): %w", parentID, err)
	}
	if parent.ActionName != "transcode_batch" {
		return "", fmt.Errorf("parent action %s is not a transcode_batch (fail closed)", parentID)
	}
	if parent.Status == StatusCancelled {
		return "cancelled", nil
	}
	pec := parseExecutionContext(parent, e)
	if getBool(pec.State, "cancel_requested") || getBool(pec.Inputs, "cancel_requested") {
		return "cancelled", nil
	}
	if getBool(pec.State, "paused") {
		return "paused", nil
	}
	if parent.Status == StatusWaitingDecision {
		return "waiting_decision", nil
	}
	return "", nil
}

// beginAdmission resolves the parent link and, for an actual admission, holds
// the parent's execution lease while the policy is re-read and the child's
// submit outcome is persisted. It returns handled=true with a terminal/waiting
// StepResult when the caller must not contact the worker.
//
// It never waits for a busy parent: a scheduler already executing the parent
// holds that lease, and waiting would deadlock child -> parent.
func (e *Engine) beginAdmission(ctx context.Context, ec *ExecutionContext, phase string) (*admissionLease, StepResult, bool) {
	parentID, known, err := e.resolveParentBatchID(ec)
	if err != nil {
		return nil, stepFailClosed(fmt.Errorf("%s parent link check failed (fail closed): %w", phase, err)), true
	}
	if !known {
		return &admissionLease{ctx: ctx}, StepResult{}, false
	}

	// Reuse the parent lease when this child runs inside the parent's own
	// scheduler execution (the parent lease token is present in ctx).
	if owner, inherited := leaseOwnerFromContext(ctx, parentID); inherited {
		policy, perr := e.parentPolicy(parentID)
		if perr != nil {
			return nil, stepFailClosed(perr), true
		}
		if policy != "" {
			return nil, e.parentBlockedResult(phase, policy), true
		}
		return &admissionLease{parentID: parentID, owner: owner, ctx: ctx, inherited: true}, StepResult{}, false
	}

	// Narrow admission guard: acquire the parent lease without waiting while the
	// child keeps its own lease. A busy parent means another execution owns it.
	leaseCtx, release, cerr := e.claimExecution(ctx, parentID, false)
	if cerr != nil {
		if errors.Is(cerr, errActionBusy) {
			return nil, StepResult{
				Status:           StepWaitingExternal,
				WaitingCondition: "parent_busy",
				WaitingReason:    fmt.Sprintf("Parent action %s is executing; deferring %s admission", parentID, phase),
			}, true
		}
		return nil, stepFailClosed(fmt.Errorf("acquiring parent admission lease: %w", cerr)), true
	}
	policy, perr := e.parentPolicy(parentID)
	if perr != nil {
		release()
		return nil, stepFailClosed(perr), true
	}
	if policy != "" {
		release()
		return nil, e.parentBlockedResult(phase, policy), true
	}
	owner, _ := leaseOwnerFromContext(leaseCtx, parentID)
	return &admissionLease{parentID: parentID, owner: owner, ctx: leaseCtx, release: release}, StepResult{}, false
}

// parentLeaseHeld verifies ownership immediately before a remote submit. A
// failed renewal means another executor owns the parent (or the lease expired),
// so no new mutation may be sent. It does not create any lock or wait.
func (e *Engine) parentLeaseHeld(lease *admissionLease) bool {
	if lease == nil || lease.parentID == "" {
		return true
	}
	if lease.owner == "" || e.deps.Store == nil {
		return false
	}
	ok, err := e.deps.Store.RenewActionExecution(lease.parentID, lease.owner, time.Now(), actionLeaseTTL)
	return err == nil && ok
}

func stepFailClosed(err error) StepResult {
	return StepResult{Status: StepFailed, Error: err.Error()}
}

// parentBlockedResult converts a durable parent policy into a step result. A
// paused or decision-waiting parent defers reversibly; a cancelled parent
// refuses the admission permanently. Neither contacts the worker.
func (e *Engine) parentBlockedResult(phase, policy string) StepResult {
	switch policy {
	case "paused":
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "parent_paused",
			WaitingReason:    fmt.Sprintf("Parent transcode batch is paused; deferring %s admission", phase),
		}
	case "waiting_decision":
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "parent_waiting_decision",
			WaitingReason:    fmt.Sprintf("Parent transcode batch is waiting for a user decision; deferring %s admission", phase),
		}
	default:
		return StepResult{
			Status:  StepFailed,
			Error:   fmt.Sprintf("%s admission refused: parent transcode batch was cancelled (no new submit)", phase),
			Outputs: map[string]any{"failure_classification": string(resilience.Cancelled), "parent_cancelled": true},
		}
	}
}
