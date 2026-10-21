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

func TestFinalSizeRejectionPreservesSourceBeforePublication(t *testing.T) {
	for _, candidateBytes := range []int{85, 95, 110} {
		t.Run(fmt.Sprint(candidateBytes), func(t *testing.T) {
			dir := t.TempDir()
			source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), string(make([]byte, 100)))
			candidate := filepath.Join(dir, "external", "candidate.mkv")
			cfg := pr6b2Config(dir, func(cfg *WorkerConfig) { cfg.ExternalRoots = []string{filepath.Join(dir, "external")} })
			w := NewWorker(cfg)
			newStubSpawn().install(w)
			rec := &pr6b2Recorder{createOut: true}
			rec.install(w)
			w.SetRunFFmpeg(func(ctx context.Context, plan *ExecutionPlan, job *JobRecord, input, output, progress, log string) error {
				if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
					return err
				}
				return os.WriteFile(output, make([]byte, candidateBytes), 0600)
			})
			plan := ioOptPlan(t)
			plan.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: 15}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			published := 0
			w.SetFinalizeOutput(func(ctx context.Context, local, destination, id string) error {
				published++
				return FinalizeOutputAtomic(ctx, local, destination, id)
			})
			if _, err := w.Submit(context.Background(), SubmitRequest{ID: "guarded", SourcePath: source, CandidatePath: candidate, Plan: plan}, "unused", ""); err != nil {
				t.Fatal(err)
			}
			err := pr6b2Run(t, w, "guarded")
			job := pr6b1LoadJob(t, cfg.StateDir, "guarded")
			if candidateBytes > 85 {
				if err == nil || published != 0 || job.ReasonCode != "insufficient_savings" || job.FailureClassification != "policy_rejected" {
					t.Fatalf("invalid candidate was not rejected: %v published=%d job=%+v", err, published, job)
				}
			} else {
				if err != nil || published != 1 || job.Status != "completed" {
					t.Fatalf("exact 15%% boundary: %v job=%+v", err, job)
				}
			}
			info, err := os.Stat(source)
			if err != nil || info.Size() != 100 {
				t.Fatal("original changed", err)
			}
			for _, phase := range []string{"staging", "source_hash", "probe"} {
				if job.PhaseCosts[phase].Attempts != 1 {
					t.Fatalf("missing %s timing: %+v", phase, job.PhaseCosts)
				}
			}
			if candidateBytes == 85 {
				// The destination sits under an external (mounted NAS) root, so
				// the publication reports the M bytes it wrote there and no
				// read-back; a same-filesystem rename commits it.
				pub := job.PhaseCosts["publication"]
				if pub.NASWrittenBytes == nil || *pub.NASWrittenBytes != 85 || pub.NASReadBytes == nil || *pub.NASReadBytes != 0 || job.PhaseCosts["cleanup"].Attempts != 1 {
					t.Fatal("mounted publication bytes wrong or cleanup lost", job.PhaseCosts)
				}
				if _, fallback := job.PhaseCosts["publication_copy_fallback"]; fallback {
					t.Fatal("atomic rename recorded as a copy fallback", job.PhaseCosts)
				}
			}
		})
	}
}

type blockedObservedStore struct {
	ioOptCountingStore
	started chan struct{}
	release chan struct{}
}

func (s *blockedObservedStore) DownloadAtomic(ctx context.Context, source, local string) error {
	close(s.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	return s.ioOptCountingStore.DownloadAtomic(ctx, source, local)
}
func TestSlowSyntheticIOLeavesIndependentSlotAndSchedulerObservable(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "fake-nas")
	store := &blockedObservedStore{ioOptCountingStore: ioOptCountingStore{root: root, content: make([]byte, 1024)}, started: make(chan struct{}), release: make(chan struct{})}
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 2})
	w.mediaStore = store
	spawn := newStubSpawn()
	spawn.install(w)
	slow := &JobRecord{ID: "slow", Status: "running", PID: spawn.occupy("slow"), Source: filepath.Join(root, "source.mkv"), Candidate: filepath.Join(root, "candidate.mkv"), Plan: ioOptPlan(t), CreatedAt: time.Now().UTC(), StagingState: string(StagingStatePending), StagedInputPath: filepath.Join(dir, "work", "slow", "input.mkv"), EffectiveInputPath: filepath.Join(dir, "work", "slow", "input.mkv")}
	slowDir := filepath.Join(w.cfg.StateDir, slow.ID)
	slowFile := filepath.Join(slowDir, "job.json")
	if err := SaveJobAtomic(slowFile, slow); err != nil {
		t.Fatal(err)
	}
	fast := &JobRecord{ID: "independent", Status: "queued", Source: filepath.Join(root, "independent.mkv"), Candidate: filepath.Join(root, "independent-candidate.mkv"), Plan: ioOptPlan(t), CreatedAt: time.Now().UTC()}
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, fast.ID, "job.json"), fast); err != nil {
		t.Fatal(err)
	}
	operational, err := resolveOperationalForExecution(slow)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := w.ensureStaged(ctx, slowDir, slowFile, slow, operational); finished <- err }()
	<-store.started
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	start := time.Now()
	w.runSchedulerSweeps(ctx, "unused", "")
	reconcileWall := time.Since(start)
	observation := w.SchedulerObservation(time.Now())
	total, used, _ := w.slotSnapshot("")
	if observation.Health != "ok" || observation.Sweeps["queue"].StartedJobs != 1 || total != 2 || used != 2 {
		t.Fatalf("blocked I/O prevented independent admission: %+v slots=%d/%d", observation, used, total)
	}
	pending, err := LoadJob(slowFile)
	if err != nil || pending.Phase != "reading_source" {
		t.Fatal("staging progress not independently observable", pending, err)
	}
	select {
	case err := <-finished:
		t.Fatal("blocked source escaped before release", err)
	default:
	}
	close(store.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	t.Logf("controlled slow I/O: blocked_transfer_bytes=1024 scheduler_wall=%s slots=%d/%d independently_admitted=1 phase=reading_source injected_wait_until_release=true", reconcileWall, used, total)
}
