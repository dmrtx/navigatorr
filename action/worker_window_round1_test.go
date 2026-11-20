package action

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func seedSamples(e *Engine, node, class, family string, durSec, encSec float64) {
	var s []EncodeSample
	for i := 0; i < 3; i++ {
		s = append(s, EncodeSample{Worker: node, Class: class, Family: family, DurationSec: durSec, EncodeSec: encSec})
	}
	prev := e.loadEncodeSamples
	e.loadEncodeSamples = func() ([]EncodeSample, error) {
		var all []EncodeSample
		if prev != nil {
			p, err := prev()
			if err != nil {
				return nil, err
			}
			all = append(all, p...)
		}
		return append(all, s...), nil
	}
	e.history = encodeHistory{}
}

// Item 1: once the family is known only that family's estimate counts; before
// it, x265's, except for a conversion known to be audio-only.
func TestWindowEstimateUsesExactlyTheKnownFamily(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	seedSamples(e, "m4", "1080p", EncodeFamilyHardware, 3600, 600)
	if _, ok := e.windowEstimate("m4", windowShape{durationSec: 3600, class: "1080p"}); ok {
		t.Fatal("an unknown family borrowed the hardware history")
	}
	if _, ok := e.windowEstimate("m4", windowShape{durationSec: 3600, class: "1080p", family: EncodeFamilyX265}); ok {
		t.Fatal("x265 borrowed the hardware history")
	}
	if est, ok := e.windowEstimate("m4", windowShape{durationSec: 3600, class: "1080p", family: EncodeFamilyHardware}); !ok || est < 590 || est > 610 {
		t.Fatalf("hardware estimate %v %v", est, ok)
	}
	seedSamples(e, "m4", "1080p", EncodeFamilyCopy, 3600, 60)
	if est, ok := e.windowEstimate("m4", windowShape{durationSec: 3600, class: "1080p", audioOnly: true}); !ok || est > 100 {
		t.Fatalf("audio-only uses copy: %v %v", est, ok)
	}
	if _, ok := e.windowEstimate("m4", windowShape{durationSec: 3600, class: "1080p"}); ok {
		t.Fatal("a video conversion of unknown family used copy history")
	}
}

func TestWindowAdmitsUsesTheFamilyTheStateRecords(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	seedSamples(e, "m4", "1080p", EncodeFamilyX265, 3600, 3600)
	seedSamples(e, "m4", "1080p", EncodeFamilyHardware, 3600, 300)
	windowAt(t, e, clock, "m4", 30*time.Minute, 0)
	probe := map[string]any{"duration_sec": 3600.0, "width": 1920, "height": 1080}
	mk := func(id string, extra map[string]any) *ExecutionContext {
		st := map[string]any{workerStateKey: "m4", queueProbeKey: probe}
		for k, v := range extra {
			st[k] = v
		}
		seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "movie"}, st)
		return parseExecutionContextMust(t, e, id)
	}
	if ok, why, _ := e.windowAdmits(mk("x265", nil)); ok || !strings.Contains(why, "does not fit") {
		t.Fatalf("an hour of x265 in 30 min: %v %q", ok, why)
	}
	hw := map[string]any{"size_budget": map[string]any{"encoder_choice": map[string]any{"video_codec": "hevc_videotoolbox"}}}
	if ok, why, _ := e.windowAdmits(mk("hw", hw)); !ok {
		t.Fatalf("a hardware encode of 300 s fits: %q", why)
	}
	// A recorded family with no history on the node is no estimate.
	seedSamples(e, "m1", "1080p", EncodeFamilyX265, 3600, 100)
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: "m1", Until: clock.now().Add(time.Hour).UTC().Format(time.RFC3339)}, nil); err != nil {
		t.Fatal(err)
	}
	st := mk("m1hw", hw)
	st.State[workerStateKey] = "m1"
	if ok, why, _ := e.windowAdmits(st); ok || !strings.Contains(why, "no duration estimate") {
		t.Fatalf("hardware on a node without hardware history: %v %q", ok, why)
	}
}

