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
			d := newReconcileDispatcher()
			defer d.wg.Wait()
			for {
				if err := e.dispatchReconcile(ctx, d); err != nil && ctx.Err() == nil {
					log.Printf("action reconciler: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-e.reconcileWake:
				}
			}
		}()
	})
	return e.reconcilerDone
}

// reconcileConcurrency bounds continuations in flight at once. A continuation
// can hash a whole source or validate a candidate for minutes, so this leaves
// room for cheap worker polls beside several of those.
const reconcileConcurrency = 8

// reconcileDispatcher carries in-flight continuations across reconciler ticks.
// A slow continuation keeps only its own slot; the next tick still polls every
// other action instead of waiting for the slowest one of the previous tick.
//
// A continuation that has to wait for a media hash slot gives its dispatcher
// slot back first (see acquireHashSlot): whole-file reads are bounded by the
// engine's hash pool, so continuations queued behind them never keep live jobs
// from being polled.
type reconcileDispatcher struct {
	mu       sync.Mutex
	inflight map[string]struct{}
	sem      chan struct{}
	wg       sync.WaitGroup
}

// reconcileSlotKey carries the dispatcher slot of the running continuation.
type reconcileSlotKey struct{}

// reconcileSlot releases a dispatcher slot exactly once, whether the
// continuation yields it for a hash or finishes.
type reconcileSlot struct {
	once    sync.Once
	release func()
}

func (s *reconcileSlot) yield() { s.once.Do(s.release) }

// yieldReconcileSlot frees the caller's dispatcher slot, if it holds one.
func yieldReconcileSlot(ctx context.Context) {
	if s, ok := ctx.Value(reconcileSlotKey{}).(*reconcileSlot); ok {
		s.yield()
	}
}

func newReconcileDispatcher() *reconcileDispatcher {
	return &reconcileDispatcher{inflight: map[string]struct{}{}, sem: make(chan struct{}, reconcileConcurrency)}
}

// dispatchReconcile starts a continuation for each reconcilable action that is
// not already running from an earlier tick, without waiting for any of them to
// finish.
func (e *Engine) dispatchReconcile(ctx context.Context, d *reconcileDispatcher) error {
	ids, err := e.reconcilableIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		d.mu.Lock()
		_, busy := d.inflight[id]
		d.mu.Unlock()
		if busy {
			continue
		}
		// Wait for a slot rather than skipping: actions are listed in id order,
		// so skipping would starve the same ids every tick. Cheap polls free a
		// slot within milliseconds, and continuations waiting for a hash have
		// already given theirs back.
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		d.mu.Lock()
		d.inflight[id] = struct{}{}
		d.mu.Unlock()
		slot := &reconcileSlot{release: func() { <-d.sem }}
		d.wg.Add(1)
		go func(id string) {
			defer d.wg.Done()
			defer func() {
				d.mu.Lock()
				delete(d.inflight, id)
				d.mu.Unlock()
				slot.yield()
			}()
			if _, err := e.resume(context.WithValue(ctx, reconcileSlotKey{}, slot), id, "", nil, true); err != nil && !errors.Is(err, errActionBusy) && ctx.Err() == nil {
				log.Printf("action reconciler: %s: %v", id, err)
			}
		}(id)
	}
	return nil
}

