package action

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// nextPlan reads the plan after the engine has observed its workers once (the
// plan itself never probes a worker).
func nextPlan(t *testing.T, e *Engine) QueuePlan {
	t.Helper()
	warm(e)
	plan, err := e.QueueNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// warm is what dispatch does as a matter of course: it reads each worker's
// report and its slot pool, which the plan then reads without asking.
func warm(e *Engine) {
	e.WorkerObservation(context.Background())
	for _, w := range e.deps.Workers.All() {
		e.workerVideoCapacity(context.Background(), w)
	}
}

// ticketWaiter is a conversion that has a node and waits for a free slot
// there, the way a submit that found the node full does: it holds a slot
// ticket (worker_slot_wait_since), which is what makes a batch hold back the
// queued items behind it.
func ticketWaiter(t *testing.T, e *Engine, id, node string, age time.Duration) {
	t.Helper()
	ticketWaiterOf(t, e, id, node, "", age)
}

// ticketWaiterOf is ticketWaiter for a child of the batch parent.
func ticketWaiterOf(t *testing.T, e *Engine, id, node, parent string, age time.Duration) {
	t.Helper()
	since := e.now().Add(-age)
	inputs := map[string]any{}
	if parent != "" {
		inputs["parent_action_id"] = parent
	}
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusWaitingExternal, 0, "", inputs, map[string]any{
		"worker":               node,
		workerSlotWaitSinceKey: since.Format(time.RFC3339Nano),
		transcodeQueueOrderKey: queueOrder(queueRankPlain, since, id, 0),
	})
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	inst.WaitingCondition = "worker_busy"
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
}

func planNode(t *testing.T, plan QueuePlan, name string) PlanNode {
	t.Helper()
	for _, n := range plan.Nodes {
		if n.Worker == name {
			return n
		}
	}
	t.Fatalf("no node %s in %+v", name, plan.Nodes)
	return PlanNode{}
}

func skipOf(n PlanNode, id string) (PlanSkip, bool) {
	for _, s := range n.Skipped {
		if s.ID == id {
			return s, true
		}
	}
	return PlanSkip{}, false
}

func nextID(n PlanNode) string {
	if n.Next == nil {
		return ""
	}
	return n.Next.ID
}

func wantSkip(t *testing.T, n PlanNode, id, code string) PlanSkip {
	t.Helper()
	s, ok := skipOf(n, id)
	if !ok {
		t.Fatalf("node %s does not list %s as skipped: %+v", n.Worker, id, n.Skipped)
	}
	if s.Code != code {
		t.Fatalf("node %s skips %s for %q (%s), want %q", n.Worker, id, s.Code, s.Text, code)
	}
	if s.Text == "" || !strings.HasSuffix(s.Text, ".") {
		t.Fatalf("the reason of %s on %s is not a sentence: %q", id, n.Worker, s.Text)
	}
	return s
}

func openEngine(t *testing.T) (*Engine, *placementClock) {
	t.Helper()
	return placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
}

func TestQueueNextEmptyQueue(t *testing.T) {
	e, _ := openEngine(t)
	plan := nextPlan(t, e)
	if len(plan.Nodes) != 2 {
		t.Fatalf("nodes: %+v", plan.Nodes)
	}
	for _, n := range plan.Nodes {
		if n.State != NodeReady || n.FreeSlots != 1 || n.Next != nil || len(n.Skipped) != 0 || n.WhyNone == nil || n.WhyNone.Code != ReasonQueueEmpty {
			t.Fatalf("an empty queue on %s: %+v", n.Worker, n)
		}
	}
}

func TestQueueNextGivesEachNodeTheNextConversionAndExplainsTheRest(t *testing.T) {
	e, _ := openEngine(t)
	sizedWaiter(t, e, "a", 30*time.Minute, 10*gib, nil)
	sizedWaiter(t, e, "b", 20*time.Minute, 10*gib, nil)
	sizedWaiter(t, e, "c", 10*time.Minute, 10*gib, nil)
	plan := nextPlan(t, e)
	m4, m1 := planNode(t, plan, "m4"), planNode(t, plan, "m1")
	if nextID(m4) != "a" || nextID(m1) != "b" {
		t.Fatalf("next: m4 %q, m1 %q", nextID(m4), nextID(m1))
	}
	if m4.Next.Position != 1 || m1.Next.Position != 2 || m4.Next.Kind != QueueKindAction {
		t.Fatalf("picks: %+v %+v", m4.Next, m1.Next)
	}
	if len(m4.Skipped) != 0 || m4.WhyNone != nil {
		t.Fatalf("m4 has its next: %+v", m4)
	}
	// m1 skips what is ahead of its next: a starts on m4.
	if len(m1.Skipped) != 1 {
		t.Fatalf("m1 skipped: %+v", m1.Skipped)
	}
	wantSkip(t, m1, "a", ReasonStartsElsewhere)
}

func TestQueueNextBusyNodeSaysNoFreeSlot(t *testing.T) {
	e, _ := openEngine(t)
	busy(t, e, "run-m4", "m4")
	busy(t, e, "run-m1", "m1")
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	plan := nextPlan(t, e)
	for _, n := range plan.Nodes {
		if n.State != NodeBusy || n.FreeSlots != 0 || n.Next != nil {
			t.Fatalf("%s: %+v", n.Worker, n)
		}
		if n.WhyNone.Code != ReasonNoFreeSlot {
			t.Fatalf("why none: %+v", n.WhyNone)
		}
		wantSkip(t, n, "a", ReasonNoFreeSlot)
	}
}

func TestQueueNextAllNodesPaused(t *testing.T) {
	e, _ := openEngine(t)
	for _, node := range []string{"m4", "m1"} {
		if _, err := e.SetNodePause(context.Background(), node, store.WorkerPauseGraceful); err != nil {
			t.Fatal(err)
		}
	}
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	plan := nextPlan(t, e)
	for _, n := range plan.Nodes {
		if n.State != NodePaused || n.Next != nil || n.WhyNone.Code != ReasonNodePaused {
			t.Fatalf("%s: %+v", n.Worker, n)
		}
		s := wantSkip(t, n, "a", ReasonNodePaused)
		if !strings.Contains(s.Text, n.Worker) {
			t.Fatalf("the reason does not name the node: %q", s.Text)
		}
	}
	// One node resumed takes the work again.
	if _, err := e.SetNodePause(context.Background(), "m1", ""); err != nil {
		t.Fatal(err)
	}
	plan = nextPlan(t, e)
	if nextID(planNode(t, plan, "m1")) != "a" || planNode(t, plan, "m4").State != NodePaused {
		t.Fatalf("after resuming m1: %+v", plan.Nodes)
	}
}

func TestQueueNextClosedWindow(t *testing.T) {
	e, clock := openEngine(t)
	realtimeSamples(e)
	windowAt(t, e, clock, "m4", -time.Minute, 0)
	probed(t, e, "a", 10*time.Minute, "tv", 600)
	plan := nextPlan(t, e)
	m4, m1 := planNode(t, plan, "m4"), planNode(t, plan, "m1")
	if m4.State != NodeClosed || m4.Next != nil || m4.WhyNone.Code != windowWaitCondition {
		t.Fatalf("m4: %+v", m4)
	}
	s := wantSkip(t, m4, "a", windowWaitCondition)
	if !strings.Contains(s.Text, "window closed") {
		t.Fatalf("text: %q", s.Text)
	}
	if nextID(m1) != "a" || !m1.Next.HasEstimate || m1.Next.EstimateSec <= 0 {
		t.Fatalf("m1: %+v", m1.Next)
	}
}

