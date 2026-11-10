package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestWebAdmissionShortcutsAndRestrictedMutation(t *testing.T) {
	s, h := testUI(t)
	for _, body := range []string{
		`{"name":"action_run","arguments":{"action":"transcode_media","path":"/media/shortcut.mkv"}}`,
		`{"name":"action_run","arguments":{"action":"transcode_media","path":"/media/ignored.mkv","inputs":"{\"path\":\"/media/preferred.mkv\",\"idempotency_key\":\"nested-receipt\"}"}}`,
	} {
		w := request(h, "POST", "/api/maintenance/tool", body, true)
		var result struct {
			ID string `json:"id"`
		}
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		inst, err := s.engine.Deps().Store.GetActionInstance(result.ID)
		if err != nil {
			t.Fatal(err)
		}
		var inputs map[string]any
		json.Unmarshal([]byte(inst.InputsJSON), &inputs)
		if inputs["path"] != "/media/shortcut.mkv" && inputs["path"] != "/media/preferred.mkv" {
			t.Fatal("shortcut input lost", inputs)
		}
		if inputs["path"] == "/media/preferred.mkv" && inst.IdempotencyKey != "nested-receipt" {
			t.Fatal("receipt lost", inst.IdempotencyKey)
		}
	}
	if err := s.engine.Deps().Store.CreateActionInstance(store.ActionInstance{ID: "outside", ActionName: "safe_media_replacement", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"action_retry", "action_resume", "action_cancel"} {
		w := request(h, "POST", "/api/maintenance/tool", `{"name":"`+name+`","arguments":{"id":"outside"}}`, true)
		if w.Code != 403 {
			t.Fatal("unrelated workflow mutation allowed", name, w.Code)
		}
	}
}

func TestPhaseCostsShareTheActionStatusProjection(t *testing.T) {
	s, h := testUI(t)
	costs := map[string]any{"coordinator_inventory": map[string]any{"duration_ms": 15, "attempts": 1, "active_compute_ms": nil, "nas_read_bytes": nil, "provenance": "coordinator_measured"}}
	seedOperation(t, s.engine.Deps().Store, "measured", "transcode_batch", "completed", nil, nil, map[string]any{"phase_costs": costs})
	w := request(h, "GET", "/api/maintenance/operations?status=all", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page operationsPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("unexpected jobs: %+v", page)
	}
	ui, _ := json.Marshal(page.Jobs[0]["phase_costs"])
	response := request(h, "POST", "/api/maintenance/tool", `{"name":"action_status","arguments":{"id":"measured"}}`, true)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var mcpResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &mcpResult); err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Action struct {
			PhaseCosts map[string]any `json:"phase_costs"`
		} `json:"action"`
	}
	if len(mcpResult.Content) == 0 {
		t.Fatal(response.Body.String())
	}
	if err := json.Unmarshal([]byte(mcpResult.Content[0].Text), &summary); err != nil {
		t.Fatal(err)
	}
	mcpCosts, _ := json.Marshal(summary.Action.PhaseCosts)
	if string(ui) != string(mcpCosts) {
		t.Fatalf("UI/MCP measured costs differ: UI=%s MCP=%s", ui, mcpCosts)
	}
}

// The worker's per-step final validation costs survive the operations
// snapshot of a conversion: the page and action_status both list them, nested
// under the validation phase in the shape the coordinator mirrors them
// (TestWorkerValidationStepCostsAreMirrored), with wall time, attempts, runs
// and CPU time, and never as phases of their own.
func TestValidationStepCostsReachTheOperationsPage(t *testing.T) {
	s, h := testUI(t)
	cost := func(duration, attempts, runs, cpu int) map[string]any {
		c := map[string]any{"duration_ms": duration, "attempts": attempts, "active_compute_ms": cpu, "nas_read_bytes": nil, "nas_written_bytes": nil, "cache_hits": nil, "cache_misses": nil, "provenance": "worker_measured", "timing_scope": "phase_wall; phases may overlap"}
		if runs > 0 {
			c["runs"] = runs
		}
		return c
	}
	stepNames := []string{"extract", "timestamps", "alignment", "vmaf", "cambi", "ssim", "chroma"}
	steps := map[string]any{}
	for i, name := range stepNames {
		steps[name] = cost(1000*(i+1), 1, 7, 2000*(i+1))
	}
	validation := cost(240000, 1, 0, 1900000)
	validation["steps"] = steps
	costs := map[string]any{"worker_encode": cost(900000, 1, 0, 7000000), "worker_validation": validation}
	seedOperation(t, s.engine.Deps().Store, "validated", "transcode_media", "completed", map[string]any{"path": "/media/a.mkv"}, map[string]any{"phase_costs": costs}, map[string]any{"phase_costs": costs})
	w := request(h, "GET", "/api/maintenance/operations?id=validated", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page operationsPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("unexpected jobs: %+v", page)
	}
	shown, _ := page.Jobs[0]["phase_costs"].(map[string]any)
	if len(shown) != 2 {
		t.Fatalf("steps listed as phases of their own: %v", shown)
	}
	shownValidation, _ := shown["worker_validation"].(map[string]any)
	shownSteps, _ := shownValidation["steps"].(map[string]any)
	for _, name := range stepNames {
		c, _ := shownSteps[name].(map[string]any)
		if c["attempts"] != float64(1) || c["runs"] != float64(7) || c["active_compute_ms"] == nil || c["duration_ms"] == nil {
			t.Fatalf("%s missing from the operations page: %v", name, shownValidation)
		}
	}
	response := request(h, "POST", "/api/maintenance/tool", `{"name":"action_status","arguments":{"id":"validated"}}`, true)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var mcpResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &mcpResult); err != nil || len(mcpResult.Content) == 0 {
		t.Fatal(err, response.Body.String())
	}
	var summary struct {
		Action struct {
			PhaseCosts map[string]any `json:"phase_costs"`
		} `json:"action"`
	}
	if err := json.Unmarshal([]byte(mcpResult.Content[0].Text), &summary); err != nil {
		t.Fatal(err)
	}
	ui, _ := json.Marshal(shown)
	mcp, _ := json.Marshal(summary.Action.PhaseCosts)
	if string(ui) != string(mcp) {
		t.Fatalf("UI/MCP validation step costs differ: UI=%s MCP=%s", ui, mcp)
	}
}
