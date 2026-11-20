package action

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// realtimeSamples gives both nodes a history of 1080p x265 encodes that ran at
// real time (a file of N seconds took N seconds).
func realtimeSamples(e *Engine) {
	var s []EncodeSample
	for _, node := range []string{"m4", "m1"} {
		for i := 0; i < 3; i++ {
			s = append(s, EncodeSample{Worker: node, Class: "1080p", Family: EncodeFamilyX265, DurationSec: 3600, EncodeSec: 3600})
		}
	}
	e.loadEncodeSamples = func() ([]EncodeSample, error) { return s, nil }
}

// windowAt sets a node's window closing d from the engine's clock.
func windowAt(t *testing.T, e *Engine, clock *placementClock, node string, d time.Duration, margin time.Duration, types ...string) {
	t.Helper()
	until := clock.now().Add(d).UTC().Format(time.RFC3339)
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: node, Until: until, MarginSec: int(margin / time.Second), MediaTypes: types}, nil); err != nil {
		t.Fatal(err)
	}
}

func typesOnly(t *testing.T, e *Engine, node string, types ...string) {
	t.Helper()
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: node, MediaTypes: types}, nil); err != nil {
		t.Fatal(err)
	}
}

// probed is a conversion of the given media type whose source the early check
// has probed.
func probed(t *testing.T, e *Engine, id string, age time.Duration, mediaType string, durSec float64) {
	t.Helper()
	inputs := map[string]any{}
	if mediaType != "" {
		inputs["media_type"] = mediaType
	}
	state := map[string]any{}
	if durSec > 0 {
		state[queueProbeKey] = map[string]any{"duration_sec": durSec, "width": 1920, "height": 1080}
	}
	waiter(t, e, id, age, inputs, state)
}

