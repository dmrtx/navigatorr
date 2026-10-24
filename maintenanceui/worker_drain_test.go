package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestWorkerDrainEndpointTurnsDrainOnAndOff(t *testing.T) {
	s, h := testUI(t)
	var status transcode.DrainStatus
	read := func(w interface{ Bytes() []byte }) {
		t.Helper()
		status = transcode.DrainStatus{}
		if err := json.Unmarshal(w.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
	}

	w := request(h, "GET", "/api/maintenance/worker-drain", "", true)
	if read(w.Body); w.Code != 200 || status.Enabled {
		t.Fatal("drain must start off", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{}`, `{"enabled":"yes"}`, `{"enabled":true,"extra":1}`} {
		if w := request(h, "POST", "/api/maintenance/worker-drain", bad, true); w.Code != 400 {
			t.Fatal("a malformed change must be refused", bad, w.Code)
		}
	}
	if drain, _ := s.engine.Deps().Store.GetWorkerDrain(); drain.Enabled {
		t.Fatal("a refused request must not start the drain")
	}

	w = request(h, "POST", "/api/maintenance/worker-drain", `{"enabled":true}`, true)
	if read(w.Body); w.Code != 200 || !status.Enabled || status.ReadyForDeploy || status.Since == "" {
		t.Fatal("drain on", w.Code, w.Body.String())
	}
	// The same state shows in the nodes list the header and dialog read.
	var info struct {
		Drain *transcode.DrainStatus `json:"drain"`
	}
	w = request(h, "GET", "/api/maintenance/workers", "", true)
	if json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Drain == nil || !info.Drain.Enabled {
		t.Fatal("workers must carry the drain", w.Body.String())
	}

	w = request(h, "POST", "/api/maintenance/worker-drain", `{"enabled":false}`, true)
	if read(w.Body); w.Code != 200 || status.Enabled || status.Since != "" {
		t.Fatal("resume", w.Code, w.Body.String())
	}
	if request(h, "POST", "/api/maintenance/worker-drain", `{"enabled":true}`, false).Code != 401 {
		t.Fatal("drain must need the session")
	}
}

// Finished encodes that could not be saved are listed with their action, so
// the console can offer Try again and Show in Files for each.
func TestWorkerDrainListsUnsavedEncodes(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	state := `{"job_id":"job-unsaved","transcode_submitted":true,"recovery_required":true,"resolved_path":"/media/Show/S01E01.mkv"}`
	if err := st.CreateActionInstance(store.ActionInstance{ID: "unsaved", ActionName: "transcode_media", Status: "waiting_decision", InputsJSON: `{"path":"/media/Show/S01E01.mkv","parent_action_id":"batch-1"}`, StateJSON: state}); err != nil {
		t.Fatal(err)
	}
	w := request(h, "POST", "/api/maintenance/worker-drain", `{"enabled":true}`, true)
	var status transcode.DrainStatus
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || status.PublicationPending != 1 || status.ReadyForDeploy || len(status.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(status.NotReady) == 0 || len(status.Workers) == 0 || len(status.Workers[0].InFlight) != 1 || status.Workers[0].InFlight[0].File != "S01E01.mkv" {
		t.Fatalf("the status must say what is in flight per worker and why it is not ready: %s", w.Body.String())
	}
	if job := status.Jobs[0]; job.ActionID != "unsaved" || job.ParentID != "batch-1" || job.State != transcode.DrainJobPublicationPending || job.File != "S01E01.mkv" {
		t.Fatalf("job: %+v", job)
	}
}
