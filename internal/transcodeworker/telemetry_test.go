package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/smbdirect"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestJobHeartbeatPersistsProgressWithoutStatusPollAndStopsAtTerminal(t *testing.T) {
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir()})
	jobDir := filepath.Join(w.cfg.StateDir, "heartbeat")
	jobFile := filepath.Join(jobDir, "job.json")
	job := &JobRecord{ID: "heartbeat", Status: "running", PID: os.Getpid(), DurationSec: 100}
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(jobDir, "progress.txt")
	if err := os.WriteFile(progressPath, []byte("fps=366.1\nspeed=15.5x\nout_time_us=26200000\nprogress=continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := w.startJobHeartbeat(context.Background(), jobDir, jobFile, 5*time.Millisecond)
	defer stop()
	if err := os.WriteFile(progressPath, []byte("fps=366.1\nspeed=15.5x\nout_time_us=52400000\nprogress=continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err := LoadJob(jobFile)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastKnownProgress != nil && got.LastKnownProgress.Progress == 52.4 && !got.WorkerHeartbeatAt.IsZero() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runner heartbeat did not persist an encoder update independently")
		case <-ticker.C:
		}
	}
	stop()
	job, _ = LoadJob(jobFile)
	job.Status = "completed"
	job.FinishedAt = time.Now().UTC()
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(jobFile)
	if err := w.persistJobHeartbeat(jobDir, jobFile, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(jobFile)
	if string(before) != string(after) {
		t.Fatal("heartbeat rewrote a terminal record")
	}
	if err := os.Remove(progressPath); err != nil {
		t.Fatal(err)
	}
	job.Status = "running"
	m := observeJobProgress(jobDir, job, job.LastProgressAt.Add(time.Minute))
	if !job.ProgressIsStale || m.Progress != 52.4 || m.Speed != 15.5 {
		t.Fatalf("missing progress file lost the last known measurement: %+v %+v", job.JobTelemetry, m)
	}
}

func TestProgressIgnoresPartiallyWrittenRecord(t *testing.T) {
	name := filepath.Join(t.TempDir(), "progress.txt")
	data := "fps=300\nspeed=12x\nout_time_us=5000000\nprogress=continue\nfps=0\nspeed=0\nout_time_us=0\nprogress=con"
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	m := ParseProgress(name, 10)
	if !m.Valid || m.Progress != 50 || m.FPS != 300 || m.Speed != 12 {
		t.Fatalf("partial record replaced the preceding measurement: %+v", m)
	}
}

func TestStatusDoesNotOverwriteConcurrentCompletion(t *testing.T) {
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir()})
	jobDir := filepath.Join(w.cfg.StateDir, "completed-race")
	jobFile := filepath.Join(jobDir, "job.json")
	job := &JobRecord{ID: "completed-race", Status: "running", PID: 12345, CreatedAt: time.Now().Add(-time.Minute)}
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	w.SetAliveFunc(func(*JobRecord) bool {
		job.Status = "completed"
		job.FinishedAt = time.Now()
		if err := SaveJobAtomic(jobFile, job); err != nil {
			t.Fatal(err)
		}
		return false
	})
	st, err := w.Status(context.Background(), job.ID)
	if err != nil || st.Status != "completed" || st.Phase != "completed" || st.Progress != 100 {
		t.Fatalf("completed runner was changed back to failed/running: %+v %v", st, err)
	}
}

func TestCapacitySweepDoesNotOverwriteConcurrentCompletion(t *testing.T) {
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir()})
	jobDir := filepath.Join(w.cfg.StateDir, "capacity-race")
	jobFile := filepath.Join(jobDir, "job.json")
	job := &JobRecord{ID: "capacity-race", Status: "running", PID: 12345, CreatedAt: time.Now().Add(-time.Minute)}
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	w.SetAliveFunc(func(*JobRecord) bool {
		job.Status = "completed"
		job.FinishedAt = time.Now()
		if err := SaveJobAtomic(jobFile, job); err != nil {
			t.Fatal(err)
		}
		return false
	})
	n, err := w.countActiveJobs("")
	got, loadErr := LoadJob(jobFile)
	if err != nil || loadErr != nil || n != 0 || got.Status != "completed" {
		t.Fatalf("capacity sweep overwrote completed job: count=%d job=%+v errors=%v/%v", n, got, err, loadErr)
	}
}