func TestWindowMediaTypeFilter(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	typesOnly(t, e, "m1", "movie")

	probed(t, e, "ep", 10*time.Minute, "tv", 0)
	w, cond, reason := turn(t, e, "ep")
	if w != "" || cond != windowWaitCondition {
		t.Fatalf("a tv file on movie-only nodes: worker %q, wait %q", w, cond)
	}
	for _, want := range []string{"node m4", "node m1", "type tv is not allowed", "only movie"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	// A conversion with no media type is allowed only where no type is listed.
	probed(t, e, "unknown", 9*time.Minute, "", 0)
	if w, cond, _ := turn(t, e, "unknown"); w != "" || cond != windowWaitCondition {
		t.Fatalf("no media type: worker %q, wait %q", w, cond)
	}
	// The movie goes ahead, though the tv file is older.
	probed(t, e, "film", 5*time.Minute, "movie", 0)
	if w, _, _ := turn(t, e, "film"); w != "m4" {
		t.Fatalf("a movie on movie-only nodes: %q", w)
	}
	// Changing the window lets the tv file in.
	typesOnly(t, e, "m1", "tv", "movie")
	if w, _, _ := turn(t, e, "ep"); w != "m1" {
		t.Fatalf("after m1 took tv: %q", w)
	}
}

func TestWindowFitNoFitAndNoEstimate(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	realtimeSamples(e)
	windowAt(t, e, clock, "m4", 2*time.Hour, 0)
	windowAt(t, e, clock, "m1", 2*time.Hour, 0)

	probed(t, e, "fits", 10*time.Minute, "movie", 7000)
	if w, _, _ := turn(t, e, "fits"); w != "m4" {
		t.Fatalf("a 7000 s encode in a 2 h window: %q", w)
	}

	probed(t, e, "long", 9*time.Minute, "movie", 7400)
	w, cond, reason := turn(t, e, "long")
	if w != "" || cond != windowWaitCondition || !strings.Contains(reason, "does not fit") || !strings.Contains(reason, "123 min") {
		t.Fatalf("a 7400 s encode: worker %q, %q, %q", w, cond, reason)
	}

	probed(t, e, "unprobed", 8*time.Minute, "movie", 0)
	if w, cond, reason := turn(t, e, "unprobed"); w != "" || cond != windowWaitCondition || !strings.Contains(reason, "not been probed") {
		t.Fatalf("no probe: %q %q %q", w, cond, reason)
	}

	// No history on the node is no estimate either.
	e.loadEncodeSamples = func() ([]EncodeSample, error) { return nil, nil }
	e.history = encodeHistory{}
	probed(t, e, "nohistory", 7*time.Minute, "movie", 100)
	if w, cond, reason := turn(t, e, "nohistory"); w != "" || cond != windowWaitCondition || !strings.Contains(reason, "no encode history") {
		t.Fatalf("no history: %q %q %q", w, cond, reason)
	}

	// A window without an end needs no estimate; the other node can take what
	// this one refuses.
	if _, err := e.DeleteNodeWindow("m1", nil); err != nil {
		t.Fatal(err)
	}
	// The older ones that only windows held back are ahead of it now.
	if w, cond, reason := turn(t, e, "long"); w != "m1" {
		t.Fatalf("it should go to the node whose window has no end: %q %q %q", w, cond, reason)
	}
}

func TestWindowMarginMovesTheClosingTime(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	realtimeSamples(e)
	// 3 h away, 1 h margin: it closes in 2 h, so a 7400 s encode does not fit.
	windowAt(t, e, clock, "m4", 3*time.Hour, time.Hour)
	windowAt(t, e, clock, "m1", 3*time.Hour, time.Hour)
	probed(t, e, "a", 5*time.Minute, "tv", 7400)
	if w, cond, _ := turn(t, e, "a"); w != "" || cond != windowWaitCondition {
		t.Fatalf("margin ignored: %q %q", w, cond)
	}
	probed(t, e, "b", 4*time.Minute, "tv", 7000)
	if w, _, _ := turn(t, e, "b"); w != "m4" {
		t.Fatalf("fits before the margin: %q", w)
	}
}

func TestClosedWindowTakesNothingNewAndLeavesRunningJobsAlone(t *testing.T) {
	m4 := diskNode(1000*gib, 10*gib, 500*gib)
	e, clock := placementEngine(t, m4, diskNode(1000*gib, 10*gib, 500*gib))
	realtimeSamples(e)
	busy(t, e, "running", "m4")
	windowAt(t, e, clock, "m4", time.Hour, 0)
	node, _ := e.deps.Workers.Get("m4")
	if c := e.workerVideoCapacity(context.Background(), node); c != 1 {
		t.Fatalf("an open window keeps the capacity: %d", c)
	}
	clock.advance(2 * time.Hour)
	if c := e.workerVideoCapacity(context.Background(), node); c != 0 {
		t.Fatalf("a closed window has capacity %d, want 0", c)
	}
	if inst := mustInstance(t, e.deps.Store, "running"); inst.Status != StatusRunning || assignedWorker(parseExecutionContextMust(t, e, "running").State) != "m4" {
		t.Fatalf("a closed window touched the running job: %s", inst.Status)
	}
	// m1 is open and takes the new one; with m1 closed as well nothing starts.
	probed(t, e, "new", 10*time.Minute, "tv", 600)
	if w, _, _ := turn(t, e, "new"); w != "m1" {
		t.Fatalf("m4 is closed, m1 open: %q", w)
	}
	windowAt(t, e, clock, "m1", -time.Minute, 0)
	probed(t, e, "later", 5*time.Minute, "tv", 600)
	w, cond, reason := turn(t, e, "later")
	if w != "" || cond != windowWaitCondition || !strings.Contains(reason, "window closed since") {
		t.Fatalf("both closed: %q %q %q", w, cond, reason)
	}
	// It stays closed as time goes on, until the window is changed or deleted.
	clock.advance(48 * time.Hour)
	if w, _, _ := turn(t, e, "later"); w != "" {
		t.Fatalf("a closed window reopened by itself: %q", w)
	}
	if _, err := e.DeleteNodeWindow("m4", nil); err != nil {
		t.Fatal(err)
	}
	// m4 still has the running job in its one slot: free it first.
	inst := mustInstance(t, e.deps.Store, "running")
	inst.Status = StatusCompleted
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	if w, cond, reason := turn(t, e, "later"); w != "m4" {
		t.Fatalf("after the window was deleted: %q %q %q", w, cond, reason)
	}
}

func TestAWaiterOnlyAWindowHoldsBackHoldsNobodyBack(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	typesOnly(t, e, "m1", "movie")
	probed(t, e, "old-tv", 20*time.Minute, "tv", 0)
	if w, cond, _ := turn(t, e, "old-tv"); w != "" || cond != windowWaitCondition {
		t.Fatalf("%q %q", w, cond)
	}
	// It takes no ticket and no queue position: it is no contender.
	st := actionState(t, e.deps.Store, "old-tv")
	if hasSlotTicket(st) || st[workerSlotPositionKey] != nil {
		t.Fatalf("a window-held conversion kept a ticket or a position: %v", st)
	}
	probed(t, e, "film-1", 10*time.Minute, "movie", 0)
	probed(t, e, "film-2", 5*time.Minute, "movie", 0)
	if w, _, _ := turn(t, e, "film-1"); w != "m4" {
		t.Fatalf("film-1: %q", w)
	}
	// film-2 waits for a slot (both are taken by... m1 is free), so it goes to m1.
	if w, _, _ := turn(t, e, "film-2"); w != "m1" {
		t.Fatalf("film-2: %q", w)
	}
	// A third waits on a real slot shortage, not on the window.
	probed(t, e, "film-3", 3*time.Minute, "movie", 0)
	if _, cond, _ := turn(t, e, "film-3"); cond != "worker_busy" {
		t.Fatalf("a full farm is worker_busy, got %q", cond)
	}
	// And the window-held one is not counted as ahead of film-3.
	if n := mustInstance(t, e.deps.Store, "film-3").WaitingReason; strings.Contains(n, "old-tv") {
		t.Fatalf("film-3 waits behind the window-held file: %q", n)
	}
}

func TestMixedWindowAndFullKeepsWorkerBusy(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	busy(t, e, "busy-m1", "m1")
	probed(t, e, "ep", 10*time.Minute, "tv", 0)
	if w, cond, _ := turn(t, e, "ep"); w != "" || cond != "worker_busy" {
		t.Fatalf("m4 is excluded by its window and m1 is merely full: %q %q", w, cond)
	}
}

func TestBatchItemsGoOnPastAWindowHeldChild(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	typesOnly(t, e, "m1", "movie")
	ids := seedQueueBatch(t, e, "batch-1", 2)
	probed(t, e, "child-1", 5*time.Minute, "tv", 0)
	// the child of the first item
	inst := mustInstance(t, e.deps.Store, "child-1")
	in := parseExecutionContextMust(t, e, "child-1").Inputs
	in["parent_action_id"], in["batch_item_key"] = "batch-1", "k1"
	inst.InputsJSON = toJSON(in)
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	if err := e.deps.Store.UpdateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "k1", FilePath: "/media/batch-1/File1.mkv", Status: "waiting_for_slot", ChildActionID: "child-1"}); err != nil {
		t.Fatal(err)
	}
	if _, cond, _ := turn(t, e, "child-1"); cond != windowWaitCondition {
		t.Fatalf("the child: %q", cond)
	}
	held, err := e.deps.Store.WindowWaitingBatchItemIDs("batch-1")
	if err != nil || !held[ids[0]] || held[ids[1]] {
		t.Fatalf("window-waiting items: %v %v", held, err)
	}
	items, _ := e.deps.Store.ListTranscodeBatchItems("batch-1")
	got, ok := e.batchDispatchCandidates(context.Background(), "batch-1", items)
	if !ok || !containsIdx(got, batchIndex(t, items, ids[1])) {
		t.Fatalf("the second item was not scheduled behind the window-held first: %v %v", got, ok)
	}
	if containsIdx(got, batchIndex(t, items, ids[0])) {
		t.Fatalf("the window-held item was scheduled again: %v", got)
	}
	if n := e.batchQueueAllowance(context.Background(), "batch-1"); n < 1 {
		t.Fatalf("the window-held child holds the batch's slots: allowance %d", n)
	}
}

