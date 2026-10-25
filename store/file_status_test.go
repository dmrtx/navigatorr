package store

import "testing"

func TestListFileStatusRowsJoinsAttemptsAndReplacementsAndSkipsPreviews(t *testing.T) {
	s := openTest(t)
	create := func(inst ActionInstance) {
		t.Helper()
		if err := s.CreateActionInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	create(ActionInstance{ID: "real", ActionName: "transcode_batch", Status: ActionStatusRunning, InputsJSON: `{"dry_run":false}`})
	create(ActionInstance{ID: "preview", ActionName: "transcode_batch", Status: ActionStatusCompleted, InputsJSON: `{"dry_run":true}`})
	create(ActionInstance{ID: "child-done", ActionName: "transcode_media", Status: ActionStatusCompleted, OutputsJSON: `{}`})
	create(ActionInstance{ID: "child-skip", ActionName: "transcode_media", Status: ActionStatusCompleted, OutputsJSON: `{"skip_transcode":true}`})
	create(ActionInstance{ID: "promote-done", ActionName: "promote_transcode_candidate", Status: ActionStatusCompleted, IdempotencyKey: "promote:filesystem:child-done", OutputsJSON: `{"promoted":true}`})
	for _, item := range []TranscodeBatchItem{
		{BatchID: "real", ItemKey: "a", FilePath: "/media/a.mkv", Status: "queued"},
		{BatchID: "real", ItemKey: "b", FilePath: "/media/b.mkv", Status: "completed", Decision: "transcode", ChildActionID: "child-done", CandidatePath: "/media/b.new.mkv"},
		{BatchID: "real", ItemKey: "c", FilePath: "/media/c.mkv", Status: "completed", ChildActionID: "child-skip"},
		{BatchID: "preview", ItemKey: "d", FilePath: "/media/d.mkv", Status: "queued"},
	} {
		if err := s.CreateTranscodeBatchItem(item); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListFileStatusRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("a preview marked a file: %+v", rows)
	}
	byPath := map[string]FileStatusRow{}
	for _, row := range rows {
		byPath[row.FilePath] = row
	}
	if a := byPath["/media/a.mkv"]; a.BatchStatus != ActionStatusRunning || a.ChildStatus != "" || a.Replaced {
		t.Fatalf("queued file: %+v", a)
	}
	if b := byPath["/media/b.mkv"]; !b.Replaced || b.ReplacementStatus != ActionStatusCompleted || b.ChildStatus != ActionStatusCompleted || b.ChildSkipped {
		t.Fatalf("replaced file: %+v", b)
	}
	if c := byPath["/media/c.mkv"]; !c.ChildSkipped || c.Replaced {
		t.Fatalf("skipped file: %+v", c)
	}
}

func TestListFileStatusRowsFlagsAnAttemptWaitingInTheWorkersLine(t *testing.T) {
	s := openTest(t)
	create := func(inst ActionInstance) {
		t.Helper()
		if err := s.CreateActionInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	create(ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: ActionStatusRunning, InputsJSON: `{"dry_run":false}`})
	create(ActionInstance{ID: "in-line", ActionName: "transcode_media", Status: ActionStatusWaitingExternal, StateJSON: `{"transcode_phase":"queued"}`})
	create(ActionInstance{ID: "encoding", ActionName: "transcode_media", Status: ActionStatusWaitingExternal, StateJSON: `{"transcode_phase":"encoding"}`})
	create(ActionInstance{ID: "started", ActionName: "transcode_media", Status: ActionStatusWaitingExternal, OutputsJSON: `{"transcode_phase":"encoding"}`, StateJSON: `{"transcode_phase":"queued"}`})
	create(ActionInstance{ID: "left", ActionName: "transcode_media", Status: ActionStatusCompleted, StateJSON: `{"transcode_phase":"queued"}`})
	for key, child := range map[string]string{"a": "in-line", "b": "encoding", "c": "started", "d": "left"} {
		if err := s.CreateTranscodeBatchItem(TranscodeBatchItem{BatchID: "batch", ItemKey: key, FilePath: "/media/" + key + ".mkv", Status: "running", ChildActionID: child}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListFileStatusRows()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"/media/a.mkv": true, "/media/b.mkv": false, "/media/c.mkv": false, "/media/d.mkv": false}
	for _, row := range rows {
		if row.ChildQueuedOnWorker != want[row.FilePath] {
			t.Fatalf("%s: ChildQueuedOnWorker = %t", row.FilePath, row.ChildQueuedOnWorker)
		}
	}
	if len(rows) != len(want) {
		t.Fatalf("rows: %+v", rows)
	}
}
