package action

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
)

func TestSourceChangedSincePlanned(t *testing.T) {
	for _, tc := range []struct {
		name             string
		planned, current int64
		want             bool
	}{
		{"same size", 1_000_000, 1_000_000, false},
		{"a few percent smaller", 1_000_000, 950_000, false},
		{"a few percent larger", 1_000_000, 1_050_000, false},
		{"exactly at the tolerance, smaller", 1_000_000, 900_000, false},
		{"exactly at the tolerance, larger", 1_000_000, 1_100_000, false},
		{"just past the tolerance, smaller", 1_000_000, 899_999, true},
		{"just past the tolerance, larger", 1_000_000, 1_100_001, true},
		// Planned at 4.7 GB, converted to 460 MB by the time it ran.
		{"already converted", 4_700_000_000, 460_000_000, true},
		{"replaced by a bigger release", 2_000_000_000, 4_000_000_000, true},
		{"a planned size that is not a multiple of ten", 15, 17, true},
		{"the same, inside the tolerance", 15, 16, false},
		{"sizes too big to multiply", math.MaxInt64, 1, true},
		{"sizes too big to multiply, unchanged", math.MaxInt64 - 1, math.MaxInt64, false},
		{"planned size unknown", 0, 500, false},
		{"current size unknown", 500, 0, false},
		{"neither known", 0, 0, false},
	} {
		if got := sourceChangedSincePlanned(tc.planned, tc.current); got != tc.want {
			t.Errorf("%s: sourceChangedSincePlanned(%d, %d) = %v, want %v", tc.name, tc.planned, tc.current, got, tc.want)
		}
	}
}

// staleBatch seeds a one-item batch whose item was planned at plannedBytes and
// whose file now has currentBytes (a sparse file), and returns what a test
// needs to run the item.
func staleBatch(t *testing.T, plannedBytes, currentBytes int64, state map[string]any) (*Engine, *store.Store, *ExecutionContext, store.TranscodeBatchItem) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Show S03E22.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(currentBytes); err != nil {
		t.Fatal(err)
	}
	f.Close()
	res, err := fsop.NewResolver([]string{dir}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Fs: res})
	seedBatchParent(t, st, "stale-batch", StatusWaitingExternal, state)
	item := store.TranscodeBatchItem{BatchID: "stale-batch", ItemKey: "epfile-22", FilePath: path, Status: "queued", Decision: "transcode", Profile: "general-hevc", SourceBytes: plannedBytes}
	if err := st.CreateTranscodeBatchItem(item); err != nil {
		t.Fatal(err)
	}
	parent, err := st.GetActionInstance("stale-batch")
	if err != nil {
		t.Fatal(err)
	}
	return e, st, parseExecutionContext(parent, e), item
}

func TestABatchItemWhoseFileChangedIsSkippedWithoutAnEncode(t *testing.T) {
	e, st, ec, item := staleBatch(t, 1_000_000, 100_000, map[string]any{})
	busy, err := e.processBatchItem(context.Background(), &item, ec, "Show", false)
	if err != nil || busy {
		t.Fatalf("a skipped item is neither an error nor a busy worker: busy=%v err=%v", busy, err)
	}
	if item.Status != "skip" || item.Decision != "skip" {
		t.Fatalf("the item must be skipped: %+v", item)
	}
	if len(item.Reasons) != 1 || !strings.Contains(item.Reasons[0], "no longer the one this batch planned") {
		t.Fatalf("the skip must say why: %v", item.Reasons)
	}
	if item.ChildActionID != "" {
		t.Fatalf("no encode may start for a changed file: %s", item.ChildActionID)
	}
	if child, err := st.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, item)); err != nil || child != nil {
		t.Fatalf("no child action may exist: %+v %v", child, err)
	}
	stored, err := st.ListTranscodeBatchItems("stale-batch")
	if err != nil || len(stored) != 1 || stored[0].Status != "skip" || len(stored[0].Reasons) != 1 {
		t.Fatalf("the skip must be stored: %+v %v", stored, err)
	}
}