func TestWindowWriteWakesTheQueue(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	typesOnly(t, e, "m1", "movie")
	probed(t, e, "ep", 5*time.Minute, "tv", 0)
	if _, cond, _ := turn(t, e, "ep"); cond != windowWaitCondition {
		t.Fatal(cond)
	}
	state := actionState(t, e.deps.Store, "ep")
	state[workerSlotPolledAtKey] = clock.now().UTC().Format(time.RFC3339Nano)
	clock.advance(time.Second)
	if e.slotWakeDue(state) {
		t.Fatal("nothing changed yet")
	}
	for len(e.reconcileWake) > 0 {
		<-e.reconcileWake
	}
	if _, err := e.DeleteNodeWindow("m4", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.reconcileWake:
	default:
		t.Fatal("deleting the window did not wake the reconciler")
	}
	if !e.slotWakeDue(state) {
		t.Fatal("a window delete must bring the held conversion's poll forward")
	}
	if w, _, _ := turn(t, e, "ep"); w != "m4" {
		t.Fatalf("after the delete: %q", w)
	}
	// Writes need a real node and a matching revision.
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: "nope"}, nil); err == nil {
		t.Fatal("a window for an unknown node")
	}
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: "m1", MediaTypes: []string{"tv"}}, new(int64)); err == nil {
		t.Fatal("a stale revision was accepted")
	}
}

