package action

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
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

// A stat that does not answer (a stalled network mount) leaves the job
// without a size instead of holding up its admission.
func TestSubmittedSizeStatIsBounded(t *testing.T) {
	mediaDir := t.TempDir()
	source := mediaDir + "/ep01.mkv"
	if err := os.WriteFile(source, []byte("synthetic"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	release := make(chan struct{})
	defer close(release)
	oldStat, oldTimeout := submittedStat, submittedStatTimeout
	submittedStat = func(string) (os.FileInfo, error) { <-release; return nil, os.ErrNotExist }
	submittedStatTimeout = 50 * time.Millisecond
	defer func() { submittedStat, submittedStatTimeout = oldStat, oldTimeout }()
	began := time.Now()
	res, err := e.Enqueue(WithOrigin(context.Background(), "web"), "transcode_media", map[string]any{"path": source, "mode": "quality"}, "stalled")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("admission waited %v on the stat", elapsed)
	}
	inst, err := st.GetActionInstance(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	if _, ok := state[SubmittedSourceBytesKey]; ok {
		t.Fatalf("a stat that timed out recorded a size: %v", state)
	}
}
