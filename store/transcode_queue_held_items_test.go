package store

import "testing"

// An item's own hold counts only while it has no child: once it has one, the
// child's mark is the hold, and a stale item flag must not keep it held.
func TestHeldBatchItemIDsHonoursAnItemHoldOnlyWithoutAChild(t *testing.T) {
	s := openTest(t)
	if err := s.CreateActionInstance(ActionInstance{ID: "b", ActionName: "transcode_batch", Status: ActionStatusWaitingExternal, StateJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"one", "two"} {
		if err := s.CreateTranscodeBatchItem(TranscodeBatchItem{BatchID: "b", ItemKey: k, FilePath: "/m/" + k, Status: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	one, _ := s.GetTranscodeBatchItem("b", "one")
	two, _ := s.GetTranscodeBatchItem("b", "two")
	if _, err := s.ApplyQueueWrite(QueueWrite{ItemHeld: map[int64]bool{one.ID: true, two.ID: true}}); err != nil {
		t.Fatal(err)
	}
	held, err := s.HeldBatchItemIDs("b")
	if err != nil || !held[one.ID] || !held[two.ID] {
		t.Fatalf("held: %v %v", held, err)
	}
	// "two" gets a child: its stale item flag no longer holds it.
	if err := s.UpdateTranscodeBatchItem(TranscodeBatchItem{BatchID: "b", ItemKey: "two", FilePath: "/m/two", Status: "waiting_for_slot", ChildActionID: "c2"}); err != nil {
		t.Fatal(err)
	}
	held, _ = s.HeldBatchItemIDs("b")
	if !held[one.ID] || held[two.ID] {
		t.Fatalf("held after the child: %v", held)
	}
	// The child's own mark does hold it.
	if _, err := s.ApplyQueueWrite(QueueWrite{ActionHeld: map[string]bool{"c2": true}}); err != nil {
		t.Fatal(err)
	}
	if held, _ = s.HeldBatchItemIDs("b"); !held[two.ID] {
		t.Fatalf("a held child's item is not held: %v", held)
	}
}
