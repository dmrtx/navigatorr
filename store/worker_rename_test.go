package store

import (
	"strings"
	"testing"
	"time"
)

func TestRenameWorkerMovesEverythingKeyedByTheFormerName(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	if err := s.CreateActionInstance(ActionInstance{ID: "a1", ActionName: "transcode_media", StateJSON: `{"worker":"m1","upload_worker":"m1","job_id":"j"}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateActionInstance(ActionInstance{ID: "a2", ActionName: "transcode_media", StateJSON: `{"worker":"m4"}`}); err != nil {
		t.Fatal(err)
	}
	build, err := s.CreateBuild(Build{Target: "m1", Source: "ci", Holder: "r", Kind: "k", Title: "t", PreemptMode: "graceful"}, now, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO transcode_batch_items (batch_id, item_key, file_path, display_label, decision, profile, reasons_json, status, child_action_id, candidate_path, error, attempts, created_at, updated_at, worker)
		VALUES ('b1', 'k1', '/p/e1.mkv', 'e1', 'transcode', 'general-hevc', '[]', 'queued', 'c1', '', '', 0, 'now', 'now', 'm1'),
		       ('b1', 'k2', '/p/e2.mkv', 'e2', 'transcode', 'general-hevc', '[]', 'queued', 'c2', '', '', 0, 'now', 'now', 'm4')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWorkerSlotSettings("m1", 2, 1, 3, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWorkerPause("m1", WorkerPauseGraceful); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBuildPause("m1", true); err != nil {
		t.Fatal(err)
	}

	n, err := s.RenameWorker("m1", "m1pro")
	if err != nil || n == 0 {
		t.Fatal(n, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state_json FROM action_instances WHERE id = 'a1'`).Scan(&state); err != nil || strings.Contains(state, `"m1"`) || strings.Count(state, `"m1pro"`) != 2 {
		t.Fatal("both worker keys follow", state, err)
	}
	if err := s.db.QueryRow(`SELECT state_json FROM action_instances WHERE id = 'a2'`).Scan(&state); err != nil || !strings.Contains(state, `"m4"`) {
		t.Fatal("another worker's action is left alone", state, err)
	}
	var onNew, onOld, other int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transcode_batch_items WHERE worker = 'm1pro'`).Scan(&onNew); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transcode_batch_items WHERE worker = 'm1'`).Scan(&onOld); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transcode_batch_items WHERE worker = 'm4'`).Scan(&other); err != nil || onNew != 1 || onOld != 0 || other != 1 {
		t.Fatal("batch items follow, another worker's stay", onNew, onOld, other, err)
	}
	if got, err := s.GetBuild(build.ID); err != nil || got.Target != "m1pro" {
		t.Fatal("the build target follows", got, err)
	}
	if row, err := s.GetWorkerSlotSettings("m1pro"); err != nil || row == nil || row.VideoSlots != 2 {
		t.Fatal("slot settings follow", row, err)
	}
	if old, _ := s.GetWorkerSlotSettings("m1"); old != nil {
		t.Fatal("nothing stays under the former name", old)
	}
	if pauses, _ := s.ListWorkerPauses(); pauses["m1pro"].Mode != WorkerPauseGraceful || len(pauses) != 1 {
		t.Fatal("the transcode pause follows", pauses)
	}
	if paused, _ := s.BuildsPaused("m1pro"); !paused {
		t.Fatal("the builds pause follows")
	}
	if paused, _ := s.BuildsPaused("m1"); paused {
		t.Fatal("the former name holds no pause")
	}
	// Again: nothing is left to move.
	if n, err := s.RenameWorker("m1", "m1pro"); err != nil || n != 0 {
		t.Fatal("idempotent", n, err)
	}
}

func TestRenameWorkerKeepsTheCurrentNamesOwnRows(t *testing.T) {
	s := openTest(t)
	if _, err := s.SetWorkerSlotSettings("m1", 1, 1, 2, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWorkerSlotSettings("m1pro", 3, 1, 4, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameWorker("m1", "m1pro"); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.GetWorkerSlotSettings("m1pro"); row == nil || row.VideoSlots != 3 {
		t.Fatal("the row under the current name wins", row)
	}
	if old, _ := s.GetWorkerSlotSettings("m1"); old != nil {
		t.Fatal("the former name's row is dropped", old)
	}
	if _, err := s.RenameWorker("m1", "m1"); err == nil {
		t.Fatal("two different names are required")
	}
}
