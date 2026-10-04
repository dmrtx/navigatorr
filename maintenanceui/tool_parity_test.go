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
