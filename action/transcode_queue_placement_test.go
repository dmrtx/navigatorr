package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// clock is the engine's time in these tests: real time plus an offset.
type placementClock struct{ offset atomic.Int64 }

func (c *placementClock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *placementClock) advance(d time.Duration) {
	c.offset.Add(int64(d))
}

// placementEngine is m4 then m1 with a clock the test moves.
func placementEngine(t *testing.T, m4, m1 transcode.Executor) (*Engine, *placementClock) {
	t.Helper()
	e := retentionWorkers(t, m4, m1)
	clock := &placementClock{}
	e.deps.Now = clock.now
	return e, clock
}

// holdStep is the one step of the template the tests run: once a conversion has
// a worker it just waits on it, so it stays live and counted on that worker.
var holdTemplate = ActionTemplate{
	Name: "transcode_media",
	Steps: []StepDefinition{{Name: "hold", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "external_job", WaitingReason: "running on its worker"}, nil
	}}},
}

// turn gives one conversion its turn through the engine's own step path, which
// asks for a worker and saves whatever state the wait left. It returns the
// worker it was given ("" when it waits) and the wait it ended in.
func turn(t *testing.T, e *Engine, id string) (worker, condition, reason string) {
	t.Helper()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	ec := parseExecutionContext(inst, e)
	if _, err := e.execute(context.Background(), inst, ec, holdTemplate); err != nil {
		t.Fatal(err)
	}
	after, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	state = actionState(t, e.deps.Store, id)
	worker = assignedWorker(state)
	if worker == "" {
		return "", after.WaitingCondition, after.WaitingReason
	}
	return worker, "", ""
}

func waiter(t *testing.T, e *Engine, id string, age time.Duration, inputs, state map[string]any) {
	t.Helper()
	if state == nil {
		state = map[string]any{}
	}
	state[transcodeQueueOrderKey] = queueOrder(queueRankPlain, e.now().Add(-age), id, 0)
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusWaitingExternal, 0, "", inputs, state)
}

func sizedWaiter(t *testing.T, e *Engine, id string, age time.Duration, size int64, refusals map[string]time.Duration) {
	t.Helper()
	state := map[string]any{SubmittedSourceBytesKey: size}
	refuse(e, state, refusals)
	waiter(t, e, id, age, map[string]any{}, state)
}

func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func resolverFor(t *testing.T, dir string) *fsop.Resolver {
	t.Helper()
	r, err := fsop.NewResolver([]string{dir}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func busy(t *testing.T, e *Engine, id, node string) {
	t.Helper()
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": node})
}

// An old waiter whose size fits the idle node, refused by that node a moment
// ago, holds neither its slot nor the batch's. Past the window it is placed
// there again.
func TestAWaiterRefusedByAnodeDoesNotHoldThatNodesSlot(t *testing.T) {
	t.Run("refused 5 s ago", func(t *testing.T) {
		e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
		busy(t, e, "busy-m1", "m1")
		sizedWaiter(t, e, "old-a", 10*time.Minute, 30*gib, map[string]time.Duration{"m4": 5 * time.Second})
		seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
		if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "item-1", FilePath: "/x/small.mkv", Status: "queued"}); err != nil {
			t.Fatal(err)
		}
		if w, _, _ := turn(t, e, "old-a"); w != "" {
			t.Fatalf("the refused waiter was given %q", w)
		}
		if n := e.batchQueueAllowance(context.Background(), "batch-1"); n < 1 {
			t.Fatalf("the batch may start %d item(s); the refused waiter holds m4's slot against it", n)
		}
		sizedWaiter(t, e, "small-b", 5*time.Minute, 10*gib, nil)
		if w, _, _ := turn(t, e, "small-b"); w != "m4" {
			t.Fatalf("the small waiter behind was given %q, want m4", w)
		}
	})
	t.Run("past the window", func(t *testing.T) {
		e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
		busy(t, e, "busy-m1", "m1")
		sizedWaiter(t, e, "old-a", 10*time.Minute, 30*gib, map[string]time.Duration{"m4": 5 * time.Second})
		sizedWaiter(t, e, "small-b", 5*time.Minute, 10*gib, nil)
		if w, _, _ := turn(t, e, "old-a"); w != "" {
			t.Fatalf("the refused waiter was given %q", w)
		}
		clock.advance(roomRefusedFor + time.Second)
		if w, _, _ := turn(t, e, "small-b"); w != "" {
			t.Fatalf("the small waiter was given %q while the older one reaches m4 again", w)
		}
		if w, _, _ := turn(t, e, "old-a"); w != "m4" {
			t.Fatalf("the waiter was given %q after the window, want m4", w)
		}
	})
}

