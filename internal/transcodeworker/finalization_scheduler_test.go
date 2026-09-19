package transcodeworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/smbdirect"
)

type recoveringDirectStore struct {
	*fakeDirectStore
	failure  error
	failures int32
	calls    atomic.Int32
}

func (s *recoveringDirectStore) Publish(ctx context.Context, local, destination, id string) error {
	if s.calls.Add(1) <= s.failures {
		return s.failure
	}
	return s.fakeDirectStore.Publish(ctx, local, destination, id)
}

func schedulerPublicationFailure(t *testing.T, failure error, failures int32) (*Worker, *recoveringDirectStore, string, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-media")
	id := "job-auto-publication"
	work := filepath.Join(dir, "work", id)
	candidate := filepath.Join(root, "TV", "candidate.mkv")
	store := &recoveringDirectStore{
		fakeDirectStore: &fakeDirectStore{root: root, source: []byte("SMB source")},
		failure:         failure, failures: failures,
	}
	w := NewWorker(&WorkerConfig{
		StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root},
		LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAuto, MaxParallelJobs: 1,
	})
	w.mediaStore = store
	w.SetAliveFunc(func(job *JobRecord) bool { return job.PID != 0 })
	w.SetProbeSource(func(context.Context, string) ([]SourceStream, float64, error) {
		return []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc"}}, 60, nil
	})
	encodes := &atomic.Int32{}
	w.SetRunFFmpeg(func(_ context.Context, _ *ExecutionPlan, _ *JobRecord, _, output, _, _ string) error {
		encodes.Add(1)
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			return err
		}
		return os.WriteFile(output, []byte("encoded candidate"), 0o600)
	})
	job := &JobRecord{
		ID: id, Status: "running", Source: filepath.Join(root, "TV", "source.mkv"), Candidate: candidate,
		Profile: "hevc-vt", Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
		StagingPolicy: string(StagingPolicyAuto), StagingState: string(StagingStatePending),
		FinalizationState: string(FinalizationStatePending), StagedInputPath: filepath.Join(work, "input.mkv"),
		EffectiveInputPath: filepath.Join(work, "input.mkv"), LocalCandidatePath: filepath.Join(work, "candidate.mkv"),
		IntendedDestination: candidate, PartialPath: PartialPathFor(candidate, id),
	}
	pr6b2Seed(t, w.cfg.StateDir, job)
	if err := w.InternalRun(context.Background(), id); err == nil {
		t.Fatal("initial publication must fail after successful encoding")
	}
	if encodes.Load() != 1 {
		t.Fatalf("initial encode count=%d want=1", encodes.Load())
	}
	return w, store, id, encodes
}