// Item 2: a failed read is not cached.
func TestAFailedSampleReadIsNotCached(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	calls := 0
	fail := true
	e.loadEncodeSamples = func() ([]EncodeSample, error) {
		calls++
		if fail {
			return nil, context.DeadlineExceeded
		}
		return []EncodeSample{{Worker: "m4", Class: "1080p", Family: EncodeFamilyX265, DurationSec: 100, EncodeSec: 100}, {Worker: "m4", Class: "1080p", Family: EncodeFamilyX265, DurationSec: 100, EncodeSec: 100}}, nil
	}
	if s := e.encodeSamples(); len(s) != 0 {
		t.Fatalf("samples from a failed read: %v", s)
	}
	fail = false
	if s := e.encodeSamples(); len(s) != 2 {
		t.Fatalf("the failure was cached: %d samples after %d calls", len(s), calls)
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
	// A success is cached for the next calls.
	e.encodeSamples()
	if calls != 2 {
		t.Fatalf("a success was not cached: %d calls", calls)
	}
}

// Item 7: the window's reason is not hidden by a recent refusal.
func TestWaitReasonNamesTheWindowBeforeRecentRefusals(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	sizedWaiter(t, e, "a", 5*time.Minute, 10*gib, map[string]time.Duration{"m1": time.Minute})
	st := actionState(t, e.deps.Store, "a")
	_ = st
	inst := mustInstance(t, e.deps.Store, "a")
	in := parseExecutionContext(inst, e)
	in.Inputs["media_type"] = "tv"
	seedActionInstance(t, e.deps.Store, "b", "transcode_media", StatusWaitingExternal, 0, "", in.Inputs, in.State)
	_, cond, reason := turn(t, e, "b")
	if !strings.Contains(reason, "node m4") || !strings.Contains(reason, "type tv") {
		t.Fatalf("window exclusion missing: %q", reason)
	}
	if cond == windowWaitCondition {
		t.Fatalf("m1 only refused recently, so the wait stays worker_busy: %q", cond)
	}
}

// Item 10: boundaries.
func TestWindowBoundaries(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	realtimeSamples(e)
	closes := clock.now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: "m4", Until: closes.Format(time.RFC3339)}, nil); err != nil {
		t.Fatal(err)
	}
	pin := func(at time.Time) { e.deps.Now = func() time.Time { return at } }
	probe := map[string]any{"duration_sec": 3600.0, "width": 1920, "height": 1080}
	ec := func(id string) *ExecutionContext {
		seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "movie"}, map[string]any{workerStateKey: "m4", queueProbeKey: probe})
		return parseExecutionContextMust(t, e, id)
	}
	pin(closes)
	if ok, _, _ := e.windowAdmits(ec("at_close")); ok {
		t.Fatal("now == closes_at: nothing starts")
	}
	pin(closes.Add(-time.Hour))
	if ok, why, _ := e.windowAdmits(ec("exact")); !ok {
		t.Fatalf("now+est == closes_at starts: %q", why)
	}
	pin(closes.Add(-time.Hour + time.Second))
	if ok, _, _ := e.windowAdmits(ec("late")); ok {
		t.Fatal("one second past the fit must not start")
	}
}

// Item 8: queued batch items apply type and closed windows, and skip only the
// duration fit.
func TestQueuedBatchItemPlacementShape(t *testing.T) {
	row := store.QueuedBatchItemRow{MediaType: "tv"}
	s := queuedItemPlacement(row).window
	if !s.check || s.mediaType != "tv" || !s.noFit {
		t.Fatalf("shape %+v", s)
	}
}

