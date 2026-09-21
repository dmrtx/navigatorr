package action

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

const actionLeaseTTL = 2 * time.Minute

var errActionBusy = errors.New("action is already executing")

// actionLeaseOwnerKey identifies the lease owner token for a specific action.
// Keying by action id lets a child execution inherit and reuse an already-held
// parent lease (see beginAdmission) instead of re-acquiring or releasing it.
type actionLeaseOwnerKey struct{ actionID string }

func (e *Engine) now() time.Time {
	if e.deps.Now != nil {
		return e.deps.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) pollInterval() time.Duration {
	if e.deps.ReconcileInterval > 0 {
		return e.deps.ReconcileInterval
	}
	return 5 * time.Second
}

// StartReconciler starts one service-owned loop, independently of MCP requests.
// Cancel ctx and wait for the returned channel before closing the store.
func (e *Engine) StartReconciler(ctx context.Context) <-chan struct{} {
	e.reconcilerOnce.Do(func() {
		go func() {
			defer close(e.reconcilerDone)
			ticker := time.NewTicker(e.pollInterval())
			defer ticker.Stop()
			for {
				if err := e.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
					log.Printf("action reconciler: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
	return e.reconcilerDone
}

// ReconcileOnce resumes opted-in external waits and interrupted executions.
// Pagination is stable while actions complete. A slow NAS validation cannot
// block polling every other job: continuations use a bounded worker pool.
func (e *Engine) ReconcileOnce(ctx context.Context) error {
	if e.deps.Store == nil {
		return nil
	}
	var ids []string
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		instances, err := e.deps.Store.ListReconcilableActions(after, 100)
		if err != nil {
			return err
		}
		for i := range instances {
			inst := &instances[i]
			tmpl, ok := e.GetTemplate(inst.ActionName)
			if ok && e.shouldReconcile(inst, tmpl, parseExecutionContext(inst, e)) {
				ids = append(ids, inst.ID)
			}
			after = inst.ID
		}
		if len(instances) < 100 {
			break
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	sem := make(chan struct{}, 4)
	for _, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := e.resume(ctx, id, "", nil, true)
			if err != nil && !errors.Is(err, errActionBusy) && ctx.Err() == nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (e *Engine) shouldReconcile(inst *store.ActionInstance, tmpl ActionTemplate, ec *ExecutionContext) bool {
	if !tmpl.AutoReconcile || (inst.Status != StatusWaitingExternal && inst.Status != StatusRunning) {
		return false
	}
	if getBool(ec.State, "paused") {
		return false
	}
	// Parent cancel/pause control is enforced at each admission point under the
	// parent's durable lease. Pending children are still reconciled so they can
	// be driven to a terminal state safely; accepted jobs keep being tracked.
	if at, err := time.Parse(time.RFC3339Nano, getString(ec.State, "next_poll_at")); err == nil && e.now().Before(at) {
		return false
	}
	// Recover running actions only after durable progress exists. In particular,
	// never turn an interrupted destructive preflight into implicit approval.
	if inst.Status == StatusRunning && len(ec.State) == 0 {
		return false
	}
	return true
}

func (e *Engine) scheduleReconcile(ec *ExecutionContext, condition string) {
	tmpl, ok := e.GetTemplate(ec.ActionName)
	if !ok || !tmpl.AutoReconcile {
		return
	}
	delay := e.pollInterval()
	if condition == "worker_unreachable" || condition == "worker_reconciling" {
		failures := getInt(ec.State, "worker_poll_failures") + 1
		if failures > 5 {
			failures = 5
		}
		ec.State["worker_poll_failures"] = failures
		delay *= time.Duration(1 << (failures - 1))
		if delay > time.Minute {
			delay = time.Minute
		}
	} else {
		delete(ec.State, "worker_poll_failures")
	}
	next := e.now().Add(delay)
	for _, key := range []string{"retry_not_before", "benchmark_retry_not_before"} {
		if at, err := time.Parse(time.RFC3339Nano, getString(ec.State, key)); err == nil && at.After(next) {
			next = at
		}
	}
	ec.State["next_poll_at"] = next.Format(time.RFC3339Nano)
	ec.Outputs["next_poll_at"] = ec.State["next_poll_at"]
}

func (e *Engine) claimExecution(ctx context.Context, id string, wait bool) (context.Context, func(), error) {
	owner := generateActionID("executor")
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Leases use real elapsed time, independent of a scheduler's test clock.
		ok, err := e.deps.Store.ClaimActionExecution(id, owner, time.Now(), actionLeaseTTL)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			break
		}
		if !wait {
			return nil, nil, errActionBusy
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	leaseCtx, cancel := context.WithCancel(context.WithValue(ctx, actionLeaseOwnerKey{actionID: id}, owner))
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(actionLeaseTTL / 4)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				ok, err := e.deps.Store.RenewActionExecution(id, owner, time.Now(), actionLeaseTTL)
				if err != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	return leaseCtx, func() {
		cancel()
		<-done
		_ = e.deps.Store.ReleaseActionExecution(id, owner)
	}, nil
}

func (e *Engine) updateInstance(ctx context.Context, inst *store.ActionInstance) error {
	owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: inst.ID}).(string)
	inst.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return e.deps.Store.UpdateClaimedActionInstance(*inst, owner)
}

// persistExecutionState must complete before a request that could be accepted
// remotely. After a crash the same job identity is reconciled before any submit.
func (e *Engine) persistExecutionState(ctx context.Context, ec *ExecutionContext) error {
	if e.deps.Store == nil {
		return nil
	}
	inst, err := e.deps.Store.GetActionInstance(ec.InstanceID)
	if err != nil {
		return err
	}
	inst.InputsJSON = toJSON(ec.Inputs)
	inst.StateJSON = toJSON(ec.State)
	inst.OutputsJSON = toJSON(ec.Outputs)
	return e.updateInstance(ctx, inst)
}

func optionalDuration(state map[string]any, key string) *int64 {
	if _, ok := state[key]; !ok {
		return nil
	}
	v := getInt64(state, key)
	return &v
}

// contextReader keeps long NAS hashes interruptible on service shutdown or
// loss of the execution lease; cancellation never means cancelling the worker.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
