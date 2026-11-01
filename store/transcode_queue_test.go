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
