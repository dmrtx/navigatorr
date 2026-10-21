package tools

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

// action_run answers a request the caller got wrong with an invalid_params
// error that names the input, and leaves nothing behind: no row, and the
// idempotency key is free for the corrected request.
func TestActionRunReportsInvalidParamsWithoutReservingTheKey(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "invalid-params.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}, Transcode: readyAdmissionWorker{}})
	s := server.NewMCPServer("test", "1")
	registerActionTools(s, e)

	for name, tc := range map[string]struct {
		action, inputs, field string
	}{
		"podcast without output_path": {"clean_podcast_ads", `{"path":"/media/show/ep01.mp3","podcast_id":"synthetic-show"}`, "output_path"},
		"compact with hardware":       {"transcode_media", `{"path":"/media/show/ep01.mkv","mode":"compact","allow_hardware":true}`, "allow_hardware"},
		"sample test":                 {"benchmark_transcode", `{"path":"/media/show/ep01.mkv"}`, ""},
		"search metric":               {"transcode_media", `{"path":"/media/show/ep01.mkv","metric":"vmaf"}`, "metric"},
	} {
		t.Run(name, func(t *testing.T) {
			result := callTool(t, s, "action_run", map[string]any{"action": tc.action, "inputs": tc.inputs, "idempotency_key": "one-key"})
			if !result.IsError {
				t.Fatalf("invalid request admitted: %+v", result)
			}
			var body struct {
				Error  string `json:"error"`
				Field  string `json:"field"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(resultText(t, result)), &body); err != nil {
				t.Fatalf("error result is not structured: %v", err)
			}
			if body.Error != "invalid_params" || body.Field != tc.field || body.Reason == "" {
				t.Fatalf("unexpected error body: %+v", body)
			}
			if rows, _ := st.ListActionInstances("", 100); len(rows) != 0 {
				t.Fatalf("action rows persisted: %+v", rows)
			}
			if old, err := st.FindActionByIdempotencyKey(tc.action, "one-key"); err != nil || old != nil {
				t.Fatalf("idempotency key reserved: %+v %v", old, err)
			}
		})
	}

	// The corrected request reuses the key that the bad one tried.
	admitted := admittedAction(t, callTool(t, s, "action_run", map[string]any{"action": "transcode_media", "inputs": `{"path":"/media/show/ep01.mkv"}`, "idempotency_key": "one-key"}))
	if admitted.ID == "" {
		t.Fatal("corrected request not admitted")
	}
}