// Control: the same waiter, not refused, does hold the slot against the one
// behind it, so the test above proves the refusal is what frees it.
func TestAWaiterNotRefusedHoldsTheSlotAgainstTheOneBehind(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	busy(t, e, "busy-m1", "m1")
	sizedWaiter(t, e, "old-a", 10*time.Minute, 30*gib, nil)
	sizedWaiter(t, e, "small-b", 5*time.Minute, 10*gib, nil)
	if w, _, _ := turn(t, e, "small-b"); w != "" {
		t.Fatalf("the small waiter was given %q, want it to wait behind the older one", w)
	}
}

// Refusals are per node: refused by two nodes, it reaches neither.
func TestAWaiterRefusedByTwoNodesReachesNeither(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	sizedWaiter(t, e, "old-a", 10*time.Minute, 30*gib, map[string]time.Duration{"m1": 20 * time.Second, "m4": 5 * time.Second})
	sizedWaiter(t, e, "small-b", 5*time.Minute, 10*gib, nil)
	w, cond, reason := turn(t, e, "old-a")
	if w != "" || cond != "worker_busy" || !strings.Contains(reason, "refused it a moment ago") {
		t.Fatalf("old waiter: worker %q, %s %q", w, cond, reason)
	}
	if w, _, _ := turn(t, e, "small-b"); w == "" {
		t.Fatal("the small waiter behind waits although the refused one reaches no node")
	}
}

// The production case: one node busy, the other idle with room for small files
// only. The oldest conversion has no recorded size and a source the idle node
// cannot hold. After its first turn its size is saved by the engine, it does
// not hold the idle node's slot, and the small one and the batch get it.
func TestAnOldUnsizedConversionTheIdleNodeCannotHoldDoesNotHoldItsSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refused bool
	}{
		{"just refused by the idle node", true},
		{"never placed yet", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			big := filepath.Join(dir, "big.mkv")
			sparseFile(t, big, 70*gib)
			e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 40*gib), diskNode(1000*gib, 10*gib, 500*gib))
			e.deps.Fs = resolverFor(t, dir)
			busy(t, e, "busy-m1", "m1")
			state := map[string]any{}
			if tc.refused {
				state[requeuedFromKey] = "m4"
				refuse(e, state, map[string]time.Duration{"m4": 5 * time.Second})
			}
			waiter(t, e, "old-a", 10*time.Minute, map[string]any{"path": big}, state)
			seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
			if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "item-1", FilePath: filepath.Join(dir, "small.mkv"), Status: "queued"}); err != nil {
				t.Fatal(err)
			}

			if w, _, _ := turn(t, e, "old-a"); w == "m4" || w != "" {
				t.Fatalf("the old conversion was given %q; m4 cannot hold it and m1 is busy", w)
			}
			if got := getInt64(actionState(t, e.deps.Store, "old-a"), SubmittedSourceBytesKey); got != 70*gib {
				t.Fatalf("the size the engine saved is %d, want %d", got, 70*gib)
			}
			if n := e.batchQueueAllowance(context.Background(), "batch-1"); n < 1 {
				t.Fatalf("the batch may start %d item(s); the old conversion holds m4's slot against it", n)
			}
			sizedWaiter(t, e, "small-b", 5*time.Minute, 10*gib, nil)
			if w, _, _ := turn(t, e, "small-b"); w != "m4" {
				t.Fatalf("the small conversion behind it was given %q, want m4", w)
			}
		})
	}
}

// A size that cannot be read with a file resolver configured is not "needs
// nothing": the conversion waits, holds no slot, and the read is repeated at
// most once a minute.
func TestAnUnreadableSizeWaitsHoldsNoSlotAndIsReadOncePerMinute(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "gone.mkv")
	sparseFile(t, src, gib)
	var calls atomic.Int32
	oldStat := submittedStat
	submittedStat = func(string) (os.FileInfo, error) { calls.Add(1); return nil, os.ErrNotExist }
	defer func() { submittedStat = oldStat }()

	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	e.deps.Fs = resolverFor(t, dir)
	busy(t, e, "busy-m1", "m1")
	waiter(t, e, "old-a", 10*time.Minute, map[string]any{"path": src}, nil)
	sizedWaiter(t, e, "small-b", 5*time.Minute, 10*gib, nil)

	w, cond, reason := turn(t, e, "old-a")
	if w != "" || cond != "worker_busy" || !strings.Contains(reason, "source size could not be read") {
		t.Fatalf("worker %q, %s %q", w, cond, reason)
	}
	clock.advance(10 * time.Second)
	turn(t, e, "old-a")
	if n := calls.Load(); n != 1 {
		t.Fatalf("the size was read %d times in two turns within a minute, want 1", n)
	}
	if w, _, _ := turn(t, e, "small-b"); w != "m4" {
		t.Fatalf("the small waiter behind was given %q, want m4", w)
	}
	clock.advance(time.Minute)
	turn(t, e, "old-a")
	if n := calls.Load(); n != 2 {
		t.Fatalf("the size was read %d times after a minute, want 2", n)
	}
}