// What a submit does when the node's window no longer takes the conversion:
// back to the queue, not failed, with its worker released.
func TestBeginAdmissionRequeuesWhenTheWindowClosed(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	realtimeSamples(e)
	tmpl := ActionTemplate{Name: "transcode_media", Steps: []StepDefinition{{Name: "admit", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
		if _, res, handled := e.beginAdmission(ctx, ec, "submit"); handled {
			return res, nil
		}
		return StepResult{Status: StepCompleted}, nil
	}}}}
	state := map[string]any{workerStateKey: "m4", queueProbeKey: map[string]any{"duration_sec": 600.0, "width": 1920, "height": 1080}, transcodeQueueOrderKey: queueOrder(queueRankPlain, e.now().Add(-time.Minute), "c", 0)}
	seedActionInstance(t, e.deps.Store, "c", "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "tv"}, state)

	// Open window that fits: it goes through.
	windowAt(t, e, clock, "m4", time.Hour, 0)
	inst, _ := e.deps.Store.GetActionInstance("c")
	if res, err := e.execute(context.Background(), inst, parseExecutionContext(inst, e), tmpl); err != nil || res.Status == StatusFailed {
		t.Fatalf("%v %v", res, err)
	}
	if got := mustInstance(t, e.deps.Store, "c"); got.Status != StatusCompleted {
		t.Fatalf("an admitted conversion: %s", got.Status)
	}

	// The window closes before the submit.
	seedActionInstance(t, e.deps.Store, "d", "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "tv"}, state)
	typesOnly(t, e, "m4", "movie")
	w, cond, reason := turnWith(t, e, "d", tmpl)
	got := mustInstance(t, e.deps.Store, "d")
	if got.Status != StatusWaitingExternal || w != "" || cond == "" || !strings.Contains(reason, "Node m4: type tv is not allowed") {
		t.Fatalf("a closed window at submit: status %s worker %q wait %q %q", got.Status, w, cond, reason)
	}
	if st := actionState(t, e.deps.Store, "d"); assignedWorker(st) != "" || st["job_id"] != nil {
		t.Fatalf("the requeue kept the node: %v", st)
	}
}

func TestAnUnreadableWindowStoreFailsClosed(t *testing.T) {
	e, raw := faultEngine(t)
	waiter(t, e, "a", time.Minute, map[string]any{"media_type": "movie"}, nil)
	if _, err := raw.Exec(`DROP TABLE worker_windows`); err != nil {
		t.Fatal(err)
	}
	if w, cond, reason := turn(t, e, "a"); w != "" || cond != windowWaitCondition || !strings.Contains(reason, "could not be read") {
		t.Fatalf("an unreadable window store: %q %q %q", w, cond, reason)
	}
	node, _ := e.deps.Workers.Get("m4")
	if c := e.workerVideoCapacity(context.Background(), node); c != 0 {
		t.Fatalf("capacity %d with unreadable windows", c)
	}
	if !e.workerWindowClosed("m1") {
		t.Fatal("unreadable windows must read as closed")
	}
}

// The estimate reads the finished encodes of the store through the shared
// builder.
func TestWindowEstimateComesFromTheStoredEncodeHistory(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	for i := 0; i < 2; i++ {
		seedActionInstance(t, e.deps.Store, "done-"+strconv.Itoa(i), "transcode_media", StatusCompleted, 0, "", map[string]any{}, map[string]any{
			"worker":             "m4",
			"original":           map[string]any{"resolution": "1920x1080", "duration_sec": 3600.0},
			"size_budget":        map[string]any{"encoder_choice": map[string]any{"video_codec": transcode.VideoCodecLibX265}},
			"encode_duration_ms": 1800000.0,
		})
	}
	est, ok := e.maxEncodeEstimate("m4", "1080p", 3600)
	if !ok || est < 1790 || est > 1810 {
		t.Fatalf("estimate %v %v from two half-time encodes", est, ok)
	}
	if _, ok := e.maxEncodeEstimate("m1", "1080p", 3600); ok {
		t.Fatal("m1 has no history")
	}
	windowAt(t, e, clock, "m4", time.Hour, 0)
	probed(t, e, "half", 5*time.Minute, "movie", 3600)
	if w, _, _ := turn(t, e, "half"); w != "m4" {
		t.Fatalf("a half-time encode fits an hour: %q", w)
	}
}
