package action

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// An action that ran the removed sample search and is not finished when the
// engine meets it is ended as failed through one path, whatever it was waiting
// for, and nothing it had planned touches the media or starts new work.
func TestRemovedSearchActionsAreRetiredNotExecuted(t *testing.T) {
	v3, err := legacyModePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := legacyModePolicy("quality", false)
	if err != nil {
		t.Fatal(err)
	}
	v4 := *freezeFresh(t, map[string]any{"mode": "quality"})
	cases := []struct {
		name    string
		action  string
		status  string
		step    int
		inputs  map[string]any
		state   map[string]any
		retired bool
	}{
		{"sample test", "benchmark_transcode", StatusWaitingExternal, 2, map[string]any{"profile": "general-hevc"}, map[string]any{"benchmark_job_id": "bench-1"}, true},
		{"sample test waiting for a decision", "benchmark_transcode", StatusWaitingDecision, 2, map[string]any{"profile": "general-hevc"}, map[string]any{}, true},
		{"goal frozen under v3", "transcode_media", StatusWaitingExternal, 2, map[string]any{"mode": "size"}, map[string]any{"mode_policy": v3, "benchmark_job_id": "bench-1"}, true},
		{"goal frozen under v2", "transcode_media", StatusWaitingExternal, 2, map[string]any{"mode": "quality"}, map[string]any{"mode_policy": v2}, true},
		{"goal frozen under v3 with an encode running", "transcode_media", StatusWaitingExternal, 4, map[string]any{"mode": "size"}, map[string]any{"mode_policy": v3, "job_id": "job-old", "transcode_submitted": true}, true},
		{"fixed profile with a metric", "transcode_media", StatusWaitingExternal, 2, map[string]any{"profile": "general-hevc", "metric": "vmaf"}, map[string]any{}, true},
		{"fixed profile that optimized", "transcode_media", StatusRunning, 2, map[string]any{"profile": "general-hevc"}, map[string]any{"optimization_enabled": true}, true},
		{"batch with shared calibration", "transcode_batch", StatusWaitingExternal, 1, map[string]any{"paths": []string{"/media/a.mkv"}, "shared_calibration": true}, map[string]any{}, true},
		{"goal frozen under v4", "transcode_media", StatusWaitingExternal, 4, map[string]any{"mode": "quality"}, map[string]any{"mode_policy": v4}, false},
		{"fixed profile", "transcode_media", StatusWaitingExternal, 4, map[string]any{"profile": "hevc-vt"}, map[string]any{"optimization_enabled": false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{}
			e, st, media, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)
			before, err := os.ReadFile(media)
			if err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{}
			for k, v := range tc.inputs {
				inputs[k] = v
			}
			if tc.action != "transcode_batch" {
				inputs["path"] = media
			}
			seedActionInstance(t, st, "old-action", tc.action, tc.status, tc.step, "", inputs, tc.state)
			inst, _ := st.GetActionInstance("old-action")
			if got := removedSearchAction(inst, parseExecutionContext(inst, e)); got != tc.retired {
				t.Fatalf("removedSearchAction = %v, want %v", got, tc.retired)
			}
			if !tc.retired {
				return
			}

			res, err := e.Resume(context.Background(), "old-action", "", nil)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if res.Status != StatusFailed || !strings.Contains(res.Error, removedSearchMessage) {
				t.Fatalf("not retired as failed: %s %q", res.Status, res.Error)
			}
			stored, _ := st.GetActionInstance("old-action")
			if stored.Status != StatusFailed || stored.WaitingCondition != "" || stored.WaitingOptionsJSON != "[]" {
				t.Fatalf("stored action %+v", stored)
			}
			var failure struct{ Error string }
			if err := json.Unmarshal([]byte(stored.ErrorJSON), &failure); err != nil || failure.Error != removedSearchMessage {
				t.Fatalf("stored error %q: %v", stored.ErrorJSON, err)
			}
			// It ran nothing: no encode was submitted, and
			// the media is as it was.
			if atomic.LoadInt32(&mock.submitCalls) != 0 {
				t.Fatalf("retired action submitted an encode: %d", mock.submitCalls)
			}
			after, err := os.ReadFile(media)
			if err != nil || string(after) != string(before) {
				t.Fatalf("media changed: %v", err)
			}
			// An encode it still owned on the worker is stopped.
			wantCancels := int32(0)
			if getString(tc.state, "job_id") != "" {
				wantCancels = 1
			}
			if got := atomic.LoadInt32(&mock.cancelCalls); got != wantCancels {
				t.Fatalf("worker cancels = %d, want %d", got, wantCancels)
			}
			// The same action meets the engine again (a reconciler pass, a
			// second click): it stays as it ended.
			again, err := e.Resume(context.Background(), "old-action", "", nil)
			if err != nil || again.Status != StatusFailed || again.Error != res.Error {
				t.Fatalf("second resume: %+v %v", again, err)
			}
			// Starting again is a new request, not a retry of the old one.
			if _, err := e.Retry(context.Background(), "old-action"); err == nil || !strings.Contains(err.Error(), "removed sample search") {
				t.Fatalf("retry of a retired action: %v", err)
			}
		})
	}
}

