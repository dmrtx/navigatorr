package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A manual retry of a failed benchmark or encode reads the class the worker
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
	}
	for _, kind := range []string{"benchmark", "transcode"} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				st := setupTestStore(t)
				mock := &mockTranscodeExecutor{
					benchmarkStatusFunc: func(context.Context, string) (transcode.BenchmarkStatus, error) {
						return transcode.BenchmarkStatus{Status: transcode.StatusFailed, FailureClassification: tc.class, Error: tc.message}, nil
					},
					statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
						return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, FailureClassification: tc.class, Error: tc.message}, nil
					},
				}
				e := NewEngine(EngineDeps{Store: st, Transcode: mock})
				ec := testReconcileEC("")
				ec.State["resolved_path"] = "/media/source/shared_clip.mkv"
				name, submit := "transcode_media", "submit_transcode"
				if kind == "benchmark" {
					name, submit = "benchmark_transcode", "submit_benchmark"
					ec.State["benchmark_job_id"] = "bench-old"
				} else {
					ec.State["job_id"] = "job-old"
				}
				inst := store.ActionInstance{ID: ec.InstanceID, ActionName: name, Status: StatusFailed, CurrentStep: 2, StateJSON: toJSON(ec.State)}
				if err := st.CreateActionInstance(inst); err != nil {
					t.Fatal(err)
				}
				tmpl := ActionTemplate{Steps: []StepDefinition{{Name: "preflight"}, {Name: submit}, {Name: "wait"}}}
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
}
