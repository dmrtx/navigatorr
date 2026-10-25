package store

import (
	"database/sql"
	"encoding/json"
	"github.com/jakenesler/navigatorr/transcode"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceSnapshotProjectsWithoutChangingHistory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"original_intact":true,"paused":false,"original_sha256":"quoted\"sha","counts":{"failed":2},"progress":12.5,"original":{"size_bytes":99},"huge":"` + strings.Repeat("x", 1024*1024) + `"}`
	inst := ActionInstance{ID: "file", ActionName: "transcode_media", Status: ActionStatusFailed, InputsJSON: `{"path":"/media/a.mkv","dry_run":false,"paths":["a","b"],"unneeded":42}`, StateJSON: state, OutputsJSON: `{"original_intact":null}`}
	if err := s.CreateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	var got, output, input map[string]any
	if err := json.Unmarshal([]byte(rows[0].StateJSON), &got); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(rows[0].OutputsJSON), &output)
	json.Unmarshal([]byte(rows[0].InputsJSON), &input)
	if got["original_intact"] != true || got["paused"] != false || got["progress"] != 12.5 || got["huge"] != nil || len(rows[0].StateJSON) > 1024 {
		t.Fatalf("projection changed JSON types or retained audit: %v", got)
	}
	if value, exists := output["original_intact"]; !exists || value != nil {
		t.Fatal("explicit null lost")
	}
	if _, exists := output["paused"]; exists {
		t.Fatal("absent key invented")
	}
	if input["dry_run"] != false || len(input["paths"].([]any)) != 2 {
		t.Fatal("selection lost")
	}
	full, _ := s.GetActionInstance(inst.ID)
	if full.StateJSON != state {
		t.Fatal("audit modified")
	}
}

func TestMaintenanceMemoTracksSameSizeWritesAndFreshMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	inst := ActionInstance{ID: "active", ActionName: "transcode_media", Status: ActionStatusWaitingExternal, StateJSON: `{"progress":12.5}`, OutputsJSON: `{"original_intact":true}`, UpdatedAt: "2026-10-06T12:00:00Z"}
	if err := writer.CreateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(first) != 1 {
		t.Fatalf("%v %v", first, err)
	}
	if len(s.maintenanceMemo) != 1 {
		t.Fatal("projection not memoized")
	}
	inst.StateJSON = `{"progress":99.5}` // Identical size and timestamp, another connection.
	inst.Status = ActionStatusFailed
	inst.CurrentStep = 4
	inst.ErrorJSON = `{"error":"fresh failure"}`
	if err := writer.UpdateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	second, err := s.ListMaintenanceActionSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if second[0].StateJSON != `{"progress":99.5}` || second[0].Status != ActionStatusFailed || second[0].CurrentStep != 4 || second[0].ErrorJSON != inst.ErrorJSON {
		t.Fatalf("stale checkpoint: %+v", second[0])
	}
	if first[0].StateJSON != `{"progress":12.5}` {
		t.Fatal("previous snapshot mutated")
	}
	if _, err := writer.db.Exec("DELETE FROM action_instances WHERE id=?", inst.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListMaintenanceActionSnapshot(); err != nil || len(rows) != 0 || len(s.maintenanceMemo) != 0 {
		t.Fatal("removed row retained", rows, err)
	}
}

func TestMaintenanceSnapshotKeepsSizeModeSummaryOnly(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"mode":"size","size_main10":true,` +
		`"quality_evidence":{"verdict":"pass","plan_digest":"d","vmaf":{"mean":93.4,"p5":89.1,"worst_window_mean":88.2,"worst_window_seconds":10,"model_id":"m","frames_total":9000}},` +
		`"conversions":[{"stream_type":"audio","stream_index":1,"from_codec":"truehd","to_codec":"aac","reason":"` + strings.Repeat("r", 4096) + `"}]}`
	if err := s.CreateActionInstance(ActionInstance{ID: "file", ActionName: "transcode_media", Status: ActionStatusCompleted, InputsJSON: `{}`, OutputsJSON: `{"quality_evidence":{"verdict":"fail"}}`, StateJSON: state}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if want := `{"conversions":[{"from_codec":"truehd","stream_index":1,"stream_type":"audio","to_codec":"aac"}],"mode":"size","quality_evidence":{"vmaf":{"mean":93.4,"p5":89.1,"worst_window_mean":88.2,"worst_window_seconds":10}},"size_main10":true}`; rows[0].StateJSON != want {
		t.Fatalf("size summary projection:\n got %s\nwant %s", rows[0].StateJSON, want)
	}
	if rows[0].OutputsJSON != `{"quality_evidence":{}}` {
		t.Fatalf("evidence without VMAF must not invent a score: %s", rows[0].OutputsJSON)
	}
}