func makeFinalizationDue(t *testing.T, w *Worker, id string) {
	t.Helper()
	capLock, err := acquireCapacityLock(filepath.Clean(w.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer capLock.Unlock()
	jobDir := filepath.Join(w.cfg.StateDir, id)
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	defer jobLock.Unlock()
	job := pr6b1LoadJob(t, w.cfg.StateDir, id)
	job.NextFinalizationAt = time.Now().UTC().Add(-time.Second)
	if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func installScheduledFinalizer(w *Worker) <-chan error {
	finished := make(chan error, 10)
	w.SetTranscodeSpawner(func(_, _, jobID string) (int, string, error) {
		go func() { finished <- w.InternalRun(context.Background(), jobID) }()
		return 1 << 30, "test-finalizer", nil
	})
	return finished
}

func TestSchedulerPublishesTransientFailureWithoutAnotherEncode(t *testing.T) {
	failure := &smbdirect.Error{Class: smbdirect.SMBTransportError, Op: "publish", Err: errors.New("connection reset")}
	w, store, id, encodes := schedulerPublicationFailure(t, failure, 1)
	failed := pr6b1LoadJob(t, w.cfg.StateDir, id)
	if !failed.EncodeComplete || failed.PID != 0 || failed.NextFinalizationAt.IsZero() {
		t.Fatalf("missing durable publication recovery: %+v", failed)
	}
	status, err := w.Status(context.Background(), id)
	if err != nil || status.RecoveryRequired || status.ErrorClass != smbdirect.SMBTransportError {
		t.Fatalf("transient publication should expose automatic recovery: %+v, err=%v", status, err)
	}
	finished := installScheduledFinalizer(w)
	stop := w.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
	defer stop()
	select {
	case err := <-finished:
		t.Fatalf("recovery ignored future next_finalization_at: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	makeFinalizationDue(t, w, id)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not resume publication without a client request")
	}
	completed := pr6b1LoadJob(t, w.cfg.StateDir, id)
	if completed.Status != "completed" || completed.FinalizationRetryCount != 1 || encodes.Load() != 1 || store.calls.Load() != 2 {
		t.Fatalf("job=%+v encodes=%d publications=%d", completed, encodes.Load(), store.calls.Load())
	}
}

func TestSchedulerDoesNotRetryPermanentPublicationFailures(t *testing.T) {
	cases := map[string]error{
		"auth":       &smbdirect.Error{Class: smbdirect.SMBAuthFailed, Op: "publish", Err: errors.New("invalid credentials")},
		"permission": &smbdirect.Error{Class: smbdirect.StoragePermissionDenied, Op: "publish", Err: os.ErrPermission},
		"conflict":   &smbdirect.Error{Class: smbdirect.StorageIOError, Op: "publish", Err: smbdirect.ErrDestinationExists},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			w, store, id, encodes := schedulerPublicationFailure(t, failure, 100)
			failed := pr6b1LoadJob(t, w.cfg.StateDir, id)
			if !failed.NextFinalizationAt.IsZero() || !failed.EncodeComplete || failed.Status != "running" {
				t.Fatalf("permanent failure must remain resumable without automatic retry: %+v", failed)
			}
			status, err := w.Status(context.Background(), id)
			if err != nil || !status.RecoveryRequired || status.ErrorClass != failed.FailureClassification || status.Phase != "publication_pending" {
				t.Fatalf("permanent publication should request intervention: %+v, err=%v", status, err)
			}
			finished := installScheduledFinalizer(w)
			stop := w.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
			defer stop()
			select {
			case err := <-finished:
				t.Fatalf("permanent failure unexpectedly respawned: %v", err)
			case <-time.After(40 * time.Millisecond):
			}
			if store.calls.Load() != 1 || encodes.Load() != 1 {
				t.Fatalf("publications=%d encodes=%d", store.calls.Load(), encodes.Load())
			}
		})
	}
}

func TestSchedulerPersistsPublicationRetryLimit(t *testing.T) {
	failure := &smbdirect.Error{Class: smbdirect.SMBSessionInvalid, Op: "publish", Err: errors.New("session expired")}
	w, store, id, encodes := schedulerPublicationFailure(t, failure, 100)
	finished := installScheduledFinalizer(w)
	stop := w.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
	defer stop()
	for attempt := 1; attempt <= MaxFinalizationRetries; attempt++ {
		makeFinalizationDue(t, w, id)
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("persistent publication failure unexpectedly succeeded")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("scheduled retry never ran")
		}
		job := pr6b1LoadJob(t, w.cfg.StateDir, id)
		if job.FinalizationRetryCount != attempt {
			t.Fatalf("durable retries=%d want=%d", job.FinalizationRetryCount, attempt)
		}
		if attempt < MaxFinalizationRetries && !job.NextFinalizationAt.After(time.Now()) {
			t.Fatal("retry backoff was not persisted")
		}
	}
	// A new Worker represents a daemon restart; the same persisted budget must
	// still prevent a fourth automatic resume.
	stop()
	restarted := NewWorker(w.cfg)
	restarted.mediaStore = store
	restarted.SetAliveFunc(func(job *JobRecord) bool { return job.PID != 0 })
	restartFinished := installScheduledFinalizer(restarted)
	restartStop := restarted.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
	defer restartStop()
	select {
	case err := <-restartFinished:
		t.Fatalf("retry limit reset after daemon restart: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	job := pr6b1LoadJob(t, w.cfg.StateDir, id)
	if !job.NextFinalizationAt.IsZero() || store.calls.Load() != 1+MaxFinalizationRetries || encodes.Load() != 1 {
		t.Fatalf("job=%+v publications=%d encodes=%d", job, store.calls.Load(), encodes.Load())
	}
	status, err := restarted.Status(context.Background(), id)
	if err != nil || !status.RecoveryRequired || status.ErrorClass != smbdirect.SMBSessionInvalid || status.Phase != "publication_pending" {
		t.Fatalf("exhausted publication retries should request intervention: %+v, err=%v", status, err)
	}
}
