package maintenanceui

import (
	"encoding/json"
	"testing"
)

// A submission the caller got wrong is a 400 that names the input. Nothing is
// written, so the corrected submission reuses the same key.
func TestInvalidSubmissionIsABadRequestAndLeavesTheKeyFree(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	for name, tc := range map[string]struct {
		action, inputs, field string
	}{
		"podcast without output_path": {"clean_podcast_ads", `{"path":"/media/show/ep01.mp3","podcast_id":"synthetic-show"}`, "output_path"},
		"path outside the roots":      {"transcode_media", `{"path":"/elsewhere/ep01.mkv"}`, "path"},
		"max_height without compact":  {"transcode_media", `{"path":"/media/show/ep01.mkv","max_height":720}`, "max_height"},
		// The sample search was removed: sample tests and the inputs that
		// only tuned it are refused, by name where there is one.
		"sample test":          {"benchmark_transcode", `{"path":"/media/show/ep01.mkv"}`, ""},
		"search metric":        {"transcode_media", `{"path":"/media/show/ep01.mkv","metric":"vmaf"}`, "metric"},
		"batch quality intent": {"transcode_batch", `{"paths":["/media/show/ep01.mkv"],"priority":"savings"}`, "priority"},
	} {
		t.Run(name, func(t *testing.T) {
			arguments, _ := json.Marshal(map[string]any{"name": "action_run", "arguments": map[string]any{"action": tc.action, "inputs": tc.inputs, "idempotency_key": "browser-key"}})
			w := request(h, "POST", "/api/maintenance/tool", string(arguments), true)
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
				Field string `json:"field"`
			}
			if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "invalid_params" || body.Field != tc.field || body.Error == "" {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if rows, _ := st.ListActionInstances("", 100); len(rows) != 0 {
				t.Fatalf("rows persisted: %+v", rows)
			}
			if old, err := st.FindActionByIdempotencyKey(tc.action, "browser-key"); err != nil || old != nil {
				t.Fatalf("key reserved: %+v %v", old, err)
			}
		})
	}
	good := `{"name":"action_run","arguments":{"action":"transcode_media","inputs":"{\"path\":\"/media/show/ep01.mkv\"}","idempotency_key":"browser-key"}}`
	if w := request(h, "POST", "/api/maintenance/tool", good, true); w.Code != 202 {
		t.Fatalf("corrected submission refused: %d %s", w.Code, w.Body.String())
	}
}
