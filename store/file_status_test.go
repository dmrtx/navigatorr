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
