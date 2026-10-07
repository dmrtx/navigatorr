package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestWebAdmissionShortcutsAndRestrictedMutation(t *testing.T) {
	s, h := testUI(t)
	for _, body := range []string{
		`{"name":"action_run","arguments":{"action":"transcode_media","path":"/shortcut"}}`,
		`{"name":"action_run","arguments":{"action":"transcode_media","path":"/ignored","inputs":"{\"path\":\"/preferred\",\"idempotency_key\":\"nested-receipt\"}"}}`,
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
		if inputs["path"] != "/shortcut" && inputs["path"] != "/preferred" {
			t.Fatal("shortcut input lost", inputs)
		}
		if inputs["path"] == "/preferred" && inst.IdempotencyKey != "nested-receipt" {
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