// Item 9b: a conversion with no worker in its state is checked against the
// legacy (resolved) worker's window.
func TestWindowAdmitsResolvesAnEmptyWorker(t *testing.T) {
	e, clock := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	legacy := e.resolveWorker("")
	if legacy == "" {
		t.Skip("no legacy worker resolves")
	}
	typesOnly(t, e, legacy, "movie")
	_ = clock
	seedActionInstance(t, e.deps.Store, "nw", "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "tv"}, map[string]any{})
	if ok, why, _ := e.windowAdmits(parseExecutionContextMust(t, e, "nw")); ok || !strings.Contains(why, "type tv") {
		t.Fatalf("an unassigned conversion skipped the legacy node's window: %v %q", ok, why)
	}
}

// Item 9c: a window write sends assigned, unsent conversions that no longer
// qualify back to the queue and leaves submitted ones alone.
func TestWindowSweepRequeuesUnsentOnly(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	probe := map[string]any{"duration_sec": 600.0, "width": 1920, "height": 1080}
	seedActionInstance(t, e.deps.Store, "unsent", "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "tv"}, map[string]any{workerStateKey: "m4", queueProbeKey: probe, transcodeQueueOrderKey: queueOrder(queueRankPlain, e.now(), "unsent", 0)})
	seedActionInstance(t, e.deps.Store, "sent", "transcode_media", StatusWaitingExternal, 0, "", map[string]any{"media_type": "tv"}, map[string]any{workerStateKey: "m4", queueProbeKey: probe, "job_id": "j1", "transcode_submitted": true})
	seedActionInstance(t, e.deps.Store, "other", "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "tv"}, map[string]any{workerStateKey: "m1", queueProbeKey: probe})
	if _, err := e.SetNodeWindow(store.WorkerWindow{Worker: "m4", MediaTypes: []string{"movie"}}, nil); err != nil {
		t.Fatal(err)
	}
	// The write started a sweep in the background; wait for it, then sweep again
	// (a second pass changes nothing).
	for i := 0; i < 200 && assignedWorker(actionState(t, e.deps.Store, "unsent")) != ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 200 && e.windowSweeping.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	e.sweepWindowedNodes(context.Background())
	if w := assignedWorker(actionState(t, e.deps.Store, "unsent")); w != "" {
		t.Fatalf("the unsent conversion kept m4: %q", w)
	}
	if got := mustInstance(t, e.deps.Store, "unsent"); got.Status != StatusWaitingExternal || got.WaitingCondition == "" {
		t.Fatalf("unsent: %s %q", got.Status, got.WaitingCondition)
	}
	if w := assignedWorker(actionState(t, e.deps.Store, "sent")); w != "m4" {
		t.Fatalf("a submitted job was moved: %q", w)
	}
	if w := assignedWorker(actionState(t, e.deps.Store, "other")); w != "m1" {
		t.Fatalf("a node without a window was swept: %q", w)
	}
}

// Item 9a: a conversion checked before the windows existed gets its probe once.
func TestEarlyPreflightProbesOnceMoreForTheQueue(t *testing.T) {
	e, _, media, _ := setupProbeTestEnv(t, &mockTranscodeExecutor{}, ordinaryProbeJSON)
	ec := earlyCheckEC(t, e, "old", map[string]any{"path": media, "mode": "size"})
	ec.State[earlyPreflightKey] = true
	if _, decided := e.earlyPreflight(context.Background(), ec); decided {
		t.Fatal("decided")
	}
	if ec.State[queueProbeKey] == nil {
		t.Fatalf("no queue_probe after the second run: %v", ec.State)
	}
	if !getBool(ec.State, queueProbeTriedKey) {
		t.Fatal("not marked as tried")
	}
}

// Round 2, item 2: a request that arrives while a sweep runs is not lost; the
// running sweeper reads the windows again.
func TestWindowSweepRunsAgainForARequestMadeDuringASweep(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	e.windowSweepInline = false
	probe := map[string]any{"duration_sec": 600.0, "width": 1920, "height": 1080}
	seedActionInstance(t, e.deps.Store, "u", "transcode_media", StatusRunning, 0, "", map[string]any{"media_type": "tv"}, map[string]any{workerStateKey: "m4", queueProbeKey: probe, transcodeQueueOrderKey: queueOrder(queueRankPlain, e.now(), "u", 0)})
	passes := make(chan struct{}, 10)
	release := make(chan struct{})
	e.windowSweepPass = func() { passes <- struct{}{}; <-release }
	// Not yet a window: the first sweep (started by an unrelated write) runs.
	e.startWindowSweep(context.Background())
	<-passes // the first pass is running and holds
	// A window that excludes the file is written while it runs.
	if _, err := e.deps.Store.SetWorkerWindow(store.WorkerWindow{Worker: "m4", MediaTypes: []string{"movie"}}, nil); err != nil {
		t.Fatal(err)
	}
	e.startWindowSweep(context.Background()) // dropped before the fix
	release <- struct{}{}
	select {
	case <-passes:
	case <-time.After(5 * time.Second):
		t.Fatal("no second pass for the request made during the first")
	}
	e.windowSweepPass = nil
	close(release)
	for i := 0; i < 500 && (e.windowSweeping.Load() || assignedWorker(actionState(t, e.deps.Store, "u")) != ""); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if w := assignedWorker(actionState(t, e.deps.Store, "u")); w != "" {
		t.Fatalf("the second pass did not requeue: %q", w)
	}
}

// Round 2, item 3: a waiting conversion that copies the video is estimated by
// the copy family, as it is when it places itself.
func TestWaiterPlacementKeepsAudioOnly(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	waiter(t, e, "ao", time.Minute, map[string]any{"audio_only": true, "media_type": "movie"}, map[string]any{queueProbeKey: map[string]any{"duration_sec": 600.0, "width": 1920, "height": 1080}})
	waiter(t, e, "vid", time.Minute, map[string]any{"media_type": "movie"}, nil)
	ws, err := e.deps.Store.ListTranscodeQueueWaiters()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, w := range ws {
		got[w.ID] = e.waiterPlacement(w).window.audioOnly
	}
	if !got["ao"] || got["vid"] {
		t.Fatalf("audio_only on the waiter rows: %v", got)
	}
}

// Round 2, item 4: an interrupted early check leaves the marker unset.
func TestEarlyPreflightRerunMarkerNeedsADefiniteAnswer(t *testing.T) {
	e, _, media, _ := setupProbeTestEnv(t, &mockTranscodeExecutor{}, ordinaryProbeJSON)
	ec := earlyCheckEC(t, e, "old2", map[string]any{"path": media, "mode": "size"})
	ec.State[earlyPreflightKey] = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, decided := e.earlyPreflight(ctx, ec); decided {
		t.Fatal("decided")
	}
	if getBool(ec.State, queueProbeTriedKey) {
		t.Fatal("marked tried although the probe was interrupted")
	}
	if _, decided := e.earlyPreflight(context.Background(), ec); decided || !getBool(ec.State, queueProbeTriedKey) || ec.State[queueProbeKey] == nil {
		t.Fatalf("the next poll should probe: %v", ec.State)
	}
}

// Round 2, item 5 (H3): a batch's items share its media type, so an item no
// node can take means the whole batch is such; it holds nobody back and does
// not use another batch's allowance.
func TestBatchNoNodeCanTakeDoesNotLimitAnotherBatch(t *testing.T) {
	e, _ := placementEngine(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	typesOnly(t, e, "m4", "movie")
	typesOnly(t, e, "m1", "movie")
	seedQueueBatch(t, e, "tvb", 2)
	in := parseExecutionContextMust(t, e, "tvb").Inputs
	in["media_type"] = "tv"
	inst := mustInstance(t, e.deps.Store, "tvb")
	inst.InputsJSON = toJSON(in)
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	seedQueueBatch(t, e, "movieb", 1)
	in = parseExecutionContextMust(t, e, "movieb").Inputs
	in["media_type"] = "movie"
	inst = mustInstance(t, e.deps.Store, "movieb")
	inst.InputsJSON = toJSON(in)
	if err := e.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	if n := e.batchQueueAllowance(context.Background(), "movieb"); n < 1 {
		t.Fatalf("a batch that no node takes limited a movie batch: %d", n)
	}
}
