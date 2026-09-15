package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestJobStatusMetadataRoundTrip(t *testing.T) {
	resp := JobStatusResponse{
		ID: "job-1", Status: "running",
		Attempt: 2, RetryCount: 1, FallbackCount: 1,
		AppliedFallbacks:      []string{"fb1"},
		FailureClassification: "runner_killed",
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded transcode.JobStatus
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal into transcode.JobStatus: %v", err)
	}
	if decoded.Attempt != 2 || decoded.RetryCount != 1 || decoded.FallbackCount != 1 || decoded.FailureClassification != "runner_killed" || len(decoded.AppliedFallbacks) != 1 {
		t.Fatalf("metadata did not round-trip: %+v", decoded)
	}
}

func TestStatusDeadRunningMarksRunnerKilled(t *testing.T) {
	dir := t.TempDir()
	cfg := &WorkerConfig{StateDir: dir, AllowedRoots: []string{dir}, MaxParallelJobs: 1}
	w := NewWorker(cfg)
	w.SetAliveFunc(func(*JobRecord) bool { return false })
	jobID := "job-dead"
	jobDir := filepath.Join(dir, jobID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	rec := &JobRecord{ID: jobID, Status: "running", PID: 999999, CreatedAt: time.Now().UTC(), Attempt: 1}
	if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	st, err := w.Status(context.Background(), jobID)
	if err != nil {
		t.Fatalf("status err: %v", err)
	}
	if st.Status != "failed" {
		t.Fatalf("got %q want failed", st.Status)
	}
	if st.FailureClassification != "runner_killed" {
		t.Fatalf("classification=%q want runner_killed", st.FailureClassification)
	}
	reloaded, _ := LoadJob(filepath.Join(jobDir, "job.json"))
	if reloaded.FailureClassification != "runner_killed" || reloaded.Attempt != 1 {
		t.Fatalf("persisted record wrong: %+v", reloaded)
	}
}
