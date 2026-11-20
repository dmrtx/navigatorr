package store

import "testing"

// An item's own hold counts until the hold is carried to its child: between
// the child being linked and the carry-over the item is still held, and after
// it the child's mark holds, until that is released.
func TestHeldBatchItemIDsCoversTheHoldAcrossTheCarryOver(t *testing.T) {
	s := openTest(t)
	if err := s.CreateActionInstance(ActionInstance{ID: "b", ActionName: "transcode_batch", Status: ActionStatusWaitingExternal, StateJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTranscodeBatchItem(TranscodeBatchItem{BatchID: "b", ItemKey: "one", FilePath: "/m/one", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	one, _ := s.GetTranscodeBatchItem("b", "one")
	if _, err := s.ApplyQueueWrite(QueueWrite{ItemHeld: map[int64]bool{one.ID: true}}); err != nil {
		t.Fatal(err)
	}
	// The child is linked, and has no mark yet: still held.
	if err := s.UpdateTranscodeBatchItem(TranscodeBatchItem{BatchID: "b", ItemKey: "one", FilePath: "/m/one", Status: "waiting_for_slot", ChildActionID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if held, err := s.HeldBatchItemIDs("b"); err != nil || !held[one.ID] {
		t.Fatalf("held before the carry-over: %v %v", held, err)
	}
	// The carry-over: the child's mark is set as the item's flag is cleared.
	if _, err := s.ApplyQueueWrite(QueueWrite{ActionHeld: map[string]bool{"c1": true}, ItemHeld: map[int64]bool{one.ID: false}}); err != nil {
		t.Fatal(err)
	}
	if held, _ := s.HeldBatchItemIDs("b"); !held[one.ID] {
		t.Fatalf("not held through the child's mark: %v", held)
	}
	// Released through the child.
	if _, err := s.ApplyQueueWrite(QueueWrite{ActionHeld: map[string]bool{"c1": false}}); err != nil {
		t.Fatal(err)
	}
	if held, _ := s.HeldBatchItemIDs("b"); len(held) != 0 {
		t.Fatalf("held after the release: %v", held)
	}
}
