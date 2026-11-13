package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// benchJobCount mirrors the size of a long-lived production state directory.
const benchJobCount = 1500

// seedBenchState fills a state directory with finished jobs that look like
// real records (plan, conversions) plus two running encoders, and ages every
// finished record so it is not mistaken for one that is still being written.
func seedBenchState(tb testing.TB, count int) *Worker {
	tb.Helper()
	stateDir := filepath.Join(tb.TempDir(), "jobs")
	w := NewWorker(&WorkerConfig{StateDir: stateDir, MaxParallelJobs: 2})
	w.isAlive = func(*JobRecord) bool { return true }
	old := time.Now().Add(-72 * time.Hour)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("job-%05d", i)
		status := "completed"
		if i%7 == 0 {
			status = "failed"
		}
		job := &JobRecord{
			ID: id, Status: status, Source: "/media/" + id + ".mkv", Candidate: "/media/" + id + ".out.mkv",
			Profile: "default", IdempotencyKey: "key-" + id, ExecutionSpecDigest: "digest-" + id,
			CreatedAt: old, StartedAt: old, FinishedAt: old.Add(time.Hour), EncodeComplete: true,
			Plan:        &transcode.Plan{VideoCodec: "hevc_videotoolbox", Container: "mkv", Quality: 65},
			Conversions: []transcode.ConversionRecord{{}, {}},
		}
		path := filepath.Join(stateDir, id, "job.json")
		if err := SaveJobAtomic(path, job); err != nil {
			tb.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			tb.Fatal(err)
		}
	}
	// Age the directories too, so the listing is not "being modified".
	_ = os.Chtimes(stateDir, old, old)
	for _, id := range []string{"run-a", "run-b"} {
		job := &JobRecord{ID: id, Status: "running", PID: 4242, Source: "/media/" + id + ".mkv", Candidate: "/media/" + id + ".out.mkv",
			CreatedAt: time.Now().Add(-time.Hour), StartedAt: time.Now().Add(-time.Hour), Plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox"}}
		path := filepath.Join(stateDir, id, "job.json")
		if err := SaveJobAtomic(path, job); err != nil {
			tb.Fatal(err)
		}
		recent := time.Now().Add(-10 * time.Second)
		_ = os.Chtimes(path, recent, recent)
	}
	// The scheduler creates its lock file on the first tick; have it exist
	// already so the steady state is what gets measured.
	if err := os.WriteFile(filepath.Join(stateDir, ".capacity.lock"), nil, 0o600); err != nil {
		tb.Fatal(err)
	}
	_ = os.Chtimes(stateDir, old, old)
	return w
}

// BenchmarkPolledHold is what GET /v1/hold costs.
func BenchmarkPolledHold(b *testing.B) {
	w := seedBenchState(b, benchJobCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.Held()
		if _, err := w.countActiveJobs(""); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPolledSuspend is what GET /v1/suspend costs.
func BenchmarkPolledSuspend(b *testing.B) {
	w := seedBenchState(b, benchJobCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.Suspended()
		if _, _, err := w.suspendCounts(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPolledJobStatus is what GET /v1/jobs/<id> costs for a running job.
func BenchmarkPolledJobStatus(b *testing.B) {
	w := seedBenchState(b, benchJobCount)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Status(ctx, "run-a"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSchedulerTick is one idle tick of the daemon's scheduler: the
// post-encode sweep, the retained sweep and the queue drain.
func BenchmarkSchedulerTick(b *testing.B) {
	w := seedBenchState(b, benchJobCount)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.runSchedulerSweeps(ctx, "exe", "")
	}
}

// BenchmarkReadyActivity is what GET /v1/activity costs.
func BenchmarkReadyActivity(b *testing.B) {
	w := seedBenchState(b, benchJobCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.activitySnapshot(); err != nil {
			b.Fatal(err)
		}
	}
}
