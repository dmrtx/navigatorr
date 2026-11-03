package action

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// A transcode job knows its file's size from the moment it is queued, before
// its preflight runs: the list shows it while the job waits.
func TestEnqueuedTranscodeRecordsItsFileSize(t *testing.T) {
	mediaDir := t.TempDir()
	source := mediaDir + "/ep01.mkv"
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	res, err := e.Enqueue(WithOrigin(context.Background(), "web"), "transcode_media", map[string]any{"path": source, "mode": "quality"}, "sized")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.GetActionInstance(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	if got := state[SubmittedSourceBytesKey]; got != float64(len("synthetic source bytes")) || state["request_origin"] != "web" {
		t.Fatalf("state %v", state)
	}
	// Anything but a single file's transcode is not stat'ed.
	if n := e.submittedSourceBytes("transcode_batch", map[string]any{"path": source}); n != 0 {
		t.Fatal(n)
	}
	if n := e.submittedSourceBytes("transcode_media", map[string]any{"path": mediaDir}); n != 0 {
		t.Fatal("a directory has no file size", n)
	}
}