// ReconcileOnce resumes opted-in external waits and interrupted executions and
// waits for every continuation it started. The service loop uses
// dispatchReconcile instead, so one slow continuation never delays the next
// poll of other actions.
func (e *Engine) ReconcileOnce(ctx context.Context) error {
	ids, err := e.reconcilableIDs(ctx)
	if err != nil {
		return err
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

// reconcilableIDs pages through candidate actions; pagination is stable while
// actions complete.
func (e *Engine) reconcilableIDs(ctx context.Context) ([]string, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	var ids []string
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		instances, err := e.deps.Store.ListReconcilableActions(after, 100)
		if err != nil {
			return nil, err
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
	return ids, nil
}

func (e *Engine) shouldReconcile(inst *store.ActionInstance, tmpl ActionTemplate, ec *ExecutionContext) bool {
	if !tmpl.AutoReconcile {
		return false
	}
	if inst.Status == StatusPending {
		return getBool(ec.State, "background_admission")
	}
	if legacyBudgetStoppedBatch(inst) {
		return !getBool(ec.State, "paused") && !getBool(ec.State, "cancel_requested")
	}
	if inst.Status != StatusWaitingExternal && inst.Status != StatusRunning {
		return false
	}
	if getBool(ec.State, "paused") {
		return false
	}
	// Parent cancel/pause control is enforced at each admission point under the
	// parent's durable lease. Pending children are still reconciled so they can
	// be driven to a terminal state safely; accepted jobs keep being tracked.
	// A slot waiter skips the rest of its backoff once a worker job ended.
	if at, err := time.Parse(time.RFC3339Nano, getString(ec.State, "next_poll_at")); err == nil && e.now().Before(at) && !(inst.WaitingCondition == "worker_busy" && e.slotWakeDue(ec.State)) {
		return false
	}
	// Recover running actions only after durable progress exists. In particular,
	// never turn an interrupted destructive preflight into implicit approval.
	if inst.Status == StatusRunning && len(ec.State) == 0 {
		return false
	}
	return true
}

// legacyBudgetStoppedBatch is a mode batch stopped only by the retired flat
// operation search cap. Its remaining files are admissible now, so the
// reconciler resumes scheduling without a decision; nothing is replaced and
// the stop option stays available while files run.
func legacyBudgetStoppedBatch(inst *store.ActionInstance) bool {
	return inst.ActionName == "transcode_batch" && inst.Status == StatusWaitingDecision && inst.WaitingCondition == "search_budget_exhausted"
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
	if condition == "worker_busy" && hasSlotTicket(ec.State) {
		// Waiting for a slot. When the worker refused with Retry-After, the
		// next poll lands on that hint (+1 s, applied below). Otherwise (a
		// queue-order wait, or no hint) poll every 30-60 s rather than every
		// few seconds; jitter keeps waiters from polling in lockstep. A job
		// ending wakes the waiter early either way (slotWakeDue).
		if at, err := time.Parse(time.RFC3339Nano, getString(ec.State, workerBusyNotBeforeKey)); err != nil || !at.After(e.now()) {
			if d := slotWaitPollDelay(); d > delay {
				delay = d
			}
		}
		ec.State[workerSlotPolledAtKey] = e.now().Format(time.RFC3339Nano)
	}
	next := e.now().Add(delay)
	for _, key := range []string{"retry_not_before", "benchmark_retry_not_before", workerBusyNotBeforeKey} {
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

// mediaHashConcurrency bounds whole-file media reads (hashes and promotion
// backup copies) running at once.
const mediaHashConcurrency = 3

// acquireHashSlot takes one of the engine's hash slots, honouring ctx. When
// it has to wait, a reconciler continuation first gives back its dispatcher
// slot, so queued hashes never keep live jobs from being polled. The returned
// release is safe to call more than once.
func (e *Engine) acquireHashSlot(ctx context.Context) (func(), error) {
	if e.hashSlots == nil {
		return func() {}, nil
	}
	var once sync.Once
	release := func() { once.Do(func() { <-e.hashSlots }) }
	select {
	case e.hashSlots <- struct{}{}:
		return release, nil
	default:
	}
	yieldReconcileSlot(ctx)
	select {
	case e.hashSlots <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// hashMedia copies a whole media file from r into h (a hash or a backup
// partial) while holding one of the engine's hash slots.
func (e *Engine) hashMedia(ctx context.Context, h io.Writer, r io.Reader) (int64, error) {
	release, err := e.acquireHashSlot(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	return copyReadAhead(ctx, h, r)
}

// A NAS read costs a round trip each, so io.Copy's 32 KiB reads leave the
// link idle while SHA-256 runs and the CPU idle while the link runs. Reading
// 1 MiB at a time, a few chunks ahead of the hash, overlaps the two. The
// bytes written, and so any digest, are exactly those of io.Copy.
const (
	hashReadChunk = 1 << 20
	hashReadAhead = 4
)

var hashChunks = sync.Pool{New: func() any { b := make([]byte, hashReadChunk); return &b }}

type hashChunk struct {
	buf *[]byte
	n   int
	err error
}

// copyReadAhead is io.Copy with the reads running ahead of the writes. It
// returns only after its reader goroutine has stopped, so the caller may
// close src, and it never writes bytes the source did not return.
func copyReadAhead(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	full := make(chan hashChunk, hashReadAhead)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(full)
		for {
			select {
			case <-stop:
				return
			default:
			}
			buf := hashChunks.Get().(*[]byte)
			var c hashChunk
			if err := ctx.Err(); err != nil {
				c = hashChunk{buf: buf, err: err}
			} else {
				n, err := src.Read(*buf)
				c = hashChunk{buf: buf, n: n, err: err}
			}
			select {
			case full <- c:
			case <-stop:
				hashChunks.Put(buf)
				return
			}
			if c.err != nil {
				return
			}
		}
	}()
	var written int64
	fail := func(err error) (int64, error) {
		close(stop)
		for c := range full {
			hashChunks.Put(c.buf)
		}
		<-done
		return written, err
	}
	for c := range full {
		if c.n > 0 {
			w, err := dst.Write((*c.buf)[:c.n])
			if w < 0 || w > c.n {
				w = 0
				if err == nil {
					err = errors.New("invalid write result")
				}
			}
			written += int64(w)
			if err == nil && w < c.n {
				err = io.ErrShortWrite
			}
			if err != nil {
				hashChunks.Put(c.buf)
				return fail(err)
			}
		}
		err := c.err
		hashChunks.Put(c.buf)
		if err == io.EOF {
			<-done
			return written, nil
		}
		if err != nil {
			return fail(err)
		}
	}
	<-done
	return written, nil
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

// WakeReconciler starts admitted browser work without waiting for the poll tick.
func (e *Engine) WakeReconciler() {
	select {
	case e.reconcileWake <- struct{}{}:
	default:
	}
}
