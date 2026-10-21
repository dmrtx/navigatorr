package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A manual retry of a failed encode reads the class the worker
// recorded, and reads the message only for a record that has none. The message
// names paths and tool output; "source" and "sha" in a path used to make it
// source_changed, and a permission phrase in ffmpeg's text used to block a
// retry the worker had classified as retryable.
func TestManualRetryPrefersTheWorkersFailureClass(t *testing.T) {
	cases := []struct {
		name      string
		class     string
		message   string
		wantRetry bool
	}{
		{"worker class retryable, message looks permanent", "media_probe_failed", "inspecting source media /media/source/shared_clip.mkv: permission denied", true},
		{"worker class permanent, message looks harmless", "source_changed", "something went wrong", false},
		{"worker class unclassified", "unclassified", "storage_full: no space left on device", true},
		{"no worker class: message decides (legacy record)", "", "no space left on device", false},
		{"no worker class, benign message", "", "inspecting source media /media/source/shared_clip.mkv: End of file", true},
		// The worker's own detections of a changed source: re-copying and
		// re-hashing the same source cannot succeed, so an unchanged manual
		// retry is rejected, with or without the recorded class.
		{"source digest mismatch, typed", "source_changed", "staging input: sizemismatch: local source bytes do not match expected sha256 (fail closed, cache entry never published)", false},
		{"source digest mismatch, legacy record", "", "staging input: local source bytes do not match expected sha256 (fail closed, cache entry never published)", false},
		{"source changed while hashing, typed", "source_changed", "transcode source file invalid: local file changed while hashing /work/source/shared_clip.mkv", false},
		// A candidate that failed its integrity check is not a source
		// problem: the retry re-encodes it, and stays allowed.
		{"candidate invalid, typed", "candidate_invalid", "transcode source file invalid: local candidate no longer matches its accepted sha256", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := setupTestStore(t)
			mock := &mockTranscodeExecutor{
				statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
					return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, FailureClassification: tc.class, Error: tc.message}, nil
				},
			}
			e := NewEngine(EngineDeps{Store: st, Transcode: mock})
			ec := testReconcileEC("")
			ec.State["resolved_path"] = "/media/source/shared_clip.mkv"
			ec.State["job_id"] = "job-old"
			inst := store.ActionInstance{ID: ec.InstanceID, ActionName: "transcode_media", Status: StatusFailed, CurrentStep: 2, StateJSON: toJSON(ec.State)}
			if err := st.CreateActionInstance(inst); err != nil {
				t.Fatal(err)
			}
			tmpl := ActionTemplate{Steps: []StepDefinition{{Name: "preflight"}, {Name: "submit_transcode"}, {Name: "wait"}}}
			resume, err := e.prepareRemoteRetry(context.Background(), &inst, ec, tmpl, 2)
			if tc.wantRetry {
				if err != nil || resume != 1 {
					t.Fatalf("retry refused: resume=%d err=%v", resume, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "retry blocked") || resume != 2 {
				t.Fatalf("retry allowed for a permanent class: resume=%d err=%v", resume, err)
			}
		})
	}
}
