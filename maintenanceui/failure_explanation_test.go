package maintenanceui

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func failedRecord(errorText string, state map[string]any) operationRecord {
	return operationRecord{inst: store.ActionInstance{ID: "job", ActionName: "transcode_media", Status: "failed", ErrorJSON: errorText}, inputs: map[string]any{"path": "/media/a.mkv", "mode": "size"}, outputs: map[string]any{}, state: state}
}

// A failure says why in plain words, and blocks Retry only where running it
// again cannot change anything.
func TestFailuresSayWhyAndWhetherRetryCanHelp(t *testing.T) {
	for name, tc := range map[string]struct {
		err      string
		state    map[string]any
		happened string
		blocked  bool
		steps    string
	}{
		"damaged MP4":       {"failed to probe original media /media/a.mp4 (fail closed): ffprobe tool_exit after 61ms: exit status 1: moov atom not found", nil, "MP4 index", true, ""},
		"corrupt packets":   {"Transcode failed (ffmpeg_input_corrupt): ffmpeg_input_corrupt: packet scan failed", nil, "broken data", true, ""},
		"tracks mismatch":   {"choosing the tracks to keep: keep_audio_streams names stream 4, which is not an audio stream of this file", nil, "names stream 4", true, "tracks"},
		"file replaced":     {"Transcode failed (source_changed): size differs", nil, "changed while the job ran", true, "tracks"},
		"no permission":     {"Transcode failed (storage_permission_denied): open: permission denied", nil, "not allowed", true, "tracks"},
		"worker restarted":  {"Transcode failed (runner_killed): signal: killed", nil, "could not be reached", false, ""},
		"recorded class":    {"something odd", map[string]any{"failure_classification": "worker_unreachable"}, "could not be reached", false, ""},
		"check failed":      {"Transcode failed (candidate_invalid): quality_final_sample_misaligned: window 4", nil, "quality measurement", false, ""},
		"probe failed once": {"failed to probe original media /media/a.mkv (fail closed): ffprobe tool_exit: Invalid data found when processing input", nil, "could not be read", false, ""},
		"unknown":           {"Transcode failed (ffmpeg_unknown): exit status 1", nil, "ffmpeg unknown", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e, blocked, steps := explainFailure(failedRecord(tc.err, tc.state))
			if !strings.Contains(e.Happened, tc.happened) {
				t.Fatalf("happened %q", e.Happened)
			}
			if (blocked != "") != tc.blocked || stepIDs(steps) != tc.steps {
				t.Fatalf("blocked %q steps %q", blocked, stepIDs(steps))
			}
		})
	}
}

// A standalone failed job carries the explanation and, when Retry cannot help,
// says so and offers its steps instead; a batch file leaves those to its row.
func TestFailedJobViewBlocksAUselessRetry(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	damaged := `{"error":"failed to probe original media /media/a.mp4 (fail closed): moov atom not found"}`
	for id, inputs := range map[string]map[string]any{
		"lone":  {"path": "/media/a.mp4", "mode": "size"},
		"child": {"path": "/media/a.mp4", "mode": "size", "parent_action_id": "batch-x", "batch_item_key": "k"},
	} {
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: "failed", InputsJSON: mustJSON(t, inputs), OutputsJSON: "{}", StateJSON: "{}", ErrorJSON: damaged}); err != nil {
			t.Fatal(err)
		}
	}
	lone := rerunJobView(t, h, "lone")
	if operationMap(lone["explanation"])["happened"] == nil || lone["retry_blocked"] == nil || lone["next_steps"] == nil {
		t.Fatalf("lone view %v", lone)
	}
	child := rerunJobView(t, h, "child")
	if operationMap(child["explanation"])["happened"] == nil || child["retry_blocked"] != nil || child["next_steps"] != nil {
		t.Fatalf("child view %v", child)
	}
}
