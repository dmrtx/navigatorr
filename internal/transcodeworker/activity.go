package transcodeworker

import (
	"fmt"
	"github.com/jakenesler/navigatorr/transcode"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Reservation slots cover staging, encoding, metrics and publishing; they are
// not a CPU utilization measurement or the benchmark's inner concurrency.
func (w *Worker) activitySnapshot() (transcode.WorkerActivity, error) {
	activity := transcode.WorkerActivity{SlotsTotal: max(1, w.cfg.MaxParallelJobs), CheckedAt: time.Now().UTC(), DetailsAvailable: true, Jobs: []transcode.WorkerActivityJob{}}
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return activity, fmt.Errorf("worker activity unavailable")
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(w.cfg.StateDir, entry.Name())
		if job, err := LoadJob(filepath.Join(dir, "job.json")); err == nil && job.ID == entry.Name() && job.PID > 0 && (job.Status == "running" || job.Status == "queued") {
			var progress *float64
			measured := ParseProgress(filepath.Join(dir, "progress.txt"), job.DurationSec)
			if measured.Valid {
				progress = &measured.Progress
			}
			activity.Jobs = append(activity.Jobs, transcode.WorkerActivityJob{ID: job.ID, Kind: "transcode", File: filepath.Base(job.Source), Phase: job.Phase, StartedAt: job.StartedAt, HeartbeatAt: job.WorkerHeartbeatAt, LastProgressAt: measured.UpdatedAt, Progress: progress})
		}
		if bench, err := LoadBenchmark(filepath.Join(dir, "benchmark.json")); err == nil && bench.ID == entry.Name() && bench.PID > 0 && (bench.Status == "running" || bench.Status == "queued") {
			progress := bench.Progress
			activity.Jobs = append(activity.Jobs, transcode.WorkerActivityJob{ID: bench.ID, Kind: "benchmark", File: filepath.Base(bench.Source), Phase: bench.Phase, StartedAt: bench.StartedAt, HeartbeatAt: bench.HeartbeatAt, LastProgressAt: bench.LastProgressAt, Progress: &progress, Details: bench.ProgressDetails})
		}
	}
	sort.Slice(activity.Jobs, func(i, j int) bool {
		if !activity.Jobs[i].StartedAt.Equal(activity.Jobs[j].StartedAt) {
			return activity.Jobs[i].StartedAt.Before(activity.Jobs[j].StartedAt)
		}
		return activity.Jobs[i].ID < activity.Jobs[j].ID
	})
	activity.SlotsUsed = len(activity.Jobs)
	if len(activity.Jobs) > 16 {
		activity.Jobs = activity.Jobs[:16]
	}
	return activity, nil
}
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, 405, "use GET")
		return
	}
	if s.worker == nil {
		writeHTTPError(w, 503, "worker unavailable")
		return
	}
	activity, err := s.worker.activitySnapshot()
	if err != nil {
		writeHTTPError(w, 503, "worker activity unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeHTTPJSON(w, 200, activity)
}