// The snapshot keeps a benchmark winner's size and encoder, and only the media
// engine reason out of its uncertainties, so the page can say why a hardware
// result was chosen without carrying the estimator's whole list.
func TestMaintenanceSnapshotKeepsOnlyTheMediaEngineReason(t *testing.T) {
	if maintenanceHardwareNote != transcode.HardwareSelectedNote {
		t.Fatalf("the snapshot filters on %q but the selector writes %q", maintenanceHardwareNote, transcode.HardwareSelectedNote)
	}
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	note := transcode.HardwareSelectedNote + ": no software candidate qualified"
	for id, uncertainties := range map[string]string{
		"with-reason": `["sampled estimate","` + note + `"]`,
		"without":     `["sampled estimate","another"]`,
		"none":        `null`,
	} {
		state := `{"benchmark_decision":{"winner":{"candidate_id":"w","video_codec":"hevc_videotoolbox","estimated_bytes":4000,"quality":50,"uncertainties":` + uncertainties + `},"evaluations":[{"candidate_id":"w"}]}}`
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusCompleted, InputsJSON: `{}`, OutputsJSON: `{}`, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 3 {
		t.Fatalf("%v %v", rows, err)
	}
	for _, row := range rows {
		var state struct {
			Decision map[string]map[string]any `json:"benchmark_decision"`
		}
		if json.Unmarshal([]byte(row.StateJSON), &state) != nil {
			t.Fatal(row.StateJSON)
		}
		winner := state.Decision["winner"]
		if winner["estimated_bytes"] != float64(4000) || winner["video_codec"] != "hevc_videotoolbox" || winner["quality"] != nil || winner["candidate_id"] != nil || state.Decision["evaluations"] != nil {
			t.Fatalf("%s: projection kept or lost the wrong fields: %v", row.ID, state.Decision)
		}
		notes, _ := winner["uncertainties"].([]any)
		if row.ID == "with-reason" && (len(notes) != 1 || notes[0] != note) {
			t.Fatalf("the media engine reason was lost or padded: %v", winner)
		}
		if row.ID != "with-reason" && winner["uncertainties"] != nil {
			t.Fatalf("%s: uncertainties leaked: %v", row.ID, winner)
		}
	}
}

// The list keeps the per-window scores, the out-of-sample pool and how the
// winner was verified, but not the search's stages or candidate lists.
func TestMaintenanceSnapshotKeepsWindowReport(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"mode":"size",` +
		`"automatic_search":{"stages":[{"name":"discovery","candidates":["a","b"]}],"verified_samples":5,"verification_windows":"final_grid","winner_windows":[{"start_seconds":600,"duration_seconds":10,"role":"discovery","vmaf_mean":93,"vmaf_p5":90,"vmaf_min":88}],"verification":{"attempts":[{"candidate_id":"a","role":"smallest","windows":5,"outcome":"passed"}]}},` +
		`"quality_evidence":{"verdict":"pass","vmaf":{"mean":93.4,"p5":89.1},"windows":[{"start_seconds":600,"duration_seconds":10,"role":"grid","vmaf_mean":91,"vmaf_p5":88,"vmaf_min":85}],"fresh_vmaf":{"mean":90.2,"p5":86,"frames_total":300}}}`
	if err := s.CreateActionInstance(ActionInstance{ID: "file", ActionName: "transcode_media", Status: ActionStatusCompleted, InputsJSON: `{}`, OutputsJSON: `{}`, StateJSON: state}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	got := rows[0].StateJSON
	for _, want := range []string{`"verification_windows":"final_grid"`, `"winner_windows":[{"start_seconds":600,"duration_seconds":10,"role":"discovery"`, `"verification":{"attempts":[{"candidate_id":"a"`, `"windows":[{"start_seconds":600,"duration_seconds":10,"role":"grid"`, `"fresh_vmaf":{"mean":90.2,"p5":86}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("projection lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"stages"`) || strings.Contains(got, "frames_total") {
		t.Fatalf("projection kept bulk: %s", got)
	}
}

// A failed media engine result keeps the search and final score of each window
// next to the x265 result, through the list projection.
func TestMaintenanceSnapshotKeepsTheFailedHardwareResultsWindows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"mode":"size","hardware_continuation":{"outcome":"continued","message":"continued with x265","failed_candidate_id":"fallback-hardware-cand_q70",` +
		`"final":{"verdict":"fail","failures":["VMAF p5 80.2 is below the minimum 84.0"]},` +
		`"windows":[{"start_seconds":600,"duration_seconds":10,"role":"discovery","search":{"vmaf_mean":93,"vmaf_p5":90,"vmaf_min":88},"final":{"vmaf_mean":84,"vmaf_p5":80,"vmaf_min":78}}],"bulk":"x"}}`
	if err := s.CreateActionInstance(ActionInstance{ID: "file", ActionName: "transcode_media", Status: ActionStatusCompleted, InputsJSON: `{}`, OutputsJSON: `{}`, StateJSON: state}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	got := rows[0].StateJSON
	for _, want := range []string{`"hardware_continuation"`, `"windows":[{`, `"role":"discovery"`, `"search":{"vmaf_mean":93`, `"final":{"vmaf_mean":84`} {
		if !strings.Contains(got, want) {
			t.Fatalf("projection lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"bulk"`) {
		t.Fatalf("projection kept bulk: %s", got)
	}
}

func TestMaintenanceSnapshotKeepsTheSizeBudgetSummary(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"size_budget":{"budget_bytes":3000,"target_bytes":2910,"video_bitrate_kbps":2400,"outcome":"below_quality_target_within_budget","curve":[{"kind":"budget_fallback","bytes":2910}],"encoder_choice":{"video_codec":"hevc_videotoolbox","message":"ladder"},"retry_without_hardware":true,"non_video_bytes":400,"siblings":{"scope":"series","count":4,"files":[{"name":"a.mkv"}]},"final":{"verdict":"pass","measured":{"vmaf_mean":79.5},"windows":[{"start_seconds":1}],"limits":{"vmaf_mean_minimum":70}}},"size_budget_siblings":{"scope":"season","count":1,"reason":"too_few_siblings","message":"Only 1","files":[{"name":"a.mkv"}]},"size_budget_skip_message":"not applied"}`
	if err := s.CreateActionInstance(ActionInstance{ID: "budget", ActionName: "transcode_media", Status: ActionStatusCompleted, InputsJSON: `{}`, OutputsJSON: `{}`, StateJSON: state}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	var got struct {
		Budget   map[string]any `json:"size_budget"`
		Siblings map[string]any `json:"size_budget_siblings"`
		Note     string         `json:"size_budget_skip_message"`
	}
	if json.Unmarshal([]byte(rows[0].StateJSON), &got) != nil {
		t.Fatal(rows[0].StateJSON)
	}
	final, _ := got.Budget["final"].(map[string]any)
	if got.Budget["budget_bytes"] != float64(3000) || got.Budget["outcome"] != "below_quality_target_within_budget" || got.Budget["non_video_bytes"] != nil || len(got.Budget["curve"].([]any)) != 1 || got.Budget["siblings"] == nil || final["verdict"] != "pass" || final["windows"] != nil || final["limits"] != nil {
		t.Fatalf("projection kept or lost the wrong fields: %v", got.Budget)
	}
	if choice, _ := got.Budget["encoder_choice"].(map[string]any); choice["video_codec"] != "hevc_videotoolbox" || got.Budget["retry_without_hardware"] != true {
		t.Fatalf("projection lost the encoder choice or the retry offer: %v", got.Budget)
	}
	if got.Siblings["reason"] != "too_few_siblings" || got.Siblings["message"] != "Only 1" || got.Siblings["files"] != nil || got.Note != "not applied" {
		t.Fatalf("sibling evidence projection %v %q", got.Siblings, got.Note)
	}
}

// A failed replacement's page needs to know whether the library import was sent
// (the library may then have removed the original itself), never the command.
func TestMaintenanceSnapshotSaysWhetherTheLibraryImportWasSentWithoutTheCommand(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sent := `{"promotion":{"service":"sonarr","recovery_mode":"move_aside","commands":{"import":{"id":7,"sent_at":"2026-01-01T00:00:00Z","payload":{"files":[{"path":"/media/.c/secret.mkv"}]}}}}}`
	notSent := `{"promotion":{"service":"sonarr","recovery_mode":"move_aside","commands":{}}}`
	// A recorded command with no body still counts as sent: the safer reading.
	nullBody := `{"promotion":{"service":"sonarr","recovery_mode":"move_aside","commands":{"import":null}}}`
	for id, state := range map[string]string{"sent": sent, "not-sent": notSent, "null-body": nullBody} {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "promote_transcode_candidate", Status: ActionStatusFailed, InputsJSON: `{}`, OutputsJSON: state, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 3 {
		t.Fatalf("%v %v", rows, err)
	}
	for _, row := range rows {
		for _, got := range []string{row.OutputsJSON, row.StateJSON} {
			if strings.Contains(got, "commands") || strings.Contains(got, "secret.mkv") {
				t.Fatalf("%s kept the command: %s", row.ID, got)
			}
			if has := strings.Contains(got, `"import_sent":true`); has != (row.ID != "not-sent") {
				t.Fatalf("%s import_sent=%v: %s", row.ID, has, got)
			}
		}
	}
}

// The queue needs to know which candidates a prepared plan covers and whether
// it was approved; the candidate paths and hashes stay in the batch.
func TestMaintenanceSnapshotKeepsThePlanMembersWithoutCandidatePathsOrHashes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"batch_promotion_approved":true,"work_progress":{"phase":"copying_recovery","bytes_read":5,"total_bytes":10},"tracks_message":"Kept two audio tracks","batch_promotion_plan":{"digest":"sha256:plan","members":[{"item_key":"file-a","episode":"Sample Show S01E01","transcode_action_id":"t-a","original_path":"/media/a.mkv","candidate_path":"/media/.c/a.mkv","candidate_sha256":"secret-hash"}]}}`
	if err := s.CreateActionInstance(ActionInstance{ID: "plan", ActionName: "transcode_batch", Status: ActionStatusRunning, InputsJSON: `{}`, OutputsJSON: `{}`, StateJSON: state}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	got := rows[0].StateJSON
	for _, leaked := range []string{"candidate_path", "candidate_sha256", "secret-hash", "sha256:plan"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("the projection kept %q: %s", leaked, got)
		}
	}
	for _, kept := range []string{`"batch_promotion_approved":true`, `"item_key":"file-a"`, `"transcode_action_id":"t-a"`, `"copying_recovery"`, "Kept two audio tracks"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("the projection lost %q: %s", kept, got)
		}
	}
}

func TestMaintenanceSnapshotIsNewestFirstThenByID(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"b", "a", "old", "new", "other-kind"} {
		name := "transcode_media"
		if id == "other-kind" {
			name = "validate_torrent"
		}
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: name, Status: ActionStatusCompleted}); err != nil {
			t.Fatal(err)
		}
	}
	for id, at := range map[string]string{"old": "2026-10-01T00:00:00Z", "a": "2026-10-10T00:00:00Z", "b": "2026-10-10T00:00:00Z", "new": "2026-10-20T00:00:00Z", "other-kind": "2026-10-25T00:00:00Z"} {
		if _, err := s.db.Exec(`UPDATE action_instances SET updated_at=? WHERE id=?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.ID)
	}
	if strings.Join(got, ",") != "new,a,b,old" {
		t.Fatalf("order = %v, want newest first, ties by id, other kinds excluded", got)
	}
}

func TestMaintenanceSnapshotReadsOnlyActionsWhoseRevisionMoved(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"quiet", "busy"} {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusRunning, StateJSON: `{"progress":1}`, UpdatedAt: "2026-10-06T12:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ListMaintenanceActionSnapshot(); err != nil {
		t.Fatal(err)
	}
	// A payload the snapshot has already read is not read again while its
	// revision stands: change it behind the revision's back, and the snapshot
	// keeps the projection it has.
	if _, err := s.db.Exec(`DROP TRIGGER action_instance_revs_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE action_instances SET state_json='{"progress":50}' WHERE id='quiet'`); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "quiet" && row.StateJSON != `{"progress":1}` {
			t.Fatalf("an unchanged revision was read again: %s", row.StateJSON)
		}
	}
}

func TestActionRevisionMovesOnEveryWrite(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rev := func() (int64, bool) {
		var n int64
		err := s.db.QueryRow(`SELECT rev FROM action_instance_revs WHERE id='a'`).Scan(&n)
		return n, err == nil
	}
	inst := ActionInstance{ID: "a", ActionName: "transcode_media", Status: ActionStatusRunning, StateJSON: `{"progress":12.5}`, UpdatedAt: "2026-10-06T12:00:00Z"}
	if err := s.CreateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	first, ok := rev()
	if !ok || first < 1 {
		t.Fatalf("no revision after insert: %d %v", first, ok)
	}
	inst.StateJSON = `{"progress":99.5}` // Same size, same timestamp.
	if err := s.UpdateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	if second, _ := rev(); second <= first {
		t.Fatalf("revision did not move on update: %d -> %d", first, second)
	}
	if _, err := s.db.Exec(`UPDATE action_instances SET state_json = json_set(state_json, '$.worker', 'm1') WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if third, _ := rev(); third <= first+1 {
		t.Fatalf("revision did not move on a raw update: %d", third)
	}
	if _, err := s.db.Exec(`DELETE FROM action_instances WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := rev(); ok {
		t.Fatal("revision outlived its action")
	}
}

func TestMigrationV17RevisesActionsAlreadyStored(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 16 {
			continue
		}
		for _, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("applying migration %d: %v", m.version, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, 'now')`, m.version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO action_instances (id, action_name, status, created_at, updated_at) VALUES ('old', 'transcode_batch', 'completed', 'now', '2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListMaintenanceActionSnapshot()
	if err != nil || len(rows) != 1 || rows[0].ID != "old" {
		t.Fatalf("an action stored before the migration is missing: %v %v", rows, err)
	}
}