func singleWorkerEngine(t *testing.T, node transcode.Executor) (*Engine, *placementClock) {
	t.Helper()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "solo", Executor: node, MaxParallelJobs: 1, Podcast: true, PublishPolicy: transcode.PublishPolicyOnApproval}})
	if err != nil {
		t.Fatal(err)
	}
	clock := &placementClock{}
	return NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set, Now: clock.now}), clock
}

// With one worker the same placement applies: a refusal makes it wait out the
// window, and a node without room is waited for with the room reason.
func TestASingleWorkerInstallPlacesLikeAnyOther(t *testing.T) {
	t.Run("refused recently waits, then is assigned", func(t *testing.T) {
		e, clock := singleWorkerEngine(t, diskNode(1000*gib, 10*gib, 500*gib))
		sizedWaiter(t, e, "conv-a", time.Minute, 10*gib, map[string]time.Duration{"solo": time.Second})
		if w, cond, _ := turn(t, e, "conv-a"); w != "" || cond != "worker_busy" {
			t.Fatalf("worker %q, %s, want a wait", w, cond)
		}
		clock.advance(roomRefusedFor + time.Second)
		if w, _, _ := turn(t, e, "conv-a"); w != "solo" {
			t.Fatalf("worker %q after the window, want solo", w)
		}
	})
	t.Run("no room waits with the room reason", func(t *testing.T) {
		e, _ := singleWorkerEngine(t, diskNode(1000*gib, 10*gib, 5*gib))
		sizedWaiter(t, e, "conv-a", time.Minute, 50*gib, nil)
		w, cond, reason := turn(t, e, "conv-a")
		if w != "" || cond != "worker_busy" || !strings.Contains(reason, "has room") {
			t.Fatalf("worker %q, %s %q", w, cond, reason)
		}
	})
}

// A batch child without a size gets it at its turn, and the placement from its
// context is the one its waiter row gives.
func TestABatchChildWithoutSizeGetsItAndPlacesLikeItsWaiterRow(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mkv")
	sparseFile(t, src, 3*gib)
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	e.deps.Fs = resolverFor(t, dir)
	busy(t, e, "busy-m4", "m4")
	busy(t, e, "busy-m1", "m1")
	seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
	waiter(t, e, "child-1", time.Minute, map[string]any{"parent_action_id": "batch-1", "path": src}, nil)
	if w, _, _ := turn(t, e, "child-1"); w != "" {
		t.Fatalf("the child was given %q with both nodes full", w)
	}
	if got := getInt64(actionState(t, e.deps.Store, "child-1"), SubmittedSourceBytesKey); got != 3*gib {
		t.Fatalf("the child's size is %d, want %d", got, 3*gib)
	}
	rows, err := e.deps.Store.ListTranscodeQueueWaiters()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID != "child-1" {
			continue
		}
		fromRow := e.waiterPlacement(row)
		fromEC := e.placementOf(parseExecutionContextMust(t, e, "child-1"))
		if !reflect.DeepEqual(fromRow, fromEC) {
			t.Fatalf("waiter row placement %+v differs from the context's %+v", fromRow, fromEC)
		}
		if fromRow.sizeUnknown || fromRow.need.SourceBytes != 3*gib {
			t.Fatalf("placement %+v", fromRow)
		}
		return
	}
	t.Fatal("the child is not in the waiter list")
}

// With no worker ready the turn waits as unreachable, even for a conversion
// that needs nothing.
func TestNoReadyWorkerMakesTheTurnWaitUnreachable(t *testing.T) {
	m4, m1 := diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib)
	m4.doctorFunc = func(context.Context) error { return errors.New("offline") }
	m1.doctorFunc = func(context.Context) error { return errors.New("offline") }
	e, _ := placementEngine(t, m4, m1)
	sizedWaiter(t, e, "conv-a", time.Minute, 10*gib, nil)
	w, cond, reason := turn(t, e, "conv-a")
	if w != "" || cond != "worker_unreachable" || !strings.Contains(reason, "No video worker is ready") {
		t.Fatalf("worker %q, %s %q", w, cond, reason)
	}
}

// refuse stamps the refusals of nodes, each that long ago.
func refuse(e *Engine, state map[string]any, ago map[string]time.Duration) {
	if len(ago) == 0 {
		return
	}
	m := map[string]any{}
	for node, d := range ago {
		m[node] = e.now().Add(-d).Format(time.RFC3339Nano)
	}
	state[roomRefusalsKey] = m
}