func TestABatchItemWhoseFileIsAsPlannedStarts(t *testing.T) {
	// A few percent either way is not another file.
	for _, current := range []int64{1_000_000, 960_000, 1_040_000} {
		e, _, ec, item := staleBatch(t, 1_000_000, current, map[string]any{})
		if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
			t.Fatal(err)
		}
		if item.Status == "skip" || item.ChildActionID == "" {
			t.Fatalf("a file of %d bytes planned at 1000000 must start: %+v", current, item)
		}
	}
}

func TestABatchItemWithoutAPlannedSizeStarts(t *testing.T) {
	// Items planned before the size was recorded have none.
	e, _, ec, item := staleBatch(t, 0, 100_000, map[string]any{})
	if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
		t.Fatal(err)
	}
	if item.Status == "skip" || item.ChildActionID == "" {
		t.Fatalf("an unsized item must start as it always did: %+v", item)
	}
}

func TestABatchItemWhoseFileCannotBeMeasuredStarts(t *testing.T) {
	e, _, ec, item := staleBatch(t, 1_000_000, 100_000, map[string]any{})
	item.FilePath = filepath.Join(filepath.Dir(item.FilePath), "gone.mkv")
	if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
		t.Fatal(err)
	}
	if item.Status == "skip" || item.ChildActionID == "" {
		t.Fatalf("an unmeasurable file is the encode's to judge: %+v", item)
	}
}

func TestAnItemQueuedAgainWhoseFileChangedIsSkipped(t *testing.T) {
	// New settings only reach the originals the batch planned and has not
	// replaced, so a file converted by another request in between is not one to
	// encode again.
	state := map[string]any{"batch_item_generations": map[string]any{"epfile-22": 1}}
	e, st, ec, item := staleBatch(t, 1_000_000, 100_000, state)
	item.Reasons = []string{"New settings requested"}
	if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
		t.Fatal(err)
	}
	if item.Status != "skip" || item.ChildActionID != "" {
		t.Fatalf("a re-queued item whose file changed is skipped without an encode: %+v", item)
	}
	if len(item.Reasons) != 2 || item.Reasons[0] != "New settings requested" || !strings.Contains(item.Reasons[1], "no longer the one this batch planned") {
		t.Fatalf("the skip keeps the request and says why: %v", item.Reasons)
	}
	if child, err := st.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, item)); err != nil || child != nil {
		t.Fatalf("no attempt may start: %+v %v", child, err)
	}
}

func TestAnItemQueuedAgainWhoseFileIsAsPlannedStarts(t *testing.T) {
	state := map[string]any{"batch_item_generations": map[string]any{"epfile-22": 1}}
	e, st, ec, item := staleBatch(t, 1_000_000, 1_000_000, state)
	if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
		t.Fatal(err)
	}
	if item.Status == "skip" || item.ChildActionID == "" {
		t.Fatalf("a re-queued item whose file is as planned must start: %+v", item)
	}
	child, err := st.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, item))
	if err != nil || child == nil || !strings.HasSuffix(child.IdempotencyKey, "-attempt-1") {
		t.Fatalf("the new attempt has its own key: %+v %v", child, err)
	}
}

func TestAnItemWithAnEncodeAlreadyIsNotJudgedAgain(t *testing.T) {
	e, st, ec, item := staleBatch(t, 1_000_000, 100_000, map[string]any{})
	seedActionInstance(t, st, "child-done", "transcode_media", StatusCompleted, 0, batchChildKey(ec, item),
		map[string]any{"path": item.FilePath, "parent_action_id": "stale-batch"}, map[string]any{})
	if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
		t.Fatal(err)
	}
	if item.ChildActionID != "child-done" || item.Status != "completed" {
		t.Fatalf("a started encode keeps its own record: %+v", item)
	}
}

