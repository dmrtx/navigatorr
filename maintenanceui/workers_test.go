package maintenanceui

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

type unavailableWorker struct{ transcode.Executor }

func (unavailableWorker) Ready(context.Context) error  { return fmt.Errorf("worker unavailable") }
func (unavailableWorker) Health(context.Context) error { return fmt.Errorf("worker unavailable") }

type inaccessibleStorageWorker struct{ transcode.Executor }

func (inaccessibleStorageWorker) Ready(context.Context) error  { return nil }
func (inaccessibleStorageWorker) Health(context.Context) error { return nil }
func (inaccessibleStorageWorker) Doctor(context.Context) error {
	return fmt.Errorf("allowed SMB root inaccessible")
}

func TestConnectedWorkerWithInaccessibleStorageStillBlocksAdmission(t *testing.T) {
	s, h := testUI(t)
	deps := s.engine.Deps()
	deps.Transcode = inaccessibleStorageWorker{}
	s.engine = action.NewEngine(deps)
	w := request(h, "GET", "/api/maintenance/workers", "", true)
	var info map[string]any
	json.Unmarshal(w.Body.Bytes(), &info)
	node := info["nodes"].([]any)[0].(map[string]any)
	if info["ready"] != false || node["connected"] != true || node["status"] != "blocked" {
		t.Fatal("liveness mistaken for readiness", info)
	}
	w = request(h, "POST", "/api/maintenance/tool", `{"name":"action_run","arguments":{"action":"transcode_media","inputs":"{\"path\":\"/blocked.mkv\"}"}}`, true)
	if w.Code != 409 {
		t.Fatal("storage failure did not block admission", w.Body.String())
	}
	rows, _ := deps.Store.ListActionInstances("", 100)
	if len(rows) != 0 {
		t.Fatal("blocked job persisted")
	}
}

func TestWorkerAvailabilityAndAdmissionRecheck(t *testing.T) {
	s, h := testUI(t)
	if request(h, "GET", "/api/maintenance/workers", "", false).Code != 401 {
		t.Fatal("worker status not protected")
	}
	w := request(h, "GET", "/api/maintenance/workers", "", true)
	var info struct {
		Ready bool             `json:"ready"`
		Nodes []map[string]any `json:"nodes"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil || !info.Ready || len(info.Nodes) != 1 {
		t.Fatal(w.Body.String())
	}
	// A ready UI observation cannot authorize a later disconnected admission.
	deps := s.engine.Deps()
	deps.Transcode = unavailableWorker{}
	s.engine = action.NewEngine(deps)
	for _, name := range []string{"transcode_media", "transcode_batch", "benchmark_transcode"} {
		body := `{"name":"action_run","arguments":{"action":"` + name + `","inputs":"{\"path\":\"/never-submitted.mkv\"}","idempotency_key":"offline"}}`
		w = request(h, "POST", "/api/maintenance/tool", body, true)
		if w.Code != 409 {
			t.Fatal("offline job admitted", name, w.Code, w.Body.String())
		}
	}
	rows, _ := deps.Store.ListActionInstances("", 100)
	if len(rows) != 0 {
		t.Fatal("offline submission persisted jobs", rows)
	}
	w = request(h, "GET", "/api/maintenance/workers", "", true)
	if json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Ready {
		t.Fatal("offline worker shown ready", w.Body.String())
	}
}

func TestWebCancelUsesRegisteredEngineCancellation(t *testing.T) {
	s, h := testUI(t)
	for _, name := range []string{"action_run", "action_status", "action_detail", "action_resume", "action_retry", "action_cancel"} {
		if s.mcp.GetTool(name) == nil {
			t.Fatalf("visible control has no registered tool: %s", name)
		}
	}
	if err := s.engine.Deps().Store.CreateActionInstance(store.ActionInstance{ID: "queued", ActionName: "transcode_media", Status: "pending", InputsJSON: `{"path":"/synthetic.mkv"}`}); err != nil {
		t.Fatal(err)
	}
	w := request(h, "POST", "/api/maintenance/tool", `{"name":"action_cancel","arguments":{"id":"queued","reason":"Cancelled in synthetic test"}}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	job, _ := s.engine.Deps().Store.GetActionInstance("queued")
	if job.Status != "cancelled" || job.CurrentStep != 0 {
		t.Fatal("cancel did not persist", job)
	}
}