func TestStatusSeparatesEncodingTimeFromWallTimeAndQueueSlots(t *testing.T) {
	w := NewWorker(&WorkerConfig{StateDir: t.TempDir(), MaxParallelJobs: 3})
	now := time.Now().UTC()
	job := &JobRecord{ID: "done", Status: "completed", CreatedAt: now.Add(-7 * time.Hour), StartedAt: now.Add(-7*time.Hour + time.Minute), FinishedAt: now}
	job.EncodeStartedAt, job.EncodeFinishedAt = now.Add(-6*time.Minute), now.Add(-time.Minute)
	job.ValidationStartedAt, job.ValidationFinishedAt = now.Add(-time.Minute), now.Add(-50*time.Second)
	pr6b2Seed(t, w.cfg.StateDir, job)
	for _, j := range []*JobRecord{
		{ID: "preparing", Status: "running", PID: 12345},
		{ID: "queued-a", Status: "queued", CreatedAt: now.Add(-time.Second)},
		{ID: "queued-b", Status: "queued", CreatedAt: now},
	} {
		pr6b2Seed(t, w.cfg.StateDir, j)
	}
	st, err := w.Status(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.EncodeDurationMs == nil || *st.EncodeDurationMs != 300000 || st.WallDurationMs == nil || *st.WallDurationMs != 25200000 || st.QueueDurationMs == nil || *st.QueueDurationMs != 60000 || st.ValidationDurationMs == nil || *st.ValidationDurationMs != 10000 {
		t.Fatalf("worker durations were mixed: %+v", st.JobTelemetry)
	}
	queued, err := w.Status(context.Background(), "queued-b")
	if err != nil || queued.Phase != "queued" || queued.QueuePosition != 2 || queued.WorkerSlotsTotal != 3 || queued.WorkerSlotsUsed != 1 {
		t.Fatalf("queue/slot snapshot = %+v %v", queued, err)
	}
}

type downloadFailStore struct {
	directMediaStore
	err error
}

func (s downloadFailStore) DownloadAtomic(context.Context, string, string) error { return s.err }

func TestDirectSMBFailureOccursBeforeFFmpegAndNeverFallsBackToLocalMount(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.SMBDirect = smbdirect.Config{Enabled: true, LocalRoot: root, Share: "media"}
		// Direct SMB remains staged even if legacy mount staging was disabled.
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	w.mediaStore = downloadFailStore{
		directMediaStore: &fakeDirectStore{root: root, source: []byte("source")},
		err:              &smbdirect.Error{Class: smbdirect.SMBSigningRequired, Op: "read preflight", Err: errors.New("signing required")},
	}
	newStubSpawn().install(w)
	recorder := &pr6b2Recorder{createOut: true}
	recorder.install(w)
	source := filepath.Join(root, "TV", "source.mkv")
	candidate := filepath.Join(root, "TV", "candidate.mkv")
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: "smb-failure", SourcePath: source, CandidatePath: candidate}, "test-exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), "smb-failure"); err == nil {
		t.Fatal("SMB error was not reported")
	}
	job := pr6b1LoadJob(t, cfg.StateDir, "smb-failure")
	if recorder.encodeCalls != 0 || len(recorder.probePaths) != 0 || job.FailureClassification != "smb_signing_required" || job.StorageBackend != "smb_direct" || job.RetryCount != 0 {
		t.Fatalf("storage failure was misclassified or consumed an encode retry: %+v encodes=%d probes=%v", job, recorder.encodeCalls, recorder.probePaths)
	}
	if job.StagedInputPath == "" || job.LocalCandidatePath == candidate {
		t.Fatal("SMB direct accepted the legacy mount layout")
	}
	// A readable local file outside the SMB mapping must still be rejected.
	local := pr6b1WriteFile(t, filepath.Join(dir, "legacy-mount", "source.mkv"))
	if _, err := w.statMedia(context.Background(), local); err == nil || !strings.Contains(err.Error(), "storage_backend_mismatch") {
		t.Fatalf("configured SMB silently read a local mount: %v", err)
	}
}

func TestOperationalEncodePersistsPhaseAndCompletionTimestamps(t *testing.T) {
	dir := t.TempDir()
	w := NewWorker(pr6b2Config(dir, nil))
	src := pr6b1WriteFile(t, filepath.Join(dir, "source.mkv"))
	job := &JobRecord{ID: "phase-times", Status: "queued", Source: src, Candidate: filepath.Join(dir, "candidate.mkv"), Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().Add(-time.Minute)}
	pr6b2Seed(t, w.cfg.StateDir, job)
	w.SetProbeSource(func(context.Context, string) ([]SourceStream, float64, error) {
		return []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc"}}, 120, nil
	})
	w.SetRunFFmpeg(func(_ context.Context, _ *ExecutionPlan, j *JobRecord, _, out, _, _ string) error {
		got := pr6b1LoadJob(t, w.cfg.StateDir, j.ID)
		if got.Phase != "encoding" || got.EncodeStartedAt.IsZero() || got.WorkerHeartbeatAt.IsZero() {
			t.Fatalf("encode started without durable phase/start time: %+v", got)
		}
		return os.WriteFile(out, []byte("candidate"), 0o600)
	})
	if err := w.InternalRun(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	got := pr6b1LoadJob(t, w.cfg.StateDir, job.ID)
	if got.Phase != "completed" || got.EncodeFinishedAt.IsZero() || got.ValidationStartedAt.IsZero() || got.ValidationFinishedAt.IsZero() || got.EncodeDurationMs == nil || got.QueueDurationMs == nil || *got.QueueDurationMs < 59000 {
		t.Fatalf("completion lost timing evidence: %+v", got)
	}
	if got.EncodeFinishedAt.Before(got.EncodeStartedAt) || got.FinishedAt.Before(got.ValidationFinishedAt) {
		t.Fatal("worker phase timestamps are out of order")
	}
	// Public snapshots remain JSON-compatible with the shared client type.
	status, err := w.Status(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(status)
	var remote transcode.JobStatus
	if err := json.Unmarshal(b, &remote); err != nil || remote.Phase != "completed" || remote.EncodeDurationMs == nil || remote.FinishedAt.IsZero() {
		t.Fatalf("HTTP snapshot lost timing metadata: %+v %v", remote, err)
	}
}
