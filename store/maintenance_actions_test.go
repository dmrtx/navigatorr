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
