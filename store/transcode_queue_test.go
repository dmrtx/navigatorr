package store

import "testing"

// Only work that can still start holds a place in the transcode queue: a
// paused, cancelling or finished batch, or a paused conversion, holds no one
// back.
func TestTranscodeQueueListsOnlyWorkThatCanStart(t *testing.T) {
	s := openTest(t)
	batch := func(id, status, state string) {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_batch", Status: status, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"one", "two"} {
			status := "queued"
			if key == "two" {
				status = "running"
			}
			if err := s.CreateTranscodeBatchItem(TranscodeBatchItem{BatchID: id, ItemKey: key, FilePath: "/media/" + id + "/" + key, Status: status}); err != nil {
				t.Fatal(err)
			}
		}
	}
	batch("live", ActionStatusWaitingExternal, `{}`)
	batch("paused", ActionStatusWaitingExternal, `{"paused":true}`)
	batch("cancelling", ActionStatusRunning, `{"cancel_requested":true}`)
	batch("done", ActionStatusCompleted, `{}`)

	items, err := s.ListQueuedBatchItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].BatchID != "live" || items[0].BatchCreatedAt == "" {
		t.Fatalf("queued items: %+v", items)
	}

	conv := func(id, inputs, state string) {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusWaitingExternal, InputsJSON: inputs, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	conv("single", `{}`, `{"transcode_queue_order":"1|a"}`)
	conv("assigned", `{}`, `{"transcode_queue_order":"1|b","worker":"node-a"}`)
	conv("paused-single", `{}`, `{"transcode_queue_order":"1|c","paused":true}`)
	conv("unordered", `{}`, `{}`)
	conv("child-live", `{"parent_action_id":"live"}`, `{"transcode_queue_order":"1|d"}`)
	conv("child-paused", `{"parent_action_id":"paused"}`, `{"transcode_queue_order":"1|e"}`)
	conv("child-done", `{"parent_action_id":"done"}`, `{"transcode_queue_order":"1|f"}`)

	waiters, err := s.ListTranscodeQueueWaiters()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range waiters {
		got[w.ID] = w.QueueOrder
	}
	if len(got) != 2 || got["single"] != "1|a" || got["child-live"] != "1|d" {
		t.Fatalf("waiters: %+v", waiters)
	}
}

// A child its paused or decision-waiting batch holds before it sent anything
// uses no slot on its worker; once it holds a job it does.
func TestActiveWorkerCountLeavesOutChildrenHeldBeforeSubmit(t *testing.T) {
	s := openTest(t)
	add := func(id, condition, state string) {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusWaitingExternal, WaitingCondition: condition, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	add("running", "transcode_complete", `{"worker":"node-a","job_id":"j1"}`)
	add("held", "parent_paused", `{"worker":"node-a"}`)
	add("held-decision", "parent_waiting_decision", `{"worker":"node-a"}`)
	add("held-with-job", "parent_paused", `{"worker":"node-a","job_id":"j2"}`)
	counts, err := s.CountActiveWorkerActions("transcode_media")
	if err != nil {
		t.Fatal(err)
	}
	if counts["node-a"] != 2 {
		t.Fatalf("counts %v, want node-a=2", counts)
	}
}

// A conversion the worker keeps queued for lack of room to retain its
// candidate runs nothing: its worker starts the next job, so one such job holds
// no slot there. A second one does: when the room is short for everything, the
// queue must not move wholesale onto that worker. Once the room is there a job
// counts again.
func TestActiveWorkerCountLeavesOutOneJobWaitingForRetentionRoomPerWorker(t *testing.T) {
	s := openTest(t)
	add := func(id, state string) {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusWaitingExternal, WaitingCondition: "transcode_complete", StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	add("a-running", `{"worker":"node-a","job_id":"j1"}`)
	add("a-no-room", `{"worker":"node-a","job_id":"j2","worker_retention_blocked":"retention_budget"}`)
	add("a-no-disk", `{"worker":"node-a","job_id":"j3","worker_retention_blocked":"free_disk"}`)
	add("a-started", `{"worker":"node-a","job_id":"j4","worker_retention_blocked":""}`)
	add("b-no-room", `{"worker":"node-b","job_id":"j5","worker_retention_blocked":"retention_budget"}`)
	add("c-running", `{"worker":"node-c","job_id":"j6"}`)
	counts, err := s.CountActiveWorkerActions("transcode_media")
	if err != nil {
		t.Fatal(err)
	}
	if counts["node-a"] != 3 || counts["node-b"] != 0 || counts["node-c"] != 1 {
		t.Fatalf("counts %v, want node-a=3 (one of its two blocked jobs left out), node-b=0, node-c=1", counts)
	}
}

func TestQueuedBatchItemsCarryTheBatchPriority(t *testing.T) {
	s := openTest(t)
	for id, inputs := range map[string]string{"first": `{"queue_priority":true}`, "plain": `{}`} {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_batch", Status: ActionStatusWaitingExternal, InputsJSON: inputs, StateJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTranscodeBatchItem(TranscodeBatchItem{BatchID: id, ItemKey: "one", FilePath: "/media/" + id, Status: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ListQueuedBatchItems()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range items {
		got[item.BatchID] = item.Priority
	}
	if len(got) != 2 || !got["first"] || got["plain"] {
		t.Fatalf("priority by batch: %v", got)
	}
}

// A waiting conversion carries the size its file had when it was submitted and
// whether it is standalone, which is what the room its worker must have to
// retain its candidate depends on.
func TestTranscodeQueueWaitersCarryTheirSizeAndWhetherTheyAreStandalone(t *testing.T) {
	s := openTest(t)
	if err := s.CreateActionInstance(ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: ActionStatusWaitingExternal, StateJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	conv := func(id, inputs, state string) {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusWaitingExternal, InputsJSON: inputs, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	conv("single", `{}`, `{"transcode_queue_order":"1|a","submitted_source_bytes":31765000000}`)
	conv("single-unknown", `{}`, `{"transcode_queue_order":"1|b"}`)
	conv("child", `{"parent_action_id":"batch"}`, `{"transcode_queue_order":"1|c","submitted_source_bytes":5000}`)

	waiters, err := s.ListTranscodeQueueWaiters()
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		bytes      int64
		standalone bool
	}
	got := map[string]shape{}
	for _, w := range waiters {
		got[w.ID] = shape{w.SourceBytes, w.Standalone}
	}
	want := map[string]shape{"single": {31765000000, true}, "single-unknown": {0, true}, "child": {5000, false}}
	if len(got) != len(want) {
		t.Fatalf("waiters: %+v", waiters)
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s = %+v, want %+v (all: %+v)", id, got[id], w, got)
		}
	}
}
