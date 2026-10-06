package store

import (
	"encoding/json"
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
