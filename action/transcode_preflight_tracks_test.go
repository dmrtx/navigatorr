package action

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A job that preflight resets keeps the previous run's validation outputs in
// its state. The report a reader sees must come from this run's selection or
// from none, so the stale one goes with the rest of the reset.
func TestPreflightDropsAPreviousRunsTrackReport(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	ec := &ExecutionContext{InstanceID: "stale-tracks", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "quality"}, State: map[string]any{
		trackSelectionKey: "stale",
		trackReportKey:    &TrackSelectionReport{Message: "Kept 2 of 3 audio tracks (eng, und)"},
		"tracks":          &TrackSelectionReport{Message: "Kept 2 of 3 audio tracks (eng, und)"},
		"tracks_message":  "Kept 2 of 3 audio tracks (eng, und)",
	}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	result, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || result.Status != StepCompleted {
		t.Fatalf("preflight %+v %v", result, err)
	}
	for _, key := range []string{trackSelectionKey, trackReportKey, "tracks", "tracks_message"} {
		if v, ok := ec.State[key]; ok {
			t.Errorf("state[%s] = %v survived the reset", key, v)
		}
	}
}
