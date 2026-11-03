package store

import "testing"

func TestWorkerPauseSetListResume(t *testing.T) {
	s := openTest(t)
	if pauses, err := s.ListWorkerPauses(); err != nil || len(pauses) != 0 {
		t.Fatalf("no node is paused at the start: %v %v", pauses, err)
	}
	if _, err := s.SetWorkerPause("m1", "sideways"); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
	if _, err := s.SetWorkerPause("", WorkerPauseGraceful); err == nil {
		t.Fatal("a pause needs a node")
	}

	first, err := s.SetWorkerPause("m1", WorkerPauseGraceful)
	if err != nil || first.Mode != WorkerPauseGraceful || first.Since == "" || first.Revision != 1 {
		t.Fatalf("pause: %+v %v", first, err)
	}
	again, _ := s.SetWorkerPause("m1", WorkerPauseGraceful)
	if again.Revision != first.Revision || again.Since != first.Since {
		t.Fatalf("repeating a pause changes nothing: %+v", again)
	}
	moved, _ := s.SetWorkerPause("m1", WorkerPauseImmediate)
	if moved.Mode != WorkerPauseImmediate || moved.Since != first.Since || moved.Revision != 2 {
		t.Fatalf("a mode change keeps the start and moves the revision: %+v", moved)
	}
	if _, err := s.SetWorkerPause("m4", WorkerPauseGraceful); err != nil {
		t.Fatal(err)
	}
	pauses, _ := s.ListWorkerPauses()
	if len(pauses) != 2 || pauses["m1"].Mode != WorkerPauseImmediate {
		t.Fatalf("both nodes are paused on their own: %+v", pauses)
	}
	if _, err := s.SetWorkerPause("m1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWorkerPause("m1", ""); err != nil {
		t.Fatal("resuming a node that is not paused is not an error")
	}
	pauses, _ = s.ListWorkerPauses()
	if len(pauses) != 1 || pauses["m4"].Mode != WorkerPauseGraceful {
		t.Fatalf("resume must clear only that node: %+v", pauses)
	}
}
