package transcodeworker

import (
	"encoding/json"
	"github.com/jakenesler/navigatorr/transcode"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActivityReportsReservationsAcrossBenchmarkAndConversion(t *testing.T) {
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	worker.cfg.MaxParallelJobs = 2
	now := time.Now().UTC()
	bench := &BenchmarkRecord{ID: "bench-active", Status: "running", PID: 1234, Source: "/media/sample-one.mkv", Phase: "evaluating_metrics", Progress: 84.4, StartedAt: now.Add(-2 * time.Minute), HeartbeatAt: now, ProgressDetails: &transcode.BenchmarkProgressDetails{CompletedUnits: 17, TotalUnits: 20}}
	job := &JobRecord{ID: "active-video", Status: "running", PID: 5678, Source: "/media/sample-two.mkv", DurationSec: 100, StartedAt: now.Add(-time.Minute), JobTelemetry: transcode.JobTelemetry{Phase: "encoding", WorkerHeartbeatAt: now}}
	os.MkdirAll(filepath.Join(worker.cfg.StateDir, bench.ID), 0700)
	os.MkdirAll(filepath.Join(worker.cfg.StateDir, job.ID), 0700)
	if err := SaveBenchmarkAtomic(filepath.Join(worker.cfg.StateDir, bench.ID, "benchmark.json"), bench); err != nil {
		t.Fatal(err)
	}
	if err := SaveJobAtomic(filepath.Join(worker.cfg.StateDir, job.ID, "job.json"), job); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(worker.cfg.StateDir, job.ID, "progress.txt"), []byte("out_time_us=30000000\nprogress=continue\n"), 0600)
	response := doRequest(t, srv, "GET", "/v1/activity", "", "secret")
	var activity transcode.WorkerActivity
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &activity) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if activity.SlotsUsed != 2 || activity.SlotsTotal != 2 || len(activity.Jobs) != 2 || activity.Jobs[0].Kind != "benchmark" || activity.Jobs[0].Details.CompletedUnits != 17 || activity.Jobs[1].Progress == nil || *activity.Jobs[1].Progress != 30 {
		t.Fatal(activity)
	}
	if got := doRequest(t, srv, "GET", "/v1/activity", "", ""); got.Code != 401 {
		t.Fatal("unauthenticated activity", got.Code)
	}
	if got := doRequest(t, srv, "POST", "/v1/activity", "", "secret"); got.Code != 405 {
		t.Fatal("activity must be read-only", got.Code)
	}
}
