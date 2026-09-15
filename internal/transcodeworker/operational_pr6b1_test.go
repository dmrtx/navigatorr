package transcodeworker

// Phase 6B1 tests: operational storage config, restart-safe job metadata, and
// reconciliation awareness. They assert only metadata and reconciliation
// behaviour: no staging/finalization is wired into InternalRun/RunFFmpeg here,
// and no file is copied at Submit.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func pr6b1WriteFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("dummy-media"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pr6b1LoadJob(t *testing.T, stateDir, id string) *JobRecord {
	t.Helper()
	job, err := LoadJob(filepath.Join(stateDir, id, "job.json"))
	if err != nil {
		t.Fatalf("LoadJob(%s): %v", id, err)
	}
	return job
}

func pr6b1Config(tempDir string, mutate func(*WorkerConfig)) *WorkerConfig {
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(tempDir, "jobs"),
		AllowedRoots:    []string{tempDir},
		MaxParallelJobs: 1,
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func TestPR6B1_LoadWorkerConfigDefaults(t *testing.T) {
	cfg, err := LoadWorkerConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadWorkerConfig: %v", err)
	}
	if cfg.StagingPolicy != StagingPolicyAuto {
		t.Fatalf("staging_policy default = %q, want auto", cfg.StagingPolicy)
	}
	if !filepath.IsAbs(cfg.LocalWorkDir) {
		t.Fatalf("local_work_dir default must be absolute, got %q", cfg.LocalWorkDir)
	}
	wantWork := filepath.Join(filepath.Clean(cfg.StateDir), "_work")
	if cfg.LocalWorkDir != wantWork {
		t.Fatalf("local_work_dir default = %q, want %q", cfg.LocalWorkDir, wantWork)
	}
	if len(cfg.ExternalRoots) != len(cfg.AllowedRoots) {
		t.Fatalf("external_roots default len = %d, want %d", len(cfg.ExternalRoots), len(cfg.AllowedRoots))
	}
	for i := range cfg.AllowedRoots {
		if cfg.ExternalRoots[i] != cfg.AllowedRoots[i] {
			t.Fatalf("external_roots[%d] = %q, want copy of allowed_roots %q", i, cfg.ExternalRoots[i], cfg.AllowedRoots[i])
		}
	}

	// Non-alias: external_roots must be a copy, never the same backing array.
	if len(cfg.ExternalRoots) > 0 {
		before := cfg.AllowedRoots[0]
		cfg.ExternalRoots[0] = "/mutated/external"
		if cfg.AllowedRoots[0] != before {
			t.Fatalf("external_roots aliases allowed_roots: mutating one changed the other (%q -> %q)", before, cfg.AllowedRoots[0])
		}
		cfg2, err := LoadWorkerConfig(filepath.Join(t.TempDir(), "absent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		cfg2.AllowedRoots[0] = "/mutated/allowed"
		if cfg2.ExternalRoots[0] == "/mutated/allowed" {
			t.Fatalf("allowed_roots aliases external_roots")
		}
	}
}

func TestPR6B1_LoadWorkerConfigYAMLOverrides(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	rootA := filepath.Join(dir, "rootA")
	extA := filepath.Join(dir, "extA")
	workA := filepath.Join(dir, "workA")
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := fmt.Sprintf(`state_dir: %q
allowed_roots:
  - %q
external_roots:
  - %q
local_work_dir: %q
staging_policy: always
`, stateDir, rootA, extA, workA)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWorkerConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadWorkerConfig: %v", err)
	}
	if cfg.StateDir != filepath.Clean(stateDir) {
		t.Fatalf("state_dir = %q, want %q", cfg.StateDir, stateDir)
	}
	if cfg.StagingPolicy != StagingPolicyAlways {
		t.Fatalf("staging_policy = %q, want always", cfg.StagingPolicy)
	}
	if len(cfg.AllowedRoots) != 1 || cfg.AllowedRoots[0] != filepath.Clean(rootA) {
		t.Fatalf("allowed_roots = %v, want [%q]", cfg.AllowedRoots, rootA)
	}
	if len(cfg.ExternalRoots) != 1 || cfg.ExternalRoots[0] != filepath.Clean(extA) {
		t.Fatalf("external_roots = %v, want [%q]", cfg.ExternalRoots, extA)
	}
	if cfg.LocalWorkDir != filepath.Clean(workA) {
		t.Fatalf("local_work_dir = %q, want %q", cfg.LocalWorkDir, workA)
	}
}

func TestPR6B1_LoadWorkerConfigInvalidStagingPolicyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("staging_policy: sometimes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWorkerConfig(cfgPath)
	if err == nil {
		t.Fatal("invalid staging_policy must be rejected on config load")
	}
	if !errors.Is(err, ErrInvalidStagingPolicy) {
		t.Fatalf("error = %v, want ErrInvalidStagingPolicy", err)
	}

	// Blank policy normalizes to the operational default rather than failing.
	blankPath := filepath.Join(dir, "blank.yaml")
	if err := os.WriteFile(blankPath, []byte("state_dir: /tmp/x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWorkerConfig(blankPath)
	if err != nil {
		t.Fatalf("blank staging_policy must default, got %v", err)
	}
	if cfg.StagingPolicy != StagingPolicyAuto {
		t.Fatalf("blank staging_policy = %q, want auto", cfg.StagingPolicy)
	}
}

func TestPR6B1_SubmitLocalToLocalNoOperationalWork(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cand := filepath.Join(tempDir, "out.mkv")
	cfg := pr6b1Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-local"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	job := pr6b1LoadJob(t, cfg.StateDir, id)

	if job.Source != filepath.Clean(src) || job.Candidate != filepath.Clean(cand) {
		t.Fatalf("semantic Source/Candidate changed: %+v", job)
	}
	if job.StagingState != string(StagingStateNotRequired) {
		t.Fatalf("StagingState = %q, want not_required", job.StagingState)
	}
	if job.EffectiveInputPath != filepath.Clean(src) {
		t.Fatalf("EffectiveInputPath = %q, want source", job.EffectiveInputPath)
	}
	if job.StagedInputPath != "" {
		t.Fatalf("StagedInputPath = %q, want blank", job.StagedInputPath)
	}
	if job.LocalCandidatePath != filepath.Clean(cand) {
		t.Fatalf("LocalCandidatePath = %q, want candidate", job.LocalCandidatePath)
	}
	if job.IntendedDestination != filepath.Clean(cand) {
		t.Fatalf("IntendedDestination = %q, want candidate", job.IntendedDestination)
	}
	if job.FinalizationState != string(FinalizationStateNotRequired) {
		t.Fatalf("FinalizationState = %q, want not_required", job.FinalizationState)
	}
	if job.PartialPath != "" {
		t.Fatalf("PartialPath = %q, want blank", job.PartialPath)
	}
}

func TestPR6B1_SubmitExternalSourceAutoStagesOperationally(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(extRoot, "movie.mkv"))
	cand := filepath.Join(tempDir, "local", "out.mkv")
	cfg := pr6b1Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyAuto
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-staged"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	job := pr6b1LoadJob(t, cfg.StateDir, id)

	if job.Source != filepath.Clean(src) {
		t.Fatalf("semantic Source changed: %q", job.Source)
	}
	wantStaged := filepath.Join(cfg.StateDir, "_work", id, "input.mkv")
	if job.StagingState != string(StagingStatePending) {
		t.Fatalf("StagingState = %q, want pending", job.StagingState)
	}
	if job.StagedInputPath != wantStaged || job.EffectiveInputPath != wantStaged {
		t.Fatalf("staged/effective = %q/%q, want %q", job.StagedInputPath, job.EffectiveInputPath, wantStaged)
	}
	if _, err := os.Stat(wantStaged); !os.IsNotExist(err) {
		t.Fatalf("Submit must not copy the staged input (Phase 6B1); %s exists", wantStaged)
	}
	if job.LocalCandidatePath != filepath.Clean(cand) || job.FinalizationState != string(FinalizationStateNotRequired) {
		t.Fatalf("local destination should not finalize: %+v", job)
	}
}

func TestPR6B1_SubmitStagingNeverOnExternalSource(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(extRoot, "movie.mkv"))
	cand := filepath.Join(tempDir, "local", "out.mkv")
	cfg := pr6b1Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-never"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	job := pr6b1LoadJob(t, cfg.StateDir, id)
	if job.StagingState != string(StagingStateNotRequired) {
		t.Fatalf("StagingState = %q, want not_required", job.StagingState)
	}
	if job.EffectiveInputPath != filepath.Clean(src) || job.StagedInputPath != "" {
		t.Fatalf("effective/staged = %q/%q, want original source and blank staging", job.EffectiveInputPath, job.StagedInputPath)
	}
}

func TestPR6B1_SubmitStagingAlwaysOnLocalSource(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(tempDir, "local", "out.mkv")
	cfg := pr6b1Config(tempDir, func(c *WorkerConfig) {
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-always"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	job := pr6b1LoadJob(t, cfg.StateDir, id)
	wantStaged := filepath.Join(cfg.StateDir, "_work", id, "input.mkv")
	if job.StagingState != string(StagingStatePending) {
		t.Fatalf("StagingState = %q, want pending", job.StagingState)
	}
	if job.EffectiveInputPath != wantStaged || job.StagedInputPath != wantStaged {
		t.Fatalf("effective/staged = %q/%q, want %q", job.EffectiveInputPath, job.StagedInputPath, wantStaged)
	}
	if job.Source != filepath.Clean(src) {
		t.Fatalf("semantic Source changed: %q", job.Source)
	}
}

func TestPR6B1_SubmitExternalDestinationFinalizesLocallyFirst(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "newsub", "out.mkv")
	cfg := pr6b1Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-external-dest"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	job := pr6b1LoadJob(t, cfg.StateDir, id)

	if job.Candidate != filepath.Clean(cand) {
		t.Fatalf("semantic Candidate = %q, want intended destination %q", job.Candidate, filepath.Clean(cand))
	}
	if job.IntendedDestination != filepath.Clean(cand) {
		t.Fatalf("IntendedDestination = %q, want %q", job.IntendedDestination, filepath.Clean(cand))
	}
	wantLocal := filepath.Join(cfg.StateDir, "_work", id, "candidate.mkv")
	if job.LocalCandidatePath != wantLocal {
		t.Fatalf("LocalCandidatePath = %q, want %q", job.LocalCandidatePath, wantLocal)
	}
	if job.FinalizationState != string(FinalizationStatePending) {
		t.Fatalf("FinalizationState = %q, want pending", job.FinalizationState)
	}
	wantPartial := filepath.Clean(cand) + ".partial." + id
	if job.PartialPath != wantPartial {
		t.Fatalf("PartialPath = %q, want %q", job.PartialPath, wantPartial)
	}

	// The execution spec digest must still be over the intended destination,
	// never the local candidate path.
	wantDigest, err := transcode.DigestTranscodeExecutionSpec(job.Source, job.Candidate, job.Profile, job.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if job.ExecutionSpecDigest != wantDigest {
		t.Fatalf("digest = %q, want %q (intended destination)", job.ExecutionSpecDigest, wantDigest)
	}

	// Local-output-first: submission must not create the external destination
	// directory it has not finished writing yet.
	if _, err := os.Stat(filepath.Join(extRoot, "newsub")); !os.IsNotExist(err) {
		t.Fatalf("external destination directory must not be created at Submit")
	}
}

func TestPR6B1_SubmitLocalDestinationCreatesCandidateDir(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cand := filepath.Join(tempDir, "newsub", "out.mkv")
	cfg := pr6b1Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-local-dest"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	job := pr6b1LoadJob(t, cfg.StateDir, id)
	if job.LocalCandidatePath != filepath.Clean(cand) {
		t.Fatalf("LocalCandidatePath = %q, want %q", job.LocalCandidatePath, filepath.Clean(cand))
	}
	if job.FinalizationState != string(FinalizationStateNotRequired) {
		t.Fatalf("FinalizationState = %q, want not_required", job.FinalizationState)
	}
	if _, err := os.Stat(filepath.Dir(cand)); err != nil {
		t.Fatalf("local candidate directory must still be pre-created: %v", err)
	}
}

func TestPR6B1_LegacyJobJSONWithoutOperationalFieldsLoads(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "job-legacy")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"id":"job-legacy","status":"running","source":%q,"candidate":%q}`,
		filepath.Join(dir, "src.mkv"), filepath.Join(dir, "out.mkv"))
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	job, err := LoadJob(filepath.Join(jobDir, "job.json"))
	if err != nil {
		t.Fatalf("legacy job.json must load: %v", err)
	}
	if job.StagingPolicy != "" || job.StagingState != "" || job.EffectiveInputPath != "" ||
		job.LocalCandidatePath != "" || job.IntendedDestination != "" ||
		job.FinalizationState != "" || job.PartialPath != "" || job.EncodeComplete {
		t.Fatalf("operational fields must stay blank on legacy records: %+v", job)
	}
	if job.EffectiveInput() != job.Source || job.LocalCandidate() != job.Candidate || job.Destination() != job.Candidate {
		t.Fatalf("legacy helpers must fall back to semantic source/candidate: %+v", job)
	}
	if err := ValidateStagingState(job.StagingState); err != nil {
		t.Fatalf("blank staging state must be accepted as legacy: %v", err)
	}
	if err := ValidateFinalizationState(job.FinalizationState); err != nil {
		t.Fatalf("blank finalization state must be accepted as legacy: %v", err)
	}
}

func TestPR6B1_ValidateOperationalStates(t *testing.T) {
	for _, ok := range []string{"", "not_required", "pending", "staging", "ready"} {
		if err := ValidateStagingState(ok); err != nil {
			t.Fatalf("ValidateStagingState(%q) = %v, want nil", ok, err)
		}
	}
	if err := ValidateStagingState("bogus"); !errors.Is(err, ErrInvalidStagingState) {
		t.Fatalf("ValidateStagingState(bogus) = %v, want ErrInvalidStagingState", err)
	}
	for _, ok := range []string{"", "not_required", "pending", "finalizing", "completed"} {
		if err := ValidateFinalizationState(ok); err != nil {
			t.Fatalf("ValidateFinalizationState(%q) = %v, want nil", ok, err)
		}
	}
	if err := ValidateFinalizationState("bogus"); !errors.Is(err, ErrInvalidFinalizationState) {
		t.Fatalf("ValidateFinalizationState(bogus) = %v, want ErrInvalidFinalizationState", err)
	}
}

func pr6b1SeedRunning(t *testing.T, tempDir string, job *JobRecord) {
	t.Helper()
	if err := SaveJobAtomic(filepath.Join(tempDir, "jobs", job.ID, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func TestPR6B1_ReconcilePostEncodeDeadRunnerNotRunnerKilled(t *testing.T) {
	for _, state := range []FinalizationState{FinalizationStatePending, FinalizationStateFinalizing} {
		t.Run(string(state), func(t *testing.T) {
			stub := newStubSpawn()
			w, tempDir, _ := newPR3Worker(t, 4, stub)
			id := "job-6b1-postencode-" + string(state)
			localCandidate := filepath.Join(tempDir, "jobs", "_work", id, "candidate.mkv")
			job := &JobRecord{
				ID: id, Status: "running", PID: 1<<30 + 77,
				Source: filepath.Join("external", "a.mkv"), Candidate: filepath.Join("external", "out.mkv"),
				ProcessStartTime: "stub-start", ExecutionSpecDigest: pr5Digest,
				CreatedAt: time.Now().UTC().Add(-time.Hour), Attempt: 1,
				EncodeComplete: true, FinalizationState: string(state),
				LocalCandidatePath: localCandidate, PartialPath: filepath.Join("external", "out.mkv") + ".partial." + id,
			}
			pr6b1SeedRunning(t, tempDir, job)

			if err := w.ReconcileStartup(context.Background()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := pr6b1LoadJob(t, filepath.Join(tempDir, "jobs"), id)
			if got.Status != "running" {
				t.Fatalf("post-encode job must stay nonterminal running, got %q", got.Status)
			}
			if got.FailureClassification == "runner_killed" {
				t.Fatalf("post-encode job must not be runner_killed: %+v", got)
			}
			if got.PID != 0 || got.ProcessStartTime != "" {
				t.Fatalf("runner identity must be normalized to PID 0, got pid=%d start=%q", got.PID, got.ProcessStartTime)
			}
			if got.LocalCandidatePath != localCandidate {
				t.Fatalf("local candidate must be preserved, got %q want %q", got.LocalCandidatePath, localCandidate)
			}
			if !got.FinishedAt.IsZero() {
				t.Fatalf("post-encode job must not get a finished_at")
			}
			if _, err := os.Stat(filepath.Join(tempDir, "jobs", id, "terminal.json")); !os.IsNotExist(err) {
				t.Fatalf("post-encode job must not get a terminal marker")
			}
		})
	}
}

func TestPR6B1_ReconcileLegacyDeadRunningStillRunnerKilled(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	id := "job-6b1-legacy-dead"
	job := &JobRecord{
		ID: id, Status: "running", PID: 1<<30 + 78,
		Source: filepath.Join("src", "a.mkv"), Candidate: filepath.Join("out", "a.mkv"),
		ProcessStartTime: "stub-start", ExecutionSpecDigest: pr5Digest,
		CreatedAt: time.Now().UTC().Add(-time.Hour), Attempt: 1,
	}
	pr6b1SeedRunning(t, tempDir, job)

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := pr6b1LoadJob(t, filepath.Join(tempDir, "jobs"), id)
	if got.Status != "failed" || got.FailureClassification != "runner_killed" {
		t.Fatalf("legacy dead running must stay runner_killed, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", id, "terminal.json")); err != nil {
		t.Fatalf("legacy runner_killed must get a terminal marker: %v", err)
	}
}

func TestPR6B1_ReconcileCancelledPostEncodeStaysCancelled(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	id := "job-6b1-cancelled-postencode"
	job := &JobRecord{
		ID: id, Status: "cancelled", PID: 0,
		Source: filepath.Join("src", "a.mkv"), Candidate: filepath.Join("out", "a.mkv"),
		ExecutionSpecDigest: pr5Digest, FinishedAt: time.Now().UTC().Add(-time.Minute),
		CreatedAt:      time.Now().UTC().Add(-time.Hour),
		EncodeComplete: true, FinalizationState: string(FinalizationStatePending),
	}
	pr6b1SeedRunning(t, tempDir, job)

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := pr6b1LoadJob(t, filepath.Join(tempDir, "jobs"), id)
	if got.Status != "cancelled" {
		t.Fatalf("cancelled post-encode job must stay cancelled, got %q", got.Status)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "jobs", id, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("cancelled job must not get a new terminal marker during reconcile")
	}
}

func TestPR6B1_ReconcileUnknownOperationalStateFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		job  *JobRecord
	}{
		{
			name: "unknown staging",
			job: &JobRecord{
				ID: "job-6b1-bad-staging", Status: "running", PID: 1<<30 + 79,
				ExecutionSpecDigest: pr5Digest, EncodeComplete: true,
				FinalizationState: string(FinalizationStatePending), StagingState: "bogus",
				CreatedAt: time.Now().UTC().Add(-time.Hour),
			},
		},
		{
			name: "unknown finalization",
			job: &JobRecord{
				ID: "job-6b1-bad-finalization", Status: "running", PID: 1<<30 + 80,
				ExecutionSpecDigest: pr5Digest, EncodeComplete: true,
				FinalizationState: "bogus", StagingState: string(StagingStateNotRequired),
				CreatedAt: time.Now().UTC().Add(-time.Hour),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubSpawn()
			w, tempDir, _ := newPR3Worker(t, 4, stub)
			pr6b1SeedRunning(t, tempDir, tc.job)

			if err := w.ReconcileStartup(context.Background()); err == nil {
				t.Fatalf("unknown operational state must fail closed")
			}
			got := pr6b1LoadJob(t, filepath.Join(tempDir, "jobs"), tc.job.ID)
			if got.Status != "running" || got.FailureClassification == "runner_killed" {
				t.Fatalf("job must be left unmutated on fail-closed reconcile: %+v", got)
			}
		})
	}
}

func TestPR6B1_ReconcilePostEncodeLeavesBenchmarkUntouched(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 4, stub)
	stateDir := filepath.Join(tempDir, "jobs")

	benchDir := filepath.Join(stateDir, "bench-6b1")
	if err := os.MkdirAll(benchDir, 0755); err != nil {
		t.Fatal(err)
	}
	benchJSON := `{"id":"bench-6b1","status":"completed"}`
	if err := os.WriteFile(filepath.Join(benchDir, "benchmark.json"), []byte(benchJSON), 0644); err != nil {
		t.Fatal(err)
	}

	id := "job-6b1-postencode-bench"
	job := &JobRecord{
		ID: id, Status: "running", PID: 1<<30 + 81,
		ExecutionSpecDigest: pr5Digest, EncodeComplete: true,
		FinalizationState: string(FinalizationStatePending),
		CreatedAt:         time.Now().UTC().Add(-time.Hour),
	}
	pr6b1SeedRunning(t, tempDir, job)

	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(benchDir, "benchmark.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != benchJSON {
		t.Fatalf("benchmark.json must be untouched, got %s", raw)
	}
	if _, err := os.Stat(filepath.Join(benchDir, "terminal.json")); !os.IsNotExist(err) {
		t.Fatalf("benchmark-only dir must get no terminal marker")
	}
}

func TestPR6B1_StatusExposesOperationalFields(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cand := filepath.Join(tempDir, "out.mkv")
	cfg := pr6b1Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)

	const id = "job-6b1-status"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	st, err := w.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.StagingState != string(StagingStateNotRequired) {
		t.Fatalf("status staging_state = %q, want not_required", st.StagingState)
	}
	if st.EffectiveInputPath != filepath.Clean(src) {
		t.Fatalf("status effective_input_path = %q, want source", st.EffectiveInputPath)
	}
	if st.LocalCandidatePath != filepath.Clean(cand) {
		t.Fatalf("status local_candidate_path = %q, want candidate", st.LocalCandidatePath)
	}
	if st.IntendedDestination != filepath.Clean(cand) {
		t.Fatalf("status intended_destination = %q, want candidate", st.IntendedDestination)
	}
	if st.FinalizationState != string(FinalizationStateNotRequired) {
		t.Fatalf("status finalization_state = %q, want not_required", st.FinalizationState)
	}
}

// pr6b1PostEncodeConfig builds a worker config whose operational metadata is
// non-trivial (staging always, external destination), so reuse tests can prove
// the stored metadata is preserved verbatim.
func pr6b1PostEncodeConfig(tempDir, extRoot string) *WorkerConfig {
	return pr6b1Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyAlways
	})
}

func TestPR6B1_SubmitStrongIdempotencyPostEncodeReuseNotRunnerKilled(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "out.mkv")
	cfg := pr6b1PostEncodeConfig(tempDir, extRoot)
	stub := newStubSpawn()
	w := NewWorker(cfg)
	stub.install(w)
	ctx := context.Background()

	const id = "job-6b1-idem-postencode"
	req := SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand, IdempotencyKey: "key-6b1-idem-postencode"}

	if _, err := w.Submit(ctx, req, "test-exe", ""); err != nil {
		t.Fatalf("initial Submit: %v", err)
	}
	spawnsAfterFirst := stub.n()
	if spawnsAfterFirst == 0 {
		t.Fatal("expected the initial submit to spawn once")
	}

	jobFile := filepath.Join(cfg.StateDir, id, "job.json")
	job := pr6b1LoadJob(t, cfg.StateDir, id)
	job.Status = "running"
	job.PID = 1<<30 + 600
	job.ProcessStartTime = "stub-start"
	job.EncodeComplete = true
	job.FinalizationState = string(FinalizationStatePending)
	staging := job.StagingState
	effective := job.EffectiveInputPath
	localCandidate := job.LocalCandidatePath
	partial := job.PartialPath
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	stub.kill(id)

	resp, err := w.Submit(ctx, req, "test-exe", "")
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if !resp.Reused {
		t.Fatalf("post-encode resubmit must be reused, got %+v", resp)
	}
	if resp.Status != "running" {
		t.Fatalf("post-encode resubmit status = %q, want running", resp.Status)
	}
	if stub.n() != spawnsAfterFirst {
		t.Fatalf("post-encode resubmit must not spawn (spawns %d -> %d)", spawnsAfterFirst, stub.n())
	}

	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "running" || got.PID != 0 || got.ProcessStartTime != "" {
		t.Fatalf("post-encode reuse must normalize to running PID 0, got %+v", got)
	}
	if got.FailureClassification == "runner_killed" || !got.FinishedAt.IsZero() {
		t.Fatalf("post-encode reuse must not be runner_killed/terminal: %+v", got)
	}
	if got.StagingState != staging || got.EffectiveInputPath != effective ||
		got.LocalCandidatePath != localCandidate || got.PartialPath != partial {
		t.Fatalf("operational metadata must be preserved: got %+v", got)
	}
	if !got.EncodeComplete || got.FinalizationState != string(FinalizationStatePending) {
		t.Fatalf("post-encode markers must be preserved: %+v", got)
	}
}

func TestPR6B1_SubmitLegacySameIDPostEncodeReuseNotRunnerKilled(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "out.mkv")
	cfg := pr6b1PostEncodeConfig(tempDir, extRoot)
	stub := newStubSpawn()
	w := NewWorker(cfg)
	stub.install(w)
	ctx := context.Background()

	// A dot-prefixed job id makes the durable idempotency scan skip this job
	// dir, so the resubmit exercises the legacy same-ID branch instead of the
	// strong-idempotency match branch.
	const id = ".job-6b1-legacy-postencode"
	req := SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand, IdempotencyKey: "key-6b1-legacy-postencode"}

	if _, err := w.Submit(ctx, req, "test-exe", ""); err != nil {
		t.Fatalf("initial Submit: %v", err)
	}
	if stub.n() != 0 {
		t.Fatalf("dot-prefixed job id must not be scheduled/spawned, got %d spawns", stub.n())
	}

	jobFile := filepath.Join(cfg.StateDir, id, "job.json")
	job := pr6b1LoadJob(t, cfg.StateDir, id)
	job.Status = "running"
	job.PID = 1<<30 + 601
	job.ProcessStartTime = "stub-start"
	job.EncodeComplete = true
	job.FinalizationState = string(FinalizationStatePending)
	localCandidate := job.LocalCandidatePath
	partial := job.PartialPath
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	stub.kill(id)

	resp, err := w.Submit(ctx, req, "test-exe", "")
	if err != nil {
		t.Fatalf("legacy same-ID resubmit: %v", err)
	}
	if !resp.Reused || resp.Status != "running" {
		t.Fatalf("legacy post-encode resubmit must be reused running, got %+v", resp)
	}
	if stub.n() != 0 {
		t.Fatalf("legacy post-encode resubmit must not spawn, got %d", stub.n())
	}

	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "running" || got.PID != 0 || got.ProcessStartTime != "" {
		t.Fatalf("legacy post-encode reuse must normalize to running PID 0, got %+v", got)
	}
	if got.FailureClassification == "runner_killed" {
		t.Fatalf("legacy post-encode reuse must not be runner_killed: %+v", got)
	}
	if got.LocalCandidatePath != localCandidate || got.PartialPath != partial {
		t.Fatalf("legacy operational metadata must be preserved: got %+v", got)
	}
}

func TestPR6B1_CountActiveJobsDoesNotKillPostEncodePending(t *testing.T) {
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 1, stub)
	stateDir := filepath.Join(tempDir, "jobs")
	id := "job-6b1-count-postencode"
	job := &JobRecord{
		ID: id, Status: "running", PID: 1<<30 + 602, ProcessStartTime: "stub-start",
		Source: filepath.Join("src", "a.mkv"), Candidate: filepath.Join("out", "a.mkv"),
		ExecutionSpecDigest: pr5Digest, EncodeComplete: true,
		FinalizationState:  string(FinalizationStatePending),
		LocalCandidatePath: filepath.Join(stateDir, "_work", id, "candidate.mkv"),
		CreatedAt:          time.Now().UTC().Add(-time.Hour),
	}
	if err := SaveJobAtomic(filepath.Join(stateDir, id, "job.json"), job); err != nil {
		t.Fatal(err)
	}

	count, err := w.countActiveJobs("")
	if err != nil {
		t.Fatalf("countActiveJobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("post-encode job must not occupy a slot, got count %d", count)
	}
	got := pr6b1LoadJob(t, stateDir, id)
	if got.Status != "running" || got.PID != 0 || got.ProcessStartTime != "" {
		t.Fatalf("countActiveJobs must normalize to running PID 0, got %+v", got)
	}
	if got.FailureClassification == "runner_killed" {
		t.Fatalf("countActiveJobs must not runner_kill a post-encode job: %+v", got)
	}

	// The public scheduler path also runs capacity counting; it must not kill
	// the post-encode job either.
	if _, err := w.ScheduleQueued(context.Background(), "test-exe", ""); err != nil {
		t.Fatalf("ScheduleQueued: %v", err)
	}
	got2 := pr6b1LoadJob(t, stateDir, id)
	if got2.Status != "running" || got2.PID != 0 || got2.FailureClassification == "runner_killed" {
		t.Fatalf("scheduling must not runner_kill a post-encode job: %+v", got2)
	}
}

func TestPR6B1_ReconcileTerminalStatesIgnoreUnknownOperationalState(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"cancelled", "cancelled"},
		{"completed", "completed"},
		{"failed", "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubSpawn()
			w, tempDir, _ := newPR3Worker(t, 4, stub)
			stateDir := filepath.Join(tempDir, "jobs")
			job := &JobRecord{
				ID: "job-6b1-terminal-" + tc.status, Status: tc.status,
				Source: filepath.Join("src", "a.mkv"), Candidate: filepath.Join("out", "a.mkv"),
				ExecutionSpecDigest: pr5Digest, FinishedAt: time.Now().UTC().Add(-time.Minute),
				CreatedAt:    time.Now().UTC().Add(-time.Hour),
				StagingState: "bogus", FinalizationState: "also-bogus",
			}
			if err := SaveJobAtomic(filepath.Join(stateDir, job.ID, "job.json"), job); err != nil {
				t.Fatal(err)
			}

			if err := w.ReconcileStartup(context.Background()); err != nil {
				t.Fatalf("%s with unknown operational state must reconcile without error: %v", tc.status, err)
			}
			got := pr6b1LoadJob(t, stateDir, job.ID)
			if got.Status != tc.status || got.StagingState != "bogus" || got.FinalizationState != "also-bogus" {
				t.Fatalf("terminal %s must be untouched, got %+v", tc.status, got)
			}
			if _, err := os.Stat(filepath.Join(stateDir, job.ID, "terminal.json")); !os.IsNotExist(err) {
				t.Fatalf("terminal %s must not get a new terminal marker", tc.status)
			}
		})
	}
}

func TestPR6B1_SubmitIdempotentReusePreservesOperationalMetadata(t *testing.T) {
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "extA")
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv"))
	cand := filepath.Join(extRoot, "out.mkv")
	stateDir := filepath.Join(tempDir, "jobs")
	workA := filepath.Join(tempDir, "workA")
	workB := filepath.Join(tempDir, "workB")

	stub := newStubSpawn()
	cfgA := &WorkerConfig{
		StateDir: stateDir, AllowedRoots: []string{tempDir}, MaxParallelJobs: 1,
		ExternalRoots: []string{extRoot}, StagingPolicy: StagingPolicyAlways, LocalWorkDir: workA,
	}
	wA := NewWorker(cfgA)
	stub.install(wA)

	const id = "job-6b1-idem-preserve"
	req := SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand, IdempotencyKey: "key-6b1-preserve"}
	if _, err := wA.Submit(context.Background(), req, "test-exe", ""); err != nil {
		t.Fatalf("initial Submit: %v", err)
	}
	first := pr6b1LoadJob(t, stateDir, id)

	// Different operational config, same state dir and semantic spec.
	cfgB := &WorkerConfig{
		StateDir: stateDir, AllowedRoots: []string{tempDir}, MaxParallelJobs: 1,
		ExternalRoots: nil, StagingPolicy: StagingPolicyNever, LocalWorkDir: workB,
	}
	wB := NewWorker(cfgB)
	stub.install(wB)

	resp, err := wB.Submit(context.Background(), req, "test-exe", "")
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if !resp.Reused {
		t.Fatalf("same key/spec resubmit must be reused, got %+v", resp)
	}

	second := pr6b1LoadJob(t, stateDir, id)
	if second.StagingPolicy != first.StagingPolicy ||
		second.StagingState != first.StagingState ||
		second.EffectiveInputPath != first.EffectiveInputPath ||
		second.StagedInputPath != first.StagedInputPath ||
		second.LocalCandidatePath != first.LocalCandidatePath ||
		second.IntendedDestination != first.IntendedDestination ||
		second.FinalizationState != first.FinalizationState ||
		second.PartialPath != first.PartialPath {
		t.Fatalf("idempotent reuse must not recompute operational metadata:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if second.EffectiveInputPath != filepath.Join(workA, id, "input.mkv") {
		t.Fatalf("stored effective input must remain under the original work dir, got %q", second.EffectiveInputPath)
	}
	if second.LocalCandidatePath != filepath.Join(workA, id, "candidate.mkv") {
		t.Fatalf("stored local candidate must remain under the original work dir, got %q", second.LocalCandidatePath)
	}

	// No new dirs or copies from the resubmit (neither work dir is created).
	if _, err := os.Stat(workB); !os.IsNotExist(err) {
		t.Fatalf("resubmit must not create the new local work dir %s", workB)
	}
	if _, err := os.Stat(workA); !os.IsNotExist(err) {
		t.Fatalf("Phase 6B1 must not create/copy into local work dir %s", workA)
	}
	if _, err := os.Stat(first.StagedInputPath); !os.IsNotExist(err) {
		t.Fatalf("no staged copy may exist: %s", first.StagedInputPath)
	}
}
