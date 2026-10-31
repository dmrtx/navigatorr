package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

// A file job that failed because the file is damaged is not retried: it would
// fail the same way. A failure the file did not cause, such as a broken new
// file, still is.
func TestRetryRefusesADamagedFile(t *testing.T) {
	e, st, media, _ := setupProbeTestEnv(t, &mockTranscodeExecutor{}, standard8BitProbeJSON)
	for id, failure := range map[string]string{
		"damaged": `{"error":"failed to probe original media (fail closed): moov atom not found"}`,
		"corrupt": `{"error":"Transcode failed (ffmpeg_input_corrupt): ffmpeg_input_corrupt: packet scan failed"}`,
	} {
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: StatusFailed, InputsJSON: toJSON(map[string]any{"path": media, "mode": "size"}), StateJSON: "{}", OutputsJSON: "{}", ErrorJSON: failure}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Retry(context.Background(), id); err == nil || !strings.Contains(err.Error(), "damaged") {
			t.Fatalf("%s: retry %v", id, err)
		}
		if inst, _ := st.GetActionInstance(id); inst.Status != StatusFailed {
			t.Fatalf("%s: status %s", id, inst.Status)
		}
	}
	if !SourceDamaged("Moov atom not found") || SourceDamaged("Invalid data found when processing input") || SourceDamaged("probing local candidate for pre-publish validation: moov atom not found") {
		t.Fatal("damaged-source match")
	}
	// A broken new file is the encode's failure: the guard lets it through.
	broken := `{"error":"Transcode failed (candidate_invalid): probing local candidate for pre-publish validation: moov atom not found"}`
	if err := st.CreateActionInstance(store.ActionInstance{ID: "broken", ActionName: "transcode_media", Status: StatusFailed, InputsJSON: toJSON(map[string]any{"path": media, "mode": "size"}), StateJSON: "{}", OutputsJSON: "{}", ErrorJSON: broken}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Retry(context.Background(), "broken"); err != nil && strings.Contains(err.Error(), "damaged") {
		t.Fatalf("broken candidate refused as damaged: %v", err)
	}
}