func TestQueueNextSkipsAHeldFirstItem(t *testing.T) {
	e, _ := openEngine(t)
	sizedWaiter(t, e, "a", 30*time.Minute, 10*gib, nil)
	sizedWaiter(t, e, "b", 20*time.Minute, 10*gib, nil)
	sizedWaiter(t, e, "c", 10*time.Minute, 10*gib, nil)
	if _, err := e.HoldQueued(context.Background(), act("a"), nil); err != nil {
		t.Fatal(err)
	}
	plan := nextPlan(t, e)
	m4, m1 := planNode(t, plan, "m4"), planNode(t, plan, "m1")
	if nextID(m4) != "b" || nextID(m1) != "c" {
		t.Fatalf("next: m4 %q, m1 %q", nextID(m4), nextID(m1))
	}
	wantSkip(t, m4, "a", ReasonHeld)
	wantSkip(t, m1, "a", ReasonHeld)
	wantSkip(t, m1, "b", ReasonStartsElsewhere)
	if s, _ := skipOf(m4, "a"); s.Position != 1 {
		t.Fatalf("position: %+v", s)
	}
	// Everything held: nothing starts, and the node says so.
	for _, id := range []string{"b", "c"} {
		if _, err := e.HoldQueued(context.Background(), act(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	plan = nextPlan(t, e)
	for _, n := range plan.Nodes {
		if n.Next != nil || n.WhyNone.Code != ReasonAllHeld {
			t.Fatalf("%s: %+v", n.Worker, n)
		}
	}
}

func TestQueueNextBatchHeldAndBatchGating(t *testing.T) {
	// m4 has room for little, m1 for a lot.
	e, _ := placementEngine(t, diskNode(500*gib, 20*gib, 60*gib), diskNode(4000*gib, 20*gib, 900*gib))
	// A batch on hold: its waiting child keeps its place.
	seedActionInstance(t, e.deps.Store, "paused-batch", "transcode_batch", StatusWaitingDecision, 0, "", map[string]any{}, map[string]any{"paused": true})
	waiter(t, e, "child", 30*time.Minute, map[string]any{"parent_action_id": "paused-batch"}, map[string]any{SubmittedSourceBytesKey: 10 * gib})
	// A batch with a queued item behind a conversion of nobody's batch that
	// waits for a slot: the item starts nothing while that one waits.
	seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
	if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "i1", FilePath: "/x/Example Show S01E01.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	// "blocker" is older than the item and waits for a slot on m1.
	ticketWaiter(t, e, "blocker", "m1", 5*time.Hour)
	plan := nextPlan(t, e)
	m4 := planNode(t, plan, "m4")
	wantSkip(t, m4, "child", ReasonBatchHeld)
	var item PlanSkip
	for _, s := range m4.Skipped {
		if s.Kind == QueueKindItem {
			item = s
		}
	}
	if m4.Next != nil || item.Code != ReasonBatchGating {
		t.Fatalf("m4 takes %+v; the queued item: %+v (%+v)", m4.Next, item, m4.Skipped)
	}
	// With the blocker gone the item is no longer gated.
	finish(t, e, "blocker")
	plan = nextPlan(t, e)
	if n := planNode(t, plan, "m4"); n.Next == nil || n.Next.Kind != QueueKindItem || n.Next.Path == "" {
		t.Fatalf("after the blocker left: %+v", n)
	}
}

func TestQueueNextReasonCodes(t *testing.T) {
	ctx := context.Background()
	type scenario struct {
		name  string
		build func(t *testing.T) (*Engine, string, string) // engine, node, conversion id
		code  string
	}
	approvals := func(t *testing.T) (*Engine, string, string) {
		m4 := newRoomReporter(30*gib, 11*gib)
		m1 := newRoomReporter(30*gib, 11*gib)
		e, _ := placementEngine(t, m4, m1)
		sizedWaiter(t, e, "w", time.Minute, 29*gib, nil)
		return e, "m4", "w"
	}
	scenarios := []scenario{
		{"no disk room", func(t *testing.T) (*Engine, string, string) {
			e, _ := placementEngine(t, diskNode(500*gib, 20*gib, 60*gib), diskNode(500*gib, 20*gib, 60*gib))
			sizedWaiter(t, e, "w", time.Minute, 100*gib, nil)
			return e, "m4", "w"
		}, ReasonNoDiskRoom},
		{"waiting for approvals", approvals, ReasonApprovalRoom},
		{"too big for the node", func(t *testing.T) (*Engine, string, string) {
			e, _ := placementEngine(t, diskNode(400*gib, 20*gib, 300*gib), diskNode(600*gib, 20*gib, 500*gib))
			sizedWaiter(t, e, "w", time.Minute, 500*gib, nil)
			return e, "m4", "w"
		}, ReasonTooBig},
		{"build running", func(t *testing.T) (*Engine, string, string) {
			m4 := diskNode(1000*gib, 10*gib, 500*gib)
			m4.hold = "build"
			e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
			sizedWaiter(t, e, "w", time.Minute, 10*gib, nil)
			return e, "m4", "w"
		}, ReasonNodeBuild},
		{"paused by its report", func(t *testing.T) (*Engine, string, string) {
			m4 := diskNode(1000*gib, 10*gib, 500*gib)
			m4.hold = "paused"
			e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
			sizedWaiter(t, e, "w", time.Minute, 10*gib, nil)
			return e, "m4", "w"
		}, ReasonNodeOnHold},
		{"recently refused", func(t *testing.T) (*Engine, string, string) {
			e, _ := openEngine(t)
			sizedWaiter(t, e, "w", time.Minute, 10*gib, map[string]time.Duration{"m4": 5 * time.Second})
			return e, "m4", "w"
		}, ReasonRefused},
		{"node offline", func(t *testing.T) (*Engine, string, string) {
			m4 := diskNode(1000*gib, 10*gib, 500*gib)
			m4.doctorFunc = func(context.Context) error { return errors.New("down") }
			e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
			sizedWaiter(t, e, "w", time.Minute, 10*gib, nil)
			return e, "m4", "w"
		}, ReasonNodeOffline},
		{"size unknown", func(t *testing.T) (*Engine, string, string) {
			e, _ := openEngine(t)
			e.deps.Fs = resolverFor(t, t.TempDir())
			waiter(t, e, "w", time.Minute, map[string]any{}, nil)
			return e, "m4", "w"
		}, ReasonSizeUnknown},
		{"window: media type", func(t *testing.T) (*Engine, string, string) {
			e, _ := openEngine(t)
			typesOnly(t, e, "m4", "movie")
			probed(t, e, "w", time.Minute, "tv", 600)
			return e, "m4", "w"
		}, windowCodeMediaType},
		{"window: would not fit", func(t *testing.T) (*Engine, string, string) {
			e, clock := openEngine(t)
			realtimeSamples(e)
			windowAt(t, e, clock, "m4", 30*time.Minute, 0)
			probed(t, e, "w", time.Minute, "tv", 7200)
			return e, "m4", "w"
		}, windowCodeNoFit},
		{"window: no probe", func(t *testing.T) (*Engine, string, string) {
			e, clock := openEngine(t)
			windowAt(t, e, clock, "m4", time.Hour, 0)
			probed(t, e, "w", time.Minute, "tv", 0)
			return e, "m4", "w"
		}, windowCodeNoProbe},
		{"window: no history", func(t *testing.T) (*Engine, string, string) {
			e, clock := openEngine(t)
			windowAt(t, e, clock, "m4", time.Hour, 0)
			probed(t, e, "w", time.Minute, "tv", 600)
			return e, "m4", "w"
		}, windowCodeNoHistory},
		{"window: closed", func(t *testing.T) (*Engine, string, string) {
			e, clock := openEngine(t)
			windowAt(t, e, clock, "m4", -time.Minute, 0)
			probed(t, e, "w", time.Minute, "tv", 600)
			return e, "m4", "w"
		}, windowCodeClosed},
		{"window: unreadable", func(t *testing.T) (*Engine, string, string) {
			e, raw := faultEngine(t)
			waiter(t, e, "w", time.Minute, map[string]any{"media_type": "movie"}, nil)
			if _, err := raw.Exec(`DROP TABLE worker_windows`); err != nil {
				t.Fatal(err)
			}
			return e, "m4", "w"
		}, windowCodeUnreadable},
		{"slot taken by one ahead", func(t *testing.T) (*Engine, string, string) {
			// m4 is busy; "blocker" fits only m4 and waits. The queued item
			// behind it is gated, yet it keeps its claim on m1, so the younger
			// conversion that fits both cannot take m1's slot from it.
			e, _ := placementEngine(t, diskNode(4000*gib, 20*gib, 900*gib), diskNode(500*gib, 20*gib, 60*gib))
			seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
			if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "i1", FilePath: "/x/Example Show S01E03.mkv", Status: "queued"}); err != nil {
				t.Fatal(err)
			}
			ticketWaiter(t, e, "blocker", "m4", 5*time.Hour)
			sizedWaiter(t, e, "w", -time.Minute, 10*gib, nil)
			return e, "m1", "w"
		}, ReasonSlotTakenAhead},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			e, node, id := sc.build(t)
			plan := nextPlan(t, e)
			n := planNode(t, plan, node)
			s, ok := skipOf(n, id)
			if !ok {
				t.Fatalf("%s does not list %s: %+v (next %+v)", node, id, n.Skipped, n.Next)
			}
			if s.Code != sc.code {
				t.Fatalf("%s skips %s for %q (%s), want %q", node, id, s.Code, s.Text, sc.code)
			}
			if s.Text == "" || !strings.HasSuffix(s.Text, ".") {
				t.Fatalf("not a sentence: %q", s.Text)
			}
		})
	}
	_ = ctx
}

