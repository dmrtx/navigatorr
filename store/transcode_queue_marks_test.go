package store

import (
	"path/filepath"
	"testing"
)

func TestQueueMarksAndRevision(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if rev, err := s.QueueRevision(); err != nil || rev != 0 {
		t.Fatalf("a new store starts at revision 0: %d, %v", rev, err)
	}
	if m, err := s.GetQueueMark(QueueMarkAction, "none"); err != nil || m.Held || m.Order != "" {
		t.Fatalf("no mark is neither held nor ordered: %+v, %v", m, err)
	}
	if err := s.CreateActionInstance(ActionInstance{ID: "b1", ActionName: "transcode_batch", Status: "waiting_external", InputsJSON: "{}", OutputsJSON: "{}", StateJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTranscodeBatchItem(TranscodeBatchItem{BatchID: "b1", ItemKey: "k", FilePath: "/media/f.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	it, err := s.GetTranscodeBatchItem("b1", "k")
	if err != nil || it == nil {
		t.Fatal(it, err)
	}

	rev, err := s.ApplyQueueWrite(QueueWrite{ActionHeld: map[string]bool{"a1": true}, ActionOrder: map[string]string{"a1": "0~|x"}, ItemHeld: map[int64]bool{it.ID: true}, ItemOrder: map[int64]string{it.ID: "1|y"}, BatchOrder: map[string]string{"b1": "1|z"}})
	if err != nil || rev != 1 {
		t.Fatalf("one write is one revision: %d, %v", rev, err)
	}
	if m, _ := s.GetQueueMark(QueueMarkAction, "a1"); !m.Held || m.Order != "0~|x" {
		t.Fatalf("action mark: %+v", m)
	}
	if m, _ := s.GetQueueMark(QueueMarkBatch, "b1"); m.Held || m.Order != "1|z" {
		t.Fatalf("batch mark: %+v", m)
	}
	if m, _ := s.GetQueueItemMark(it.ID); !m.Held || m.Order != "1|y" {
		t.Fatalf("item mark: %+v", m)
	}
	if held, _ := s.HeldBatchItemIDs("b1"); !held[it.ID] {
		t.Fatalf("held items: %v", held)
	}
	// A release keeps the order; clearing the batch's item orders keeps the hold.
	if _, err := s.ApplyQueueWrite(QueueWrite{ActionHeld: map[string]bool{"a1": false}, ClearItemOrdersOf: []string{"b1"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.GetQueueMark(QueueMarkAction, "a1"); m.Held || m.Order != "0~|x" {
		t.Fatalf("a release keeps the place: %+v", m)
	}
	if m, _ := s.GetQueueItemMark(it.ID); !m.Held || m.Order != "" {
		t.Fatalf("item mark after clearing: %+v", m)
	}
	if rev, _ := s.QueueRevision(); rev != 2 {
		t.Fatalf("revision %d", rev)
	}
}