// Finished actions of the removed search stay readable as they were stored.
func TestRemovedSearchActionsStayReadable(t *testing.T) {
	v3, err := legacyModePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	e, st, media, _ := setupProbeTestEnv(t, &mockTranscodeExecutor{}, standard8BitProbeJSON)
	seedActionInstance(t, st, "old-sample-test", "benchmark_transcode", StatusCompleted, 3, "", map[string]any{"path": media, "profile": "general-hevc"},
		map[string]any{"benchmark_job_id": "bench-1", "has_winner": true})
	seedActionInstance(t, st, "old-goal", "transcode_media", StatusCompleted, 6, "", map[string]any{"path": media, "mode": "size"},
		map[string]any{"mode_policy": v3, "benchmark_done": true, "has_winner": true})
	seedActionInstance(t, st, "old-batch", "transcode_batch", StatusFailed, 2, "", map[string]any{"service": "sonarr", "series_id": 1, "shared_calibration": true, "metric": "vmaf"}, map[string]any{})

	for _, id := range []string{"old-sample-test", "old-goal", "old-batch"} {
		res, err := e.Status(context.Background(), id)
		if err != nil || res == nil {
			t.Fatalf("status %s: %+v %v", id, res, err)
		}
		stored, _ := st.GetActionInstance(id)
		if res.Status != stored.Status {
			t.Fatalf("%s: status %s, stored %s", id, res.Status, stored.Status)
		}
		// A finished action is returned untouched by a resume.
		resumed, err := e.Resume(context.Background(), id, "", nil)
		if err != nil || resumed.Status != stored.Status {
			t.Fatalf("%s: resume of a finished action: %+v %v", id, resumed, err)
		}
	}
	list, err := e.ListPaged(context.Background(), "all", 10, 0)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %d %v", len(list), err)
	}
}

// A user's cancel decision ends the action as cancelled, never as a failure.
func TestCancelDecisionEndsAsCancelled(t *testing.T) {
	mock := &mockTranscodeExecutor{
		capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			caps := modeTestCapabilities()
			caps.Encoders = map[string]bool{}
			return caps, nil
		},
	}
	e, st, media, _ := setupProbeTestEnv(t, mock, h264SDRProbeJSON)
	res, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "mode": "quality"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected a decision to be asked for, got %s (%s) %v", res.Status, res.Error, res.Outputs)
	}
	cancel := false
	for _, option := range res.WaitingOptions {
		cancel = cancel || option.Decision == "cancel"
	}
	if !cancel {
		t.Fatalf("no cancel option offered: %+v", res.WaitingOptions)
	}
	res, err = e.Resume(context.Background(), res.ID, "cancel", nil)
	if err != nil {
		t.Fatalf("cancel decision: %v", err)
	}
	stored, _ := st.GetActionInstance(res.ID)
	if res.Status != StatusCancelled || stored.Status != StatusCancelled || stored.ErrorJSON != "" && stored.ErrorJSON != "{}" || res.Outputs["reason_code"] != "user_cancelled" {
		t.Fatalf("cancel decision ended as %s/%s error=%q outputs=%v", res.Status, stored.Status, stored.ErrorJSON, res.Outputs)
	}
	if mock.submitCalls != 0 {
		t.Fatalf("a cancelled action submitted %d encodes", mock.submitCalls)
	}
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("media missing: %v", err)
	}
}

// An H.264 8-bit SDR source, the kind a goal re-encodes.
const h264SDRProbeJSON = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"46","bit_rate":"3000000"},"chapters":[]}`