func TestQueueNextNothingEligible(t *testing.T) {
	e, _ := openEngine(t)
	typesOnly(t, e, "m4", "movie")
	typesOnly(t, e, "m1", "movie")
	probed(t, e, "a", time.Minute, "tv", 600)
	plan := nextPlan(t, e)
	for _, n := range plan.Nodes {
		if n.State != NodeReady || n.Next != nil || n.WhyNone.Code != ReasonNothingEligible {
			t.Fatalf("%s: %+v", n.Worker, n)
		}
	}
}

func TestQueueNextBoundsTheSkippedList(t *testing.T) {
	e, _ := openEngine(t)
	busy(t, e, "run-m4", "m4")
	busy(t, e, "run-m1", "m1")
	for i := 0; i < maxPlanSkipped+5; i++ {
		sizedWaiter(t, e, "w"+string(rune('a'+i)), time.Duration(100-i)*time.Minute, 1*gib, nil)
	}
	plan := nextPlan(t, e)
	for _, n := range plan.Nodes {
		if len(n.Skipped) != maxPlanSkipped || !n.SkippedTruncated {
			t.Fatalf("%s lists %d skipped, truncated %v", n.Worker, len(n.Skipped), n.SkippedTruncated)
		}
	}
	// A node with a next far down the queue lists what is ahead of it, bounded.
	finish(t, e, "run-m1")
	plan = nextPlan(t, e)
	if m1 := planNode(t, plan, "m1"); nextID(m1) != "wa" || len(m1.Skipped) != 0 || m1.SkippedTruncated {
		t.Fatalf("m1: %+v", m1)
	}
}

// The plan reads and writes nothing: the queue, its revision and the state of
// every conversion are as they were, and it asks no worker anything.
func TestQueueNextIsReadOnly(t *testing.T) {
	e, _ := openEngine(t)
	sizedWaiter(t, e, "a", 30*time.Minute, 10*gib, nil)
	sizedWaiter(t, e, "b", 20*time.Minute, 10*gib, nil)
	seedActionInstance(t, e.deps.Store, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
	if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "i1", FilePath: "/x/Example Show S01E02.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	e.WorkerObservation(context.Background())
	snapshot := func() string {
		v, err := e.TranscodeQueueView()
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, en := range v.Entries {
			b.WriteString(en.Kind + en.ID + en.WaitingCondition + en.WaitingReason + "|")
		}
		for _, id := range []string{"a", "b"} {
			inst, _ := e.deps.Store.GetActionInstance(id)
			b.WriteString(inst.Status + inst.StateJSON + inst.UpdatedAt + "|")
		}
		return b.String() + string(rune(v.Revision))
	}
	before := snapshot()
	probes := func() int {
		n := 0
		for _, name := range []string{"m4", "m1"} {
			w, _ := e.deps.Workers.Get(name)
			n += int(w.Executor.(*roomReporter).doctorCalls)
		}
		return n
	}
	calls := probes()
	for i := 0; i < 3; i++ {
		if _, err := e.QueueNext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if snapshot() != before {
		t.Fatal("QueueNext changed the queue or a conversion")
	}
	if probes() != calls {
		t.Fatalf("QueueNext probed a worker: %d calls before, %d after", calls, probes())
	}
	// Nothing was assigned.
	for _, id := range []string{"a", "b"} {
		if w := assignedWorker(actionState(t, e.deps.Store, id)); w != "" {
			t.Fatalf("%s was given %q by a read", id, w)
		}
	}
}

func TestQueueNextWithoutWorkers(t *testing.T) {
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}})
	plan, err := e.QueueNext(context.Background())
	if err != nil || len(plan.Nodes) != 0 || plan.Nodes == nil {
		t.Fatalf("%+v %v", plan, err)
	}
}

