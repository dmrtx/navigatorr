package transcodeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchedulerBothSweepsFailureRecoveryAndStaleness(t *testing.T) {
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir()})
	if got := w.SchedulerObservation(time.Now()); got.Health != "unknown" {
		t.Fatal(got)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	failed := map[string]bool{"post_encode": true, "queue": true}
	w.schedulerSweepHook = func(name string) error {
		if failed[name] {
			return errors.New("SMB transport error token=SECRET /private/nas/source.mkv https://user:pass@example.invalid")
		}
		return nil
	}
	// Exercise both real scheduler loop dispatches; neither failed sweep kills
	// the loop or prevents the other sweep from being observed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := w.RunScheduler(ctx, "unused", "", time.Hour)
	defer stop()
	got := w.SchedulerObservation(time.Now())
	if got.Health != "degraded" || len(got.Sweeps) != 2 || got.Sweeps["post_encode"].ConsecutiveErrors != 1 || got.Sweeps["queue"].ConsecutiveErrors != 1 {
		t.Fatal(got)
	}
	initial := logs.String()
	w.runSchedulerSweeps(ctx, "", "")
	if logs.String() != initial {
		t.Fatal("repeated identical failures were not deduplicated")
	}
	failed["post_encode"] = false
	w.runSchedulerSweeps(ctx, "", "")
	if got = w.SchedulerObservation(time.Now()); got.Health != "degraded" || got.Sweeps["post_encode"].LastError != nil || got.Sweeps["queue"].LastError == nil {
		t.Fatal(got)
	}
	failed["queue"] = false
	w.runSchedulerSweeps(ctx, "", "")
	got = w.SchedulerObservation(time.Now())
	if got.Health != "ok" || got.LastError != nil || got.LastSuccessAt.IsZero() {
		t.Fatal(got)
	}
	if stale := w.SchedulerObservation(got.FreshUntil.Add(time.Second)); stale.Health != "unknown" || !stale.Stale {
		t.Fatal(stale)
	}
	data, _ := json.Marshal(got)
	combined := string(data) + logs.String()
	for _, private := range []string{"SECRET", "/private/nas", "user:pass", "example.invalid"} {
		if strings.Contains(combined, private) {
			t.Fatalf("private error leaked: %s", private)
		}
	}
}

func TestSchedulerPublicDiagnosticDeterministicWhenBothSweepsFail(t *testing.T) {
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir()})
	now := time.Now().UTC()
	w.recordSchedulerSweep("post_encode", now, now, 0, context.Canceled)
	w.recordSchedulerSweep("queue", now, now, 0, context.DeadlineExceeded)
	for i := 0; i < 50; i++ {
		got := w.SchedulerObservation(now)
		if got.LastError.Class != "timeout" || !got.LastSuccessAt.IsZero() {
			t.Fatal(got)
		}
	}
}

func TestSchedulerSurfacesMalformedQueueAndRecovery(t *testing.T) {
	dir := t.TempDir()
	w := NewWorker(&WorkerConfig{StateDir: dir, MaxParallelJobs: 1})
	jobDir := filepath.Join(dir, "broken")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	w.runSchedulerSweeps(context.Background(), "unused", "")
	got := w.SchedulerObservation(time.Now())
	if got.Health != "degraded" || got.Sweeps["queue"].LastError == nil || got.Sweeps["post_encode"].LastError == nil {
		t.Fatal("malformed durable state hidden", got)
	}
	if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), &JobRecord{ID: "broken", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	w.runSchedulerSweeps(context.Background(), "unused", "")
	if got = w.SchedulerObservation(time.Now()); got.Health != "ok" || got.LastError != nil {
		t.Fatal("recovery did not clear live alert", got)
	}
}

func TestQueueCorruptRecordIsObservableWithoutBlockingHealthyJob(t *testing.T) {
	dir := t.TempDir()
	w := NewWorker(&WorkerConfig{StateDir: dir, MaxParallelJobs: 1})
	spawn := newStubSpawn()
	spawn.install(w)
	broken := filepath.Join(dir, "broken")
	os.MkdirAll(broken, 0755)
	os.WriteFile(filepath.Join(broken, "job.json"), []byte("{broken"), 0600)
	if err := SaveJobAtomic(filepath.Join(dir, "healthy", "job.json"), &JobRecord{ID: "healthy", Status: "queued", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	started, err := w.ScheduleQueued(context.Background(), "unused", "")
	if err == nil || started != 1 || spawn.n() != 1 {
		t.Fatalf("corrupt record hid failure or blocked independent job: started=%d spawn=%d error=%v", started, spawn.n(), err)
	}
	healthy, err := LoadJob(filepath.Join(dir, "healthy", "job.json"))
	if err != nil || healthy.PID <= 0 {
		t.Fatal(healthy, err)
	}
}