func TestASkippedStaleItemDoesNotHoldTheSlotOfTheNext(t *testing.T) {
	e, st, _, stale := staleBatch(t, 1_000_000, 100_000, map[string]any{})
	fresh := store.TranscodeBatchItem{BatchID: "stale-batch", ItemKey: "epfile-23", FilePath: filepath.Join(filepath.Dir(stale.FilePath), "Show S03E23.mkv"), Status: "queued", Decision: "transcode", Profile: "general-hevc", SourceBytes: 1_000_000}
	if err := os.WriteFile(fresh.FilePath, make([]byte, 1_000_000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(fresh); err != nil {
		t.Fatal(err)
	}
	parent, err := st.GetActionInstance("stale-batch")
	if err != nil {
		t.Fatal(err)
	}
	// One slot: the stale item comes first and must not use it up.
	if got := e.batchAvailableSlots(context.Background(), nil, 0); got != 1 {
		t.Fatalf("this test needs a single slot, got %d", got)
	}
	if _, err := e.stepTranscodeBatchSchedule(context.Background(), parseExecutionContext(parent, e)); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListTranscodeBatchItems("stale-batch")
	if err != nil || len(items) != 2 {
		t.Fatalf("items: %+v %v", items, err)
	}
	byKey := map[string]store.TranscodeBatchItem{}
	for _, it := range items {
		byKey[it.ItemKey] = it
	}
	if got := byKey["epfile-22"]; got.Status != "skip" || got.ChildActionID != "" {
		t.Fatalf("the stale item is skipped without an encode: %+v", got)
	}
	if got := byKey["epfile-23"]; got.Status == "queued" || got.ChildActionID == "" {
		t.Fatalf("the next item must have started in the same pass: %+v", got)
	}
}

func TestAnItemQueuedAgainAfterAnAttemptIsStillJudgedAgainstThePlan(t *testing.T) {
	// An attempt ran on the planned file, the user asked for new settings, and
	// before the new attempt started the file was converted by another request:
	// recordBatchAttempt clears the child, and the plan is still what the file is
	// compared with.
	e, st, ec, item := staleBatch(t, 1_000_000, 1_000_000, map[string]any{})
	seedActionInstance(t, st, "child-done", "transcode_media", StatusCompleted, 0, batchChildKey(ec, item),
		map[string]any{"path": item.FilePath, "parent_action_id": "stale-batch"}, map[string]any{})
	item.ChildActionID = "child-done"
	recordBatchAttempt(ec, &item)
	if item.ChildActionID != "" || item.Status != "queued" {
		t.Fatalf("the attempt is retired and the item queued again: %+v", item)
	}
	if err := os.Truncate(item.FilePath, 100_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.processBatchItem(context.Background(), &item, ec, "Show", false); err != nil {
		t.Fatal(err)
	}
	if item.Status != "skip" || item.ChildActionID != "" {
		t.Fatalf("a file converted since the plan is skipped even when the item was queued again: %+v", item)
	}
	if child, err := st.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, item)); err != nil || child != nil {
		t.Fatalf("the new attempt must not start: %+v %v", child, err)
	}
	if old, err := st.GetActionInstance("child-done"); err != nil || old.Status != StatusCompleted {
		t.Fatalf("the earlier attempt keeps its record: %+v %v", old, err)
	}
}

// The premise of judging a re-queued item against the plan: the batch only
// takes new settings for the originals it planned and has not replaced, so the
// planned size is still the file's. Exercised through ReconfigureBatch.
func TestSettingsChangeRequeuesOnlyUnreplacedOriginalsAndTheyAreJudgedAgainstThePlan(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	e := candidateEngine(t, st, root, nil)
	season := filepath.Join(root, "Sample Show", "Season 1")
	names := []string{"one", "two", "three", "four"}
	seedActionInstance(t, st, "batch", "transcode_batch", StatusCompleted, 2, "", map[string]any{"service": "sonarr"}, nil)
	for _, name := range names {
		path := filepath.Join(season, "sample-episode-"+name+".mkv")
		writeCandidateFile(t, path, strings.Repeat("x", 1000))
		candidate := seedCandidateAttempt(t, st, "old-"+name, StatusCompleted, path, "job-"+name,
			map[string]any{"parent_action_id": "batch"}, map[string]any{"original_intact": true})
		writeCandidateFile(t, candidate, strings.Repeat("y", 50))
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: name, FilePath: path, Status: "completed",
			ChildActionID: "old-" + name, CandidatePath: candidate, Decision: "transcode", SourceBytes: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	// The replacement of "two" has started and the one of "four" is done: each
	// keeps its own file and history.
	seedActionInstance(t, st, "replacement", "promote_transcode_candidate", StatusWaitingDecision, 1, "promote:filesystem:old-two",
		map[string]any{"transcode_action_id": "old-two"}, nil)
	seedActionInstance(t, st, "replacement-done", "promote_transcode_candidate", StatusCompleted, 1, "promote:sonarr:old-four",
		map[string]any{"transcode_action_id": "old-four"}, nil)
	// "five" was skipped because its file changed, and is queued again.
	fivePath := filepath.Join(season, "sample-episode-five.mkv")
	writeCandidateFile(t, fivePath, strings.Repeat("x", 100))
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "five", FilePath: fivePath, Status: "skip", Decision: "skip",
		Reasons: []string{"Not converted: the file is no longer the one this batch planned (1000 B then, 100 B now)"}, SourceBytes: 1000}); err != nil {
		t.Fatal(err)
	}

	root0, _ := st.GetActionInstance("batch")
	items, _ := st.ListTranscodeBatchItems("batch")
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "all", "", BatchSelectionVersion(root0, items), "", "new-settings", map[string]any{"profile": "new"}); err != nil {
		t.Fatal(err)
	}
	byKey := func() map[string]store.TranscodeBatchItem {
		stored, err := st.ListTranscodeBatchItems("batch")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]store.TranscodeBatchItem{}
		for _, it := range stored {
			out[it.ItemKey] = it
		}
		return out
	}
	queued := byKey()
	for name, child := range map[string]string{"two": "old-two", "four": "old-four"} {
		if got := queued[name]; got.ChildActionID != child || got.Status != "completed" {
			t.Fatalf("%s has a replacement under way or done and is not queued again: %+v", name, got)
		}
	}
	for _, name := range []string{"one", "three", "five"} {
		if got := queued[name]; got.Status != "queued" || got.ChildActionID != "" || len(got.Reasons) != 1 || got.Reasons[0] != "New settings requested" {
			t.Fatalf("%s must be queued again for new settings: %+v", name, got)
		}
	}

	// Another request converts "one" while it waits; "three" is as planned.
	if err := os.Truncate(queued["one"].FilePath, 100); err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(mustInstance(t, st, "batch"), e)
	one, three := queued["one"], queued["three"]
	if _, err := e.processBatchItem(context.Background(), &one, ec, "Sample Show", false); err != nil {
		t.Fatal(err)
	}
	if one.Status != "skip" || one.ChildActionID != "" || len(one.Reasons) != 2 || !strings.Contains(one.Reasons[1], "no longer the one this batch planned") {
		t.Fatalf("the converted file is skipped, not encoded again: %+v", one)
	}
	// A file skipped before and queued again is skipped again; its old reason
	// does not pile up.
	five := queued["five"]
	if _, err := e.processBatchItem(context.Background(), &five, ec, "Sample Show", false); err != nil {
		t.Fatal(err)
	}
	if five.Status != "skip" || len(five.Reasons) != 2 || five.Reasons[0] != "New settings requested" || !strings.Contains(five.Reasons[1], "no longer the one this batch planned") {
		t.Fatalf("a skipped file queued again is skipped again with one reason each: %+v", five)
	}
	if child, err := st.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, one)); err != nil || child != nil {
		t.Fatalf("no attempt may start for the converted file: %+v %v", child, err)
	}
	if _, err := e.processBatchItem(context.Background(), &three, ec, "Sample Show", false); err != nil {
		t.Fatal(err)
	}
	child, err := st.FindActionByIdempotencyKey("transcode_media", batchChildKey(ec, three))
	if three.Status == "skip" || err != nil || child == nil || !strings.HasSuffix(child.IdempotencyKey, "-attempt-1") {
		t.Fatalf("the file as planned starts its new attempt: %+v %+v %v", three, child, err)
	}
}