func mustWorkerSet(t *testing.T, workers ...transcode.NamedWorker) *transcode.WorkerSet {
	t.Helper()
	set, err := transcode.NewWorkerSet(workers)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// What the plan says starts where is what the dispatcher gives, for the same
// state: each scenario is planned, then every waiter takes its turn through
// the engine's own step path in queue order.
func TestQueueNextAgreesWithDispatch(t *testing.T) {
	scenarios := []struct {
		name  string
		build func(t *testing.T) (*Engine, []string)
		// unpolled are waiters nothing polls (stale ones): dispatch never gives
		// them a turn.
		unpolled []string
	}{
		{"a requeued conversion prefers the node it came from", func(t *testing.T) (*Engine, []string) {
			e, _ := twoSlotM4(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
			waiter(t, e, "w1", 50*time.Minute, map[string]any{}, map[string]any{SubmittedSourceBytesKey: 10 * gib, requeuedFromKey: "m1"})
			sizedWaiter(t, e, "w2", 40*time.Minute, 10*gib, nil)
			return e, []string{"w1", "w2"}
		}, nil},
		{"a stale waiter at the head, an active one behind, one slot", func(t *testing.T) (*Engine, []string) {
			e, raw := faultEngine(t)
			busy(t, e, "run-m1", "m1")
			sizedWaiter(t, e, "old", 50*time.Minute, 10*gib, nil)
			sizedWaiter(t, e, "active", 40*time.Minute, 10*gib, nil)
			backdate(t, raw, "old", 20*time.Minute)
			return e, []string{"old", "active"}
		}, []string{"old"}},
		{"room decides who fits where", func(t *testing.T) (*Engine, []string) {
			e, _ := twoSlotM4(t, poolWorkerDisk(500*gib, 20*gib, 60*gib), poolWorkerDisk(4000*gib, 20*gib, 900*gib))
			sizedWaiter(t, e, "w1", 50*time.Minute, 120*gib, nil)
			sizedWaiter(t, e, "w2", 40*time.Minute, 10*gib, nil)
			sizedWaiter(t, e, "w3", 30*time.Minute, 10*gib, nil)
			sizedWaiter(t, e, "w4", 20*time.Minute, 10*gib, nil)
			sizedWaiter(t, e, "w5", 10*time.Minute, 150*gib, nil)
			return e, []string{"w1", "w2", "w3", "w4", "w5"}
		}, nil},
		{"a busy node and a refusal", func(t *testing.T) (*Engine, []string) {
			e, _ := twoSlotM4(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
			busy(t, e, "run", "m4")
			sizedWaiter(t, e, "w1", 50*time.Minute, 10*gib, map[string]time.Duration{"m4": 3 * time.Second})
			sizedWaiter(t, e, "w2", 40*time.Minute, 10*gib, nil)
			sizedWaiter(t, e, "w3", 30*time.Minute, 10*gib, nil)
			return e, []string{"w1", "w2", "w3"}
		}, nil},
		{"windows and a hold", func(t *testing.T) (*Engine, []string) {
			e, clock := twoSlotM4(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
			realtimeSamples(e)
			typesOnly(t, e, "m4", "movie")
			windowAt(t, e, clock, "m1", 90*time.Minute, 0)
			probed(t, e, "w1", 50*time.Minute, "tv", 7200)
			probed(t, e, "w2", 40*time.Minute, "movie", 600)
			probed(t, e, "w3", 30*time.Minute, "tv", 600)
			probed(t, e, "w4", 20*time.Minute, "movie", 600)
			probed(t, e, "w5", 10*time.Minute, "movie", 600)
			if _, err := e.HoldQueued(context.Background(), act("w2"), nil); err != nil {
				t.Fatal(err)
			}
			return e, []string{"w1", "w2", "w3", "w4", "w5"}
		}, nil},
		{"a node that is paused", func(t *testing.T) (*Engine, []string) {
			e, _ := twoSlotM4(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
			if _, err := e.SetNodePause(context.Background(), "m4", store.WorkerPauseGraceful); err != nil {
				t.Fatal(err)
			}
			sizedWaiter(t, e, "w1", 50*time.Minute, 10*gib, nil)
			sizedWaiter(t, e, "w2", 40*time.Minute, 10*gib, nil)
			return e, []string{"w1", "w2"}
		}, nil},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			e, ids := sc.build(t)
			plan := nextPlan(t, e)
			planned := map[string]string{}
			for key, node := range plan.started {
				planned[strings.TrimPrefix(key, QueueKindAction+":")] = node
			}
			dispatched := map[string]string{}
			for _, id := range ids {
				if slices.Contains(sc.unpolled, id) {
					continue
				}
				if w, _, _ := turn(t, e, id); w != "" {
					dispatched[id] = w
				}
			}
			if len(planned) == 0 {
				t.Fatal("the scenario starts nothing: it proves nothing")
			}
			// The walk stops once every node has its next, so it names the first
			// conversion of each node and nothing after; the dispatcher must have
			// given exactly those their nodes, and given each node nothing
			// earlier in the queue than the plan's next.
			for id, node := range planned {
				if dispatched[id] != node {
					t.Fatalf("%s: the plan says %q, the dispatcher gave %q (plan %v, dispatched %v)", id, node, dispatched[id], planned, dispatched)
				}
			}
			for _, n := range plan.Nodes {
				var first string
				for _, id := range ids {
					if dispatched[id] == n.Worker {
						first = id
						break
					}
				}
				if nextID(n) != first {
					t.Fatalf("%s: the plan's next is %q, the dispatcher's first is %q (dispatched %v)", n.Worker, nextID(n), first, dispatched)
				}
			}
		})
	}
}

// poolWorkerDisk is a node that reports a work disk, as diskNode does, kept
// apart so the scenario above reads as what it is.
func poolWorkerDisk(disk, floor, room int64) *roomReporter { return diskNode(disk, floor, room) }

// backdate makes a waiter look as if nothing had polled it for age.
func backdate(t *testing.T, raw *sql.DB, id string, age time.Duration) {
	t.Helper()
	at := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if _, err := raw.Exec(`UPDATE action_instances SET updated_at = ? WHERE id = ?`, at, id); err != nil {
		t.Fatal(err)
	}
}

// A waiter nobody polls any more is not waiting for a slot: dispatch never
// gives it a turn and it holds no one back, so the plan never starts it, lists
// it as not polling, and gives the node to the one behind it.
func TestQueueNextStaleWaiterNeverStartsAndHoldsNoOneBack(t *testing.T) {
	e, raw := faultEngine(t)
	busy(t, e, "run-m1", "m1")
	sizedWaiter(t, e, "old", 50*time.Minute, 10*gib, nil)
	sizedWaiter(t, e, "active", 40*time.Minute, 10*gib, nil)
	backdate(t, raw, "old", 20*time.Minute)
	plan := nextPlan(t, e)
	m4 := planNode(t, plan, "m4")
	if nextID(m4) != "active" || m4.Next.Position != 2 {
		t.Fatalf("m4 next: %+v", m4.Next)
	}
	wantSkip(t, m4, "old", ReasonNotPolling)
	if plan.started["action:old"] != "" {
		t.Fatalf("the stale waiter was started: %v", plan.started)
	}
	// m1 is busy: the stale head is listed there too.
	wantSkip(t, planNode(t, plan, "m1"), "old", ReasonNotPolling)
}

// A node that was never observed says so, and the plan does not ask it.
func TestQueueNextColdReadDoesNotProbe(t *testing.T) {
	e, _ := openEngine(t)
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	calls := func() int {
		n := 0
		for _, name := range []string{"m4", "m1"} {
			w, _ := e.deps.Workers.Get(name)
			n += int(w.Executor.(*roomReporter).doctorCalls)
		}
		return n
	}
	before := calls()
	plan, err := e.QueueNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls() != before {
		t.Fatal("a cold read asked a worker")
	}
	for _, n := range plan.Nodes {
		if n.State != NodeUnknown || n.FreeSlots != 0 || n.Next != nil || n.WhyNone.Code != ReasonNodeUnknown {
			t.Fatalf("%s: %+v", n.Worker, n)
		}
		if s := wantSkip(t, n, "a", ReasonNodeUnknown); !strings.Contains(s.Text, "has sent no recent report") ||
			n.WhyNone == nil || !strings.Contains(n.WhyNone.Text, "has sent no recent report") {
			t.Fatalf("no report, text: %q / %q", s.Text, n.WhyNone.Text)
		}
	}
	// Once the workers have reported, the plan speaks for them.
	warm(e)
	if n := planNode(t, nextPlan(t, e), "m4"); n.State != NodeReady || nextID(n) != "a" {
		t.Fatalf("after a report: %+v", n)
	}
	// A report that is too old is not spoken for either.
	st := e.workerStateFor("m4")
	st.obsMu.Lock()
	st.obs.CheckedAt = e.now().Add(-10 * time.Minute)
	st.obsMu.Unlock()
	plan, _ = e.QueueNext(context.Background())
	if n := planNode(t, plan, "m4"); n.State != NodeUnknown {
		t.Fatalf("an old report: %+v", n)
	}
	if n := planNode(t, plan, "m1"); n.State != NodeReady {
		t.Fatalf("the other node: %+v", n)
	}
}

// A node that holds itself paused through its own report is paused to the
// plan too, and the reason does not point at the operator's pause.
func TestQueueNextNodeOnHoldByItsOwnReport(t *testing.T) {
	m4 := diskNode(1000*gib, 10*gib, 500*gib)
	m4.hold = "paused"
	e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	plan := nextPlan(t, e)
	n := planNode(t, plan, "m4")
	if n.State != NodePaused || n.Next != nil || n.WhyNone.Code != ReasonNodeOnHold {
		t.Fatalf("m4: %+v", n)
	}
	wantSkip(t, n, "a", ReasonNodeOnHold)
	if nextID(planNode(t, plan, "m1")) != "a" {
		t.Fatal("m1 should take it")
	}
}

// The reasons are the verdict and the need of the node they judge: m4 does not
// retain candidates, so it is judged on disk alone; m1 does, so it is judged on
// its approval budget first.
func TestQueueNextRoomReasonUsesTheNodesOwnNeed(t *testing.T) {
	m4 := diskNode(1000*gib, 20*gib, 60*gib)
	m1 := newRoomReporter(100*gib, 10*gib)
	m1.disk, m1.floor, m1.diskRoom = 4000*gib, 20*gib, 900*gib
	set := mustWorkerSet(t,
		transcode.NamedWorker{Name: "m4", Executor: m4, MaxParallelJobs: 1, PublishPolicy: ""},
		transcode.NamedWorker{Name: "m1", Executor: m1, MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval, Podcast: true})
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
	sizedWaiter(t, e, "w", time.Minute, 100*gib, nil)
	plan := nextPlan(t, e)
	s4 := wantSkip(t, planNode(t, plan, "m4"), "w", ReasonNoDiskRoom)
	if !strings.Contains(s4.Text, "64.4 GB") || !strings.Contains(s4.Text, "needs 161.1 GB") {
		t.Fatalf("m4: %q", s4.Text)
	}
	s1 := wantSkip(t, planNode(t, plan, "m1"), "w", ReasonApprovalRoom)
	if !strings.Contains(s1.Text, "10.7 GB") || !strings.Contains(s1.Text, "needs 53.7 GB") {
		t.Fatalf("m1: %q", s1.Text)
	}
}

// With 25 held conversations ahead, the node's next is beyond position 20: the
// skipped list stops at 20 and says so.
func TestQueueNextSkippedBoundWhenNextIsBeyondIt(t *testing.T) {
	e, _ := openEngine(t)
	var held []string
	for i := 0; i < 24; i++ {
		id := "h" + string(rune('a'+i))
		sizedWaiter(t, e, id, time.Duration(200-i)*time.Minute, 1*gib, nil)
		held = append(held, id)
	}
	sizedWaiter(t, e, "last", 10*time.Minute, 1*gib, nil)
	for _, id := range held {
		if _, err := e.HoldQueued(context.Background(), act(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	plan := nextPlan(t, e)
	m4 := planNode(t, plan, "m4")
	if nextID(m4) != "last" || m4.Next.Position != 25 {
		t.Fatalf("next: %+v", m4.Next)
	}
	if len(m4.Skipped) != maxPlanSkipped || !m4.SkippedTruncated {
		t.Fatalf("%d skipped, truncated %v", len(m4.Skipped), m4.SkippedTruncated)
	}
	if m4.Skipped[0].ID != "ha" || m4.Skipped[maxPlanSkipped-1].Position != maxPlanSkipped {
		t.Fatalf("the list is not the first %d in order: %+v", maxPlanSkipped, m4.Skipped)
	}
}

// Batch items agree with the batch's own dispatch: an item behind a conversion
// that waits for a slot on its node is not started, and one that has the line
// free is.
func TestQueueNextAgreesWithBatchDispatchForItems(t *testing.T) {
	seedBatch := func(t *testing.T, e *Engine, id string, paths ...string) {
		seedActionInstance(t, e.deps.Store, id, "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
		for i, p := range paths {
			if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: id, ItemKey: "i" + string(rune('1'+i)), FilePath: p, Status: "queued"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// startsInDispatch says whether the batch would start its queued items now.
	startsInDispatch := func(t *testing.T, e *Engine, batch string) map[int64]bool {
		items, err := e.deps.Store.ListTranscodeBatchItems(batch)
		if err != nil {
			t.Fatal(err)
		}
		pending, ok := e.batchDispatchCandidates(context.Background(), batch, items)
		if !ok {
			t.Fatal("the batch could not read its queue")
		}
		out := map[int64]bool{}
		for _, i := range pending {
			if items[i].Status == "queued" {
				out[items[i].ID] = true
			}
		}
		return out
	}
	itemStarted := func(plan QueuePlan, id int64) bool {
		_, ok := plan.started[QueueKindItem+":"+strconv.FormatInt(id, 10)]
		return ok
	}
	t.Run("free line", func(t *testing.T) {
		e, _ := openEngine(t)
		seedBatch(t, e, "batch-1", "/x/Example Show S01E01.mkv", "/x/Example Show S01E02.mkv")
		plan := nextPlan(t, e)
		got := startsInDispatch(t, e, "batch-1")
		items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
		for _, it := range items {
			if itemStarted(plan, it.ID) != got[it.ID] {
				t.Fatalf("item %d: plan %v, dispatch %v", it.ID, itemStarted(plan, it.ID), got[it.ID])
			}
		}
		if !itemStarted(plan, items[0].ID) {
			t.Fatal("the first item should start")
		}
	})
	t.Run("behind a foreign waiter for a slot", func(t *testing.T) {
		e, _ := openEngine(t)
		ticketWaiter(t, e, "other", "m1", 5*time.Hour)
		seedBatch(t, e, "batch-1", "/x/Example Show S01E01.mkv")
		plan := nextPlan(t, e)
		items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
		if got := startsInDispatch(t, e, "batch-1"); got[items[0].ID] || itemStarted(plan, items[0].ID) {
			t.Fatalf("the item starts: plan %v dispatch %v", itemStarted(plan, items[0].ID), got)
		}
		n := planNode(t, plan, "m4")
		var item PlanSkip
		for _, sk := range n.Skipped {
			if sk.Kind == QueueKindItem {
				item = sk
			}
		}
		if item.Code != ReasonBatchGating {
			t.Fatalf("m4: %+v", n)
		}
	})
	t.Run("behind a child of its own batch that waits for a slot", func(t *testing.T) {
		e, _ := openEngine(t)
		seedBatch(t, e, "batch-1", "/x/Example Show S01E01.mkv", "/x/Example Show S01E02.mkv")
		items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
		// The first item already has its child, waiting in the queue.
		waiter(t, e, "child-1", 30*time.Minute, map[string]any{"parent_action_id": "batch-1"}, map[string]any{SubmittedSourceBytesKey: 10 * gib})
		first := items[0]
		first.Status, first.ChildActionID = "waiting_for_slot", "child-1"
		if err := e.deps.Store.UpdateTranscodeBatchItem(first); err != nil {
			t.Fatal(err)
		}
		// Both nodes are full, so the child cannot take a slot and keeps the
		// batch's line closed.
		busy(t, e, "run-m4", "m4")
		busy(t, e, "run-m1", "m1")
		plan := nextPlan(t, e)
		got := startsInDispatch(t, e, "batch-1")
		if got[items[1].ID] || itemStarted(plan, items[1].ID) {
			t.Fatalf("the second item starts: plan %v dispatch %v", itemStarted(plan, items[1].ID), got)
		}
		if len(plan.started) != 0 {
			t.Fatalf("nothing should start: %v", plan.started)
		}
	})
}

func seedBatchItems(t *testing.T, e *Engine, id string, paths ...string) []store.TranscodeBatchItem {
	t.Helper()
	seedActionInstance(t, e.deps.Store, id, "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
	for i, p := range paths {
		if err := e.deps.Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: id, ItemKey: "i" + string(rune('1'+i)), FilePath: p, Status: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := e.deps.Store.ListTranscodeBatchItems(id)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// dispatchResult is what a batch's dispatch makes of its queued items: the
// items it would start, and whether it could read its queue at all.
func dispatchResult(t *testing.T, e *Engine, batch string) (map[int64]bool, bool) {
	t.Helper()
	items, err := e.deps.Store.ListTranscodeBatchItems(batch)
	if err != nil {
		t.Fatal(err)
	}
	pending, ok := e.batchDispatchCandidates(context.Background(), batch, items)
	out := map[int64]bool{}
	for _, i := range pending {
		if items[i].Status == "queued" {
			out[items[i].ID] = true
		}
	}
	return out, ok
}

func dispatchStartsItems(t *testing.T, e *Engine, batch string) map[int64]bool {
	t.Helper()
	items, err := e.deps.Store.ListTranscodeBatchItems(batch)
	if err != nil {
		t.Fatal(err)
	}
	pending, ok := e.batchDispatchCandidates(context.Background(), batch, items)
	if !ok {
		t.Fatal("the batch could not read its queue")
	}
	out := map[int64]bool{}
	for _, i := range pending {
		if items[i].Status == "queued" {
			out[items[i].ID] = true
		}
	}
	return out
}

func singleNodeEngine(t *testing.T) (*Engine, *placementClock) {
	t.Helper()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m4", Executor: diskNode(1000*gib, 10*gib, 500*gib), MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval, Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
	clock := &placementClock{}
	e.deps.Now = clock.now
	return e, clock
}

// A single node needs no report for dispatch to use it, and with nothing
// waiting nothing refreshes one, so the plan does not call it unknown: it
// shows the last news it has, whatever its age, and says when that was.
func TestQueueNextSingleNodeShowsItsLastNews(t *testing.T) {
	e, clock := singleNodeEngine(t)
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	w, _ := e.deps.Workers.Get("m4")
	calls := func() int32 { return atomic.LoadInt32(&w.Executor.(*roomReporter).doctorCalls) }
	before := calls()
	n := planNode(t, mustPlan(t, e), "m4")
	if n.State != NodeReady || n.FreeSlots != 1 || nextID(n) != "a" || !n.ReportAt.IsZero() {
		t.Fatalf("never reported: %+v", n)
	}
	if calls() != before {
		t.Fatal("the plan asked the worker")
	}
	warm(e)
	reported := planNode(t, mustPlan(t, e), "m4")
	if reported.State != NodeReady || reported.ReportAt.IsZero() {
		t.Fatalf("reported: %+v", reported)
	}
	clock.advance(5 * time.Minute)
	n = planNode(t, mustPlan(t, e), "m4")
	if n.State != NodeReady || nextID(n) != "a" || !n.ReportAt.Equal(reported.ReportAt) {
		t.Fatalf("an old report: %+v", n)
	}
}

// With one node the last hold it reported stays until a newer report says
// otherwise, however old that is.
func TestQueueNextSingleNodeKeepsAnOldHold(t *testing.T) {
	for hold, code := range map[string]string{"paused": ReasonNodeOnHold, "build": ReasonNodeBuild} {
		e, clock := singleNodeEngine(t)
		sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
		w, _ := e.deps.Workers.Get("m4")
		w.Executor.(*roomReporter).hold = hold
		warm(e)
		clock.advance(5 * time.Minute)
		n := planNode(t, mustPlan(t, e), "m4")
		if n.State != NodePaused || n.Next != nil || n.WhyNone.Code != code || n.ReportAt.IsZero() {
			t.Fatalf("%s: %+v", hold, n)
		}
		wantSkip(t, n, "a", code)
		// A newer report that says the hold is over lifts it.
		w.Executor.(*roomReporter).hold = ""
		warm(e)
		if n := planNode(t, mustPlan(t, e), "m4"); n.State != NodeReady || nextID(n) != "a" {
			t.Fatalf("%s lifted: %+v", hold, n)
		}
	}
}

// The last news of the only node was that it is down: it stays offline.
func TestQueueNextSingleNodeKeepsAnOldOffline(t *testing.T) {
	e, clock := singleNodeEngine(t)
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	w, _ := e.deps.Workers.Get("m4")
	w.Executor.(*roomReporter).doctorFunc = func(context.Context) error { return errors.New("down") }
	warm(e)
	clock.advance(5 * time.Minute)
	if n := planNode(t, mustPlan(t, e), "m4"); n.State != NodeOffline || n.WhyNone.Code != ReasonNodeOffline {
		t.Fatalf("offline: %+v", n)
	}
}

func mustPlan(t *testing.T, e *Engine) QueuePlan {
	t.Helper()
	plan, err := e.QueueNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// T3: what the node is wins over an empty queue.
func TestQueueNextNodeStateWinsOverAnEmptyQueue(t *testing.T) {
	e, _ := openEngine(t)
	for _, n := range mustPlan(t, e).Nodes {
		if n.WhyNone.Code != ReasonNodeUnknown {
			t.Fatalf("cold %s: %+v", n.Worker, n.WhyNone)
		}
	}
	warm(e)
	if _, err := e.SetNodePause(context.Background(), "m4", store.WorkerPauseGraceful); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, e)
	if n := planNode(t, plan, "m4"); n.State != NodePaused || n.WhyNone.Code != ReasonNodePaused {
		t.Fatalf("paused: %+v", n)
	}
	if n := planNode(t, plan, "m1"); n.WhyNone.Code != ReasonQueueEmpty {
		t.Fatalf("idle: %+v", n)
	}
}

// N1: the age of the pool counts only where the pool is read. A node on hold
// by its own report keeps that, however old its pool is.
func TestQueueNextOnHoldNodeIsNotUnknownForItsPool(t *testing.T) {
	m4 := diskNode(1000*gib, 10*gib, 500*gib)
	m4.hold = "paused"
	e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	// Only the report is read; the pool never is.
	e.WorkerObservation(context.Background())
	plan := mustPlan(t, e)
	if n := planNode(t, plan, "m4"); n.State != NodePaused || n.WhyNone.Code != ReasonNodeOnHold {
		t.Fatalf("m4: %+v", n)
	}
	// The other node's pool was never read: unknown, not offline or ready.
	if n := planNode(t, plan, "m1"); n.State != NodeUnknown {
		t.Fatalf("m1: %+v", n)
	}
}

// N2: an operator hold says held even when the waiter is also stale.
func TestQueueNextHeldStaleWaiterSaysHeld(t *testing.T) {
	e, raw := faultEngine(t)
	sizedWaiter(t, e, "old", 50*time.Minute, 10*gib, nil)
	backdate(t, raw, "old", 20*time.Minute)
	if _, err := e.HoldQueued(context.Background(), act("old"), nil); err != nil {
		t.Fatal(err)
	}
	wantSkip(t, planNode(t, nextPlan(t, e), "m4"), "old", ReasonHeld)
}

// N3: the waiters are read once per plan, so every batch is judged on one
// snapshot.
func TestQueueNextReadsTheSlotWaitersOnce(t *testing.T) {
	e, raw := faultEngine(t)
	ticketWaiter(t, e, "other", "m1", 5*time.Hour)
	seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv")
	seedBatchItems(t, e, "batch-2", "/x/Example Show S02E01.mkv")
	g := planGates{e: e, byBatch: map[string]*planGate{}}
	items1, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	items2, _ := e.deps.Store.ListTranscodeBatchItems("batch-2")
	k1, _ := e.batchItemKeys("batch-1", items1)
	k2, _ := e.batchItemKeys("batch-2", items2)
	if !g.gated("batch-1", k1[items1[0].ID], nil) {
		t.Fatal("batch-1 should be held behind the other waiter")
	}
	// The waiter goes away; the plan in progress keeps its snapshot.
	if _, err := raw.Exec(`DELETE FROM action_instances WHERE id = 'other'`); err != nil {
		t.Fatal(err)
	}
	if !g.gated("batch-2", k2[items2[0].ID], nil) {
		t.Fatal("batch-2 was judged on a newer read")
	}
}

// N4: a node that reports it is on hold is never planned a queued item.
func TestQueueNextOnHoldNodeTakesNoQueuedItem(t *testing.T) {
	m4 := diskNode(1000*gib, 10*gib, 500*gib)
	m4.hold = "paused"
	e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
	busy(t, e, "run-m1", "m1")
	seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv")
	plan := nextPlan(t, e)
	if len(plan.started) != 0 {
		t.Fatalf("the item was planned onto a node that is on hold: %v", plan.started)
	}
	if n := planNode(t, plan, "m4"); n.State != NodePaused || n.Next != nil {
		t.Fatalf("m4: %+v", n)
	}
}

// N7: a conversion that starts earlier in the walk ends its batch's wait, so
// the queued item behind it takes the second free slot.
func TestQueueNextStartedChildUnblocksTheItemBehindIt(t *testing.T) {
	e, _ := twoSlotM4(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	items := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv", "/x/Example Show S01E02.mkv")
	waiter(t, e, "child-1", 30*time.Minute, map[string]any{"parent_action_id": "batch-1"}, map[string]any{SubmittedSourceBytesKey: 10 * gib})
	first := items[0]
	first.Status, first.ChildActionID = "waiting_for_slot", "child-1"
	if err := e.deps.Store.UpdateTranscodeBatchItem(first); err != nil {
		t.Fatal(err)
	}
	plan := nextPlan(t, e)
	itemKey := QueueKindItem + ":" + strconv.FormatInt(items[1].ID, 10)
	if plan.started["action:child-1"] == "" || plan.started[itemKey] == "" {
		t.Fatalf("both should start: %v", plan.started)
	}
	// Dispatch: the child takes its turn, the batch sees the item running, and
	// the second item may then go.
	if w, _, _ := turn(t, e, "child-1"); w == "" {
		t.Fatal("the child did not start")
	}
	first.Status = "running"
	if err := e.deps.Store.UpdateTranscodeBatchItem(first); err != nil {
		t.Fatal(err)
	}
	if !dispatchStartsItems(t, e, "batch-1")[items[1].ID] {
		t.Fatal("dispatch does not start the second item")
	}
}

// T2: a stale child of the batch still keeps the items behind it waiting, in
// dispatch (which counts every waiting_for_slot item) and in the plan.
func TestQueueNextStaleChildGatesLikeDispatch(t *testing.T) {
	e, raw := faultEngine(t)
	items := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv", "/x/Example Show S01E02.mkv")
	waiter(t, e, "child-1", 30*time.Minute, map[string]any{"parent_action_id": "batch-1"}, map[string]any{SubmittedSourceBytesKey: 10 * gib})
	backdate(t, raw, "child-1", 20*time.Minute)
	first := items[0]
	first.Status, first.ChildActionID = "waiting_for_slot", "child-1"
	if err := e.deps.Store.UpdateTranscodeBatchItem(first); err != nil {
		t.Fatal(err)
	}
	plan := nextPlan(t, e)
	_, started := plan.started[QueueKindItem+":"+strconv.FormatInt(items[1].ID, 10)]
	if started != dispatchStartsItems(t, e, "batch-1")[items[1].ID] {
		t.Fatalf("plan %v, dispatch %v", started, dispatchStartsItems(t, e, "batch-1"))
	}
	if started {
		t.Fatal("both gate in dispatch: the item should wait")
	}
}

// twoSlotM4 is m4 with two video slots and m1 with one.
func twoSlotM4(t *testing.T, m4, m1 transcode.Executor) (*Engine, *placementClock) {
	t.Helper()
	set := mustWorkerSet(t,
		transcode.NamedWorker{Name: "m4", Executor: m4, MaxParallelJobs: 2, PublishPolicy: transcode.PublishPolicyOnApproval},
		transcode.NamedWorker{Name: "m1", Executor: m1, MaxParallelJobs: 1, PublishPolicy: transcode.PublishPolicyOnApproval, Podcast: true})
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
	clock := &placementClock{}
	e.deps.Now = clock.now
	e.windowSweepInline = true
	return e, clock
}

// failMark makes the queue mark of one conversion unreadable.
func failMark(t *testing.T, raw *sql.DB, id string) {
	t.Helper()
	if _, err := raw.Exec(`INSERT INTO transcode_queue_marks (kind, id, held, queue_order, updated_at) VALUES ('action', ?, 'x', '', '2026-01-01T00:00:00Z')`, id); err != nil {
		t.Fatal(err)
	}
}

// failKey makes the place of one conversion unreadable (its mark holds none and
// its state cannot be parsed).
func failKey(t *testing.T, raw *sql.DB, id string) {
	t.Helper()
	if _, err := raw.Exec(`UPDATE action_instances SET state_json = json_set(state_json, '$.junk', 1) || '' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE action_instances SET state_json = replace(state_json, '"junk":1', '"junk":1e999') WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

// A batch never reads the mark or the place of its own children for its gate:
// one it cannot read there changes nothing, while one it cannot read of another
// batch's waiter stops it, in dispatch and in the plan alike.
func TestQueueNextUnreadableWaiterFollowsDispatch(t *testing.T) {
	started := func(plan QueuePlan, id int64) bool {
		_, ok := plan.started[QueueKindItem+":"+strconv.FormatInt(id, 10)]
		return ok
	}
	breakers := map[string]func(*testing.T, *sql.DB, string){"mark": failMark, "key": failKey}
	for name, brk := range breakers {
		t.Run(name+" of its own child", func(t *testing.T) {
			e, raw := faultEngine(t)
			item := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv")[0]
			ticketWaiterOf(t, e, "own-child", "m1", "batch-1", 5*time.Hour)
			brk(t, raw, "own-child")
			got, ok := dispatchResult(t, e, "batch-1")
			if !ok || !got[item.ID] {
				t.Fatalf("dispatch: ok %v, starts %v", ok, got)
			}
			if !started(nextPlan(t, e), item.ID) {
				t.Fatal("the plan should go on too")
			}
		})
		t.Run(name+" of another batch's waiter", func(t *testing.T) {
			e, raw := faultEngine(t)
			item := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv")[0]
			ticketWaiterOf(t, e, "foreign", "m1", "batch-2", 5*time.Hour)
			brk(t, raw, "foreign")
			got, ok := dispatchResult(t, e, "batch-1")
			if ok || got[item.ID] {
				t.Fatalf("dispatch should schedule nothing: ok %v, starts %v", ok, got)
			}
			if started(nextPlan(t, e), item.ID) {
				t.Fatal("the plan should start nothing")
			}
		})
		t.Run(name+" two batches", func(t *testing.T) {
			e, raw := faultEngine(t)
			i1 := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv")[0]
			i2 := seedBatchItems(t, e, "batch-2", "/x/Example Show S02E01.mkv")[0]
			ticketWaiterOf(t, e, "broken", "m1", "batch-1", 5*time.Hour)
			brk(t, raw, "broken")
			g1, ok1 := dispatchResult(t, e, "batch-1")
			g2, ok2 := dispatchResult(t, e, "batch-2")
			if !ok1 || !g1[i1.ID] || ok2 || g2[i2.ID] {
				t.Fatalf("dispatch: batch-1 %v/%v, batch-2 %v/%v", ok1, g1, ok2, g2)
			}
			plan := nextPlan(t, e)
			if !started(plan, i1.ID) || started(plan, i2.ID) {
				t.Fatalf("plan: %v", plan.started)
			}
		})
	}
}

// A node that reports it is on hold: dispatch counts its free slot when it
// decides to make a child for a queued item, but the child cannot start there.
// The plan names the node the conversion would really start on, and nothing
// when there is none.
func TestQueueNextOnHoldNodeAndBatchDispatch(t *testing.T) {
	held := func() *roomReporter {
		m4 := diskNode(1000*gib, 10*gib, 500*gib)
		m4.hold = "paused"
		return m4
	}
	t.Run("the other node is free", func(t *testing.T) {
		e, _ := placementEngine(t, held(), diskNode(1000*gib, 10*gib, 500*gib))
		items := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv", "/x/Example Show S01E02.mkv")
		plan := nextPlan(t, e)
		key := func(i int) string { return QueueKindItem + ":" + strconv.FormatInt(items[i].ID, 10) }
		if plan.started[key(0)] != "m1" || plan.started[key(1)] != "" {
			t.Fatalf("started: %v", plan.started)
		}
		// Dispatch counts the held node's slot too, so it makes a child for
		// each item. The first child takes m1; the second waits (m4 is on hold
		// and m1 is full), which is the one conversion the plan says starts.
		got := dispatchStartsItems(t, e, "batch-1")
		if !got[items[0].ID] || !got[items[1].ID] {
			t.Fatalf("dispatch makes children for %v", got)
		}
		for i, id := range []string{"child-1", "child-2"} {
			waiter(t, e, id, time.Duration(30-i)*time.Minute, map[string]any{"parent_action_id": "batch-1"}, map[string]any{SubmittedSourceBytesKey: 10 * gib})
		}
		if w, _, _ := turn(t, e, "child-1"); w != "m1" {
			t.Fatalf("the first child went to %q", w)
		}
		if w, _, _ := turn(t, e, "child-2"); w != "" {
			t.Fatalf("the second child went to %q instead of waiting", w)
		}
	})
	t.Run("the other node is full", func(t *testing.T) {
		e, _ := placementEngine(t, held(), diskNode(1000*gib, 10*gib, 500*gib))
		busy(t, e, "run-m1", "m1")
		items := seedBatchItems(t, e, "batch-1", "/x/Example Show S01E01.mkv")
		plan := nextPlan(t, e)
		if len(plan.started) != 0 {
			t.Fatalf("started: %v", plan.started)
		}
		// Dispatch counts the held node's slot and makes a child that then
		// waits for a slot: no conversion starts, which is what the plan says.
		// (This difference is documented in docs/transcode/API_V1.md.)
		if !dispatchStartsItems(t, e, "batch-1")[items[0].ID] {
			t.Fatal("dispatch no longer counts the held node's slot for the child; update the plan and the docs")
		}
	})
}

// The state of a node wins over an empty queue, whatever the state is.
func TestQueueNextEmptyQueueStatesPerNode(t *testing.T) {
	// own hold, paused and build
	for _, hold := range []string{"paused", "build"} {
		m4 := diskNode(1000*gib, 10*gib, 500*gib)
		m4.hold = hold
		e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
		n := planNode(t, nextPlan(t, e), "m4")
		want := ReasonNodeOnHold
		if hold == "build" {
			want = ReasonNodeBuild
		}
		if n.State != NodePaused || n.FreeSlots != 0 || n.WhyNone.Code != want {
			t.Fatalf("hold %s: %+v", hold, n)
		}
	}
	// busy
	e, _ := openEngine(t)
	busy(t, e, "run-m4", "m4")
	if n := planNode(t, nextPlan(t, e), "m4"); n.State != NodeBusy || n.WhyNone.Code != ReasonNoFreeSlot {
		t.Fatalf("busy: %+v", n)
	}
	// closed window
	e, clock := openEngine(t)
	windowAt(t, e, clock, "m4", -time.Minute, 0)
	if n := planNode(t, nextPlan(t, e), "m4"); n.State != NodeClosed || n.WhyNone.Code != windowWaitCondition {
		t.Fatalf("closed: %+v", n)
	}
}

// The age of the pool counts only where the pool is read.
func TestQueueNextPoolAgeOnlyWhereItIsRead(t *testing.T) {
	e, clock := openEngine(t)
	warm(e)
	// An expired (not absent) pool on a ready node: unknown.
	st := e.workerStateFor("m4")
	st.poolMu.Lock()
	st.pool.at = e.now().Add(-10 * time.Minute)
	st.poolMu.Unlock()
	if n := planNode(t, mustPlan(t, e), "m4"); n.State != NodeUnknown || n.WhyNone == nil ||
		!strings.Contains(n.WhyNone.Text, "free slots were not read lately") {
		t.Fatalf("expired pool: %+v", n)
	}
	// A conversion waiting says the same of that node in its skipped list.
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	if s := wantSkip(t, planNode(t, mustPlan(t, e), "m4"), "a", ReasonNodeUnknown); !strings.Contains(s.Text, "free slots were not read lately") {
		t.Fatalf("expired pool, skipped text: %q", s.Text)
	}
	// A node the operator paused, or whose window closed, is not read from its
	// pool: it keeps its state, pool or no pool.
	if _, err := e.SetNodePause(context.Background(), "m4", store.WorkerPauseGraceful); err != nil {
		t.Fatal(err)
	}
	if n := planNode(t, mustPlan(t, e), "m4"); n.State != NodePaused {
		t.Fatalf("paused: %+v", n)
	}
	if _, err := e.SetNodePause(context.Background(), "m4", ""); err != nil {
		t.Fatal(err)
	}
	windowAt(t, e, clock, "m4", -time.Minute, 0)
	if n := planNode(t, mustPlan(t, e), "m4"); n.State != NodeClosed {
		t.Fatalf("closed: %+v", n)
	}
}

// A node that reports a build shows why, and the operator's pause comes first.
func TestQueueNextBuildHoldAndPausePrecedence(t *testing.T) {
	m4 := diskNode(1000*gib, 10*gib, 500*gib)
	m4.hold = "build"
	e, _ := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
	sizedWaiter(t, e, "a", 10*time.Minute, 10*gib, nil)
	plan := nextPlan(t, e)
	n := planNode(t, plan, "m4")
	if n.State != NodePaused || n.WhyNone.Code != ReasonNodeBuild {
		t.Fatalf("m4: %+v", n)
	}
	s := wantSkip(t, n, "a", ReasonNodeBuild)
	if !strings.Contains(s.Text, "m4") {
		t.Fatalf("text: %q", s.Text)
	}
	if _, err := e.SetNodePause(context.Background(), "m4", store.WorkerPauseGraceful); err != nil {
		t.Fatal(err)
	}
	n = planNode(t, nextPlan(t, e), "m4")
	if n.State != NodePaused || n.WhyNone.Code != ReasonNodePaused {
		t.Fatalf("paused by the operator: %+v", n)
	}
	wantSkip(t, n, "a", ReasonNodePaused)
}
