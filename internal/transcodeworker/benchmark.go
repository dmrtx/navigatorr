package transcodeworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

var (
	ErrBenchmarkNotActive        = errors.New("benchmark is not active")
	ErrBenchmarkRunTokenMismatch = errors.New("benchmark run token mismatch")
)

// BenchmarkRecord represents the persistent state stored in benchmark.json on the worker.
type BenchmarkRecord struct {
	ProtocolVersion           int                                   `json:"protocol_version"`
	ID                        string                                `json:"id"`
	Status                    string                                `json:"status"` // queued, running, completed, failed, cancelled
	Source                    string                                `json:"source"`
	EffectiveSource           string                                `json:"effective_source,omitempty"`
	SourceDuration            float64                               `json:"source_duration,omitempty"`
	Metric                    string                                `json:"metric"`
	Progress                  float64                               `json:"progress"`
	Phase                     string                                `json:"phase,omitempty"`
	HeartbeatAt               time.Time                             `json:"heartbeat_at,omitempty"`
	Samples                   []transcode.BenchmarkSampleWindow     `json:"samples"`
	Candidates                []transcode.BenchmarkCandidate        `json:"candidates"`
	Quality                   *transcode.BenchmarkQualityConfig     `json:"quality,omitempty"`
	Adaptive                  *transcode.BenchmarkAdaptiveConfig    `json:"adaptive,omitempty"`
	Concurrency               *transcode.BenchmarkConcurrencyConfig `json:"concurrency,omitempty"`
	FallbackAudioBitrateBps   int64                                 `json:"fallback_audio_bitrate_bps,omitempty"`
	FallbackSubtitleSizeBytes int64                                 `json:"fallback_subtitle_size_bytes,omitempty"`
	DeclaredVideoBitrateBps   int64                                 `json:"declared_video_bitrate_bps,omitempty"`
	AttachmentBytes           int64                                 `json:"attachment_bytes,omitempty"`
	RequestDigest             string                                `json:"request_digest"`
	Attempt                   int                                   `json:"attempt"`
	RunToken                  string                                `json:"run_token"`
	PID                       int                                   `json:"pid"`
	ProcessStartTime          string                                `json:"process_start_time,omitempty"`
	CreatedAt                 time.Time                             `json:"created_at"`
	StartedAt                 time.Time                             `json:"started_at,omitempty"`
	FinishedAt                time.Time                             `json:"finished_at,omitempty"`
	ExitCode                  int                                   `json:"exit_code,omitempty"`
	Error                     string                                `json:"error,omitempty"`
	Evidence                  *BenchmarkExecutionEvidence           `json:"evidence,omitempty"`
}

// BenchmarkRunner defines the pluggable executor interface for running benchmarks.
type BenchmarkRunner interface {
	RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error
}

// LoadBenchmark loads a BenchmarkRecord from benchmark.json.
func LoadBenchmark(path string) (*BenchmarkRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading benchmark file %s: %w", path, err)
	}
	var b BenchmarkRecord
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("unmarshaling benchmark file %s: %w", path, err)
	}
	return &b, nil
}

// SaveBenchmarkAtomic saves a BenchmarkRecord to benchmark.json atomically using a temp file and rename.
func SaveBenchmarkAtomic(path string, record *BenchmarkRecord) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating benchmark directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling benchmark %s: %w", record.ID, err)
	}

	tmpFile, err := os.CreateTemp(dir, "benchmark-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp benchmark file in %s: %w", dir, err)
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp benchmark file %s: %w", tmpName, err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp benchmark file %s: %w", tmpName, err)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp benchmark file %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file %s to %s: %w", tmpName, path, err)
	}

	return nil
}

type jobFileLock struct {
	file *os.File
}

func acquireJobLock(jobDir string) (*jobFileLock, error) {
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return nil, fmt.Errorf("creating job dir for lock: %w", err)
	}
	lockFile := filepath.Join(jobDir, ".lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockFile, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", lockFile, err)
	}
	return &jobFileLock{file: f}, nil
}

func acquireCapacityLock(stateDir string) (*jobFileLock, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("creating state dir for capacity lock: %w", err)
	}
	lockFile := filepath.Join(stateDir, ".capacity.lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening capacity lock file %s: %w", lockFile, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", lockFile, err)
	}
	return &jobFileLock{file: f}, nil
}

func (l *jobFileLock) Unlock() {
	if l != nil && l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
	}
}

func generateRunToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating run token entropy: %w", err)
	}
	return fmt.Sprintf("run-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b)), nil
}

// MatchesExactBenchmarkArgs checks whether tokens contain the exact contiguous sequence:
// ["_internal_benchmark", exactJobID, exactRunToken].
// Substrings, prefixes, or suffixes do not match.
func MatchesExactBenchmarkArgs(tokens []string, jobID, runToken string) bool {
	if jobID == "" || runToken == "" || len(tokens) < 3 {
		return false
	}
	for i := 0; i <= len(tokens)-3; i++ {
		if tokens[i] == "_internal_benchmark" && tokens[i+1] == jobID && tokens[i+2] == runToken {
			return true
		}
	}
	return false
}

// IsBenchmarkExecutionAlive rigorously verifies whether the background process for a benchmark
// run is alive and matches its exact execution identity.
// It guards against PID recycling and foreign processes by verifying:
// 1. PID > 1 and process responds to signal 0.
// 2. The record has non-empty ID and RunToken.
// 3. The process's lstart matches ProcessStartTime (if recorded).
// 4. The process's tokenized argv contains the exact contiguous sequence ["_internal_benchmark", b.ID, b.RunToken].
func IsBenchmarkExecutionAlive(b *BenchmarkRecord) bool {
	if b == nil || b.PID <= 1 || b.ID == "" || b.RunToken == "" {
		return false
	}
	if !IsProcessAlive(b.PID) {
		return false
	}
	tokens, startTime, err := GetProcessTokens(b.PID)
	if err != nil {
		return false
	}
	if b.ProcessStartTime != "" && startTime != "" && b.ProcessStartTime != startTime {
		return false
	}
	return MatchesExactBenchmarkArgs(tokens, b.ID, b.RunToken)
}

func (w *Worker) getBenchmarkRunner() BenchmarkRunner {
	if w.benchmarkRunner != nil {
		return w.benchmarkRunner
	}
	return &ProductionBenchmarkRunner{}
}

// SetBenchmarkRunner injects a custom runner for lifecycle testing or future phases.
func (w *Worker) SetBenchmarkRunner(runner BenchmarkRunner) {
	w.benchmarkRunner = runner
}

func (w *Worker) spawnInternalBenchmark(selfExe, configPath, jobID, runToken string) (*exec.Cmd, error) {
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, "_internal_benchmark", jobID, runToken)

	cmd := exec.Command(selfExe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Independent session so it survives SSH disconnect
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// BenchmarkSubmit initiates a detached benchmark job with strict validation, idempotency, and workspace management.
// It is strictly idempotent across ALL states: identical ID + identical digest returns the existing status
// without spawning a new execution run. Retrying failed/cancelled benchmarks requires a new benchmark job ID.
func (w *Worker) BenchmarkSubmit(ctx context.Context, req transcode.BenchmarkRequest, selfExe, configPath string) (transcode.BenchmarkSubmitResponse, error) {
	resp := transcode.BenchmarkSubmitResponse{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              req.ID,
	}

	if err := transcode.ValidateBenchmarkRequest(&req); err != nil {
		resp.Error = err.Error()
		return resp, err
	}

	cleanSource := filepath.Clean(req.SourcePath)

	// Validate source within allowed roots (FAIL CLOSED)
	if !IsPathWithinAllowedRoots(cleanSource, w.cfg.AllowedRoots) {
		resp.Error = fmt.Sprintf("source_path %q is outside allowed roots %v (fail closed)", cleanSource, w.cfg.AllowedRoots)
		return resp, fmt.Errorf("source_path outside allowed roots: %s", cleanSource)
	}

	// Verify source exists and is not directory
	fi, err := w.statMedia(ctx, cleanSource)
	if err != nil {
		resp.Error = fmt.Sprintf("source_path %q not accessible: %v", cleanSource, err)
		return resp, fmt.Errorf("source_path not accessible: %w", err)
	}
	if fi.IsDir() {
		resp.Error = fmt.Sprintf("source_path %q is a directory", cleanSource)
		return resp, fmt.Errorf("source_path is a directory")
	}

	// Verify job directory is strictly inside StateDir (prevent path traversal)
	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, req.ID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || rel != req.ID || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		resp.Error = fmt.Sprintf("invalid benchmark job id %q (path traversal attempt)", req.ID)
		return resp, fmt.Errorf("invalid benchmark job id: path traversal")
	}

	// Capacity lock serializes slot accounting across all benchmark and transcode jobs
	capLock, err := acquireCapacityLock(cleanStateDir)
	if err != nil {
		resp.Error = fmt.Sprintf("acquiring capacity lock: %v", err)
		return resp, err
	}
	defer capLock.Unlock()

	// Per-job mutual exclusion via advisory file lock (lock order: capLock -> jobLock)
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		resp.Error = fmt.Sprintf("acquiring job lock for %s: %v", req.ID, err)
		return resp, err
	}
	defer jobLock.Unlock()

	// Collision check: reject if a transcode job already exists with this ID
	transcodeJobFile := filepath.Join(jobDir, "job.json")
	if _, err := os.Stat(transcodeJobFile); err == nil {
		resp.Error = fmt.Sprintf("job ID collision: %q already exists as a transcode job (fail closed)", req.ID)
		return resp, fmt.Errorf("job ID collision with transcode job")
	}

	benchFile := filepath.Join(jobDir, "benchmark.json")
	reqDigest, err := transcode.DigestBenchmarkRequest(&req)
	if err != nil {
		resp.Error = fmt.Sprintf("computing request digest: %v", err)
		return resp, err
	}

	// Idempotency check across all states
	if existing, err := LoadBenchmark(benchFile); err == nil && existing != nil {
		// Collision: same ID with different request digest must fail closed
		if existing.RequestDigest != reqDigest {
			resp.Error = fmt.Sprintf("job ID collision: existing benchmark job %q has different request digest (fail closed)", req.ID)
			return resp, fmt.Errorf("job ID collision with mismatched digest")
		}

		switch existing.Status {
		case "queued", "running":
			if IsBenchmarkExecutionAlive(existing) {
				// In-flight transport retry: return active status without spawning a second worker
				resp.Status = existing.Status
				return resp, nil
			}
			// Process died prematurely without updating status; reconcile to failed
			existing.Status = "failed"
			existing.FinishedAt = time.Now().UTC()
			existing.Error = "process terminated unexpectedly"
			_ = SaveBenchmarkAtomic(benchFile, existing)
			_ = w.CleanBenchmarkSamples(req.ID)
			resp.Status = "failed"
			resp.Error = existing.Error
			return resp, nil

		case "completed", "failed", "cancelled":
			// Terminal states are strictly idempotent: return existing terminal status without re-execution
			resp.Status = existing.Status
			resp.Error = existing.Error
			return resp, nil

		default:
			resp.Status = existing.Status
			resp.Error = existing.Error
			return resp, nil
		}
	}

	// New benchmark job: verify slot capacity under capacity lock
	activeJobs, err := w.countActiveJobs(req.ID)
	if err != nil {
		resp.Error = fmt.Sprintf("checking active jobs: %v", err)
		return resp, err
	}
	if activeJobs >= w.cfg.MaxParallelJobs {
		resp.Error = fmt.Sprintf("worker busy: maximum parallel jobs (%d) reached", w.cfg.MaxParallelJobs)
		return resp, fmt.Errorf("worker busy: max parallel jobs reached")
	}

	// Create workspace: only StateDir/<benchmark-id>/samples
	samplesDir := filepath.Join(jobDir, "samples")
	if err := os.MkdirAll(samplesDir, 0755); err != nil {
		resp.Error = fmt.Sprintf("creating samples workspace: %v", err)
		return resp, err
	}

	runToken, err := generateRunToken()
	if err != nil {
		resp.Error = fmt.Sprintf("generating run token: %v", err)
		_ = w.CleanBenchmarkSamples(req.ID)
		return resp, err
	}

	record := &BenchmarkRecord{
		ProtocolVersion:           transcode.WorkerProtocolVersion,
		ID:                        req.ID,
		Status:                    "queued",
		Source:                    cleanSource,
		EffectiveSource:           cleanSource,
		SourceDuration:            req.SourceDuration,
		Metric:                    strings.ToLower(strings.TrimSpace(req.Metric)),
		Samples:                   req.Samples,
		Candidates:                req.Candidates,
		Quality:                   req.Quality,
		Adaptive:                  req.Adaptive,
		Concurrency:               req.Concurrency,
		FallbackAudioBitrateBps:   req.FallbackAudioBitrateBps,
		FallbackSubtitleSizeBytes: req.FallbackSubtitleSizeBytes,
		DeclaredVideoBitrateBps:   req.DeclaredVideoBitrateBps,
		AttachmentBytes:           req.AttachmentBytes,
		RequestDigest:             reqDigest,
		Attempt:                   1,
		RunToken:                  runToken,
		CreatedAt:                 time.Now().UTC(),
	}
	if w.mediaStore != nil && w.mediaStore.Maps(cleanSource) {
		record.EffectiveSource = filepath.Join(jobDir, "source"+safeFileExtension(cleanSource))
	}

	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		resp.Error = fmt.Sprintf("saving initial benchmark state: %v", err)
		_ = w.CleanBenchmarkSamples(req.ID)
		return resp, err
	}

	cmd, err := w.spawnInternalBenchmark(selfExe, configPath, req.ID, runToken)
	if err != nil {
		record.Status = "failed"
		record.Error = fmt.Sprintf("spawning benchmark process: %v", err)
		record.FinishedAt = time.Now().UTC()
		_ = SaveBenchmarkAtomic(benchFile, record)
		_ = w.CleanBenchmarkSamples(req.ID)
		resp.Error = record.Error
		return resp, err
	}

	if w.afterSpawnHook != nil {
		w.afterSpawnHook(jobDir, cmd.Process.Pid)
	}

	record.PID = cmd.Process.Pid
	_, lstart, _ := GetProcessTokens(record.PID)
	record.ProcessStartTime = lstart
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		// Fail-closed: kill newly spawned process immediately so it is never orphaned without identity
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		record.Status = "failed"
		record.Error = fmt.Sprintf("persisting benchmark process identity: %v", err)
		record.FinishedAt = time.Now().UTC()
		_ = SaveBenchmarkAtomic(benchFile, record)
		_ = w.CleanBenchmarkSamples(req.ID)
		resp.Error = record.Error
		return resp, fmt.Errorf("persisting benchmark process identity: %w", err)
	}

	resp.Status = "queued"
	return resp, nil
}

// CleanBenchmarkSamples idempotently removes only the child samples/ directory of the benchmark job.
// Never deletes the job record or the source file.
func (w *Worker) CleanBenchmarkSamples(jobID string) error {
	cleanID := strings.TrimSpace(jobID)
	if err := transcode.ValidateBenchmarkJobID(cleanID); err != nil {
		return fmt.Errorf("invalid jobID for cleanup: %w", err)
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)

	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || rel != cleanID || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("invalid jobID for cleanup: path traversal")
	}

	// Reject if jobDir itself is a symlink
	if jfi, err := os.Lstat(jobDir); err == nil && jfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("job directory is a symlink: %s (fail closed)", jobDir)
	}

	samplesDir := filepath.Join(jobDir, "samples")
	// Double check that samplesDir is strictly child of jobDir and basename is "samples"
	if filepath.Dir(samplesDir) != jobDir || filepath.Base(samplesDir) != "samples" {
		return fmt.Errorf("refusing to clean non-sample directory: %s", samplesDir)
	}

	// Lstat check before remove: if samplesDir is a symlink, remove ONLY the symlink itself, never follow!
	fi, err := os.Lstat(samplesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(samplesDir)
	}

	// Path target evaluation before RemoveAll: verify real target is inside real jobDir
	realJobDir, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		return fmt.Errorf("resolving job directory: %w", err)
	}
	realSamplesDir, err := filepath.EvalSymlinks(samplesDir)
	if err != nil {
		return fmt.Errorf("resolving samples directory: %w", err)
	}
	relReal, err := filepath.Rel(realJobDir, realSamplesDir)
	if err != nil || relReal == "." || relReal != "samples" || strings.HasPrefix(relReal, "..") {
		return fmt.Errorf("samples directory target %s escapes job directory %s", realSamplesDir, realJobDir)
	}

	return os.RemoveAll(samplesDir)
}

// UpdateBenchmarkProgress safely updates progress, phase, and heartbeat of a benchmark job.
// It acquires the job lock, loads latest benchmark.json, verifies the RunToken, ensures status
// is still queued or running, applies monotonic progress update, and saves atomically.
// It preserves cancellation, PID, evidence, and other concurrent state.
func (w *Worker) UpdateBenchmarkProgress(jobID, runToken string, progress float64, phase string) error {
	cleanID := strings.TrimSpace(jobID)
	if err := transcode.ValidateBenchmarkJobID(cleanID); err != nil {
		return fmt.Errorf("invalid jobID for progress update: %w", err)
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || rel != cleanID || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("invalid jobID: path traversal")
	}

	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return fmt.Errorf("acquiring job lock for progress update %s: %w", cleanID, err)
	}
	defer jobLock.Unlock()

	benchFile := filepath.Join(jobDir, "benchmark.json")
	latest, err := LoadBenchmark(benchFile)
	if err != nil {
		return fmt.Errorf("loading benchmark %s: %w", cleanID, err)
	}

	// Verify the same RunToken (FAIL CLOSED)
	if latest.RunToken == "" || latest.RunToken != runToken {
		return fmt.Errorf("%w for benchmark %s (expected %q, got %q)", ErrBenchmarkRunTokenMismatch, cleanID, latest.RunToken, runToken)
	}

	// Verify status is still queued or running (do not overwrite cancelled, failed, or completed)
	if latest.Status != "queued" && latest.Status != "running" {
		return fmt.Errorf("%w for benchmark %s (status=%s)", ErrBenchmarkNotActive, cleanID, latest.Status)
	}

	// Apply monotonic progress (never regress)
	if progress > latest.Progress {
		latest.Progress = progress
	}
	if phase != "" {
		latest.Phase = phase
	}
	latest.HeartbeatAt = time.Now().UTC()

	if err := SaveBenchmarkAtomic(benchFile, latest); err != nil {
		return fmt.Errorf("saving benchmark progress for %s: %w", cleanID, err)
	}

	return nil
}

// BenchmarkStatus returns the current status and metadata of a benchmark job.
func (w *Worker) BenchmarkStatus(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
	cleanID := strings.TrimSpace(jobID)
	st := transcode.BenchmarkStatus{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              cleanID,
	}

	if err := transcode.ValidateBenchmarkJobID(cleanID); err != nil {
		st.Error = err.Error()
		return st, err
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || rel != cleanID || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		st.Error = "invalid jobID"
		return st, fmt.Errorf("invalid jobID: path traversal")
	}

	benchFile := filepath.Join(jobDir, "benchmark.json")
	record, err := LoadBenchmark(benchFile)
	if err != nil {
		st.Error = fmt.Sprintf("benchmark job not found: %v", err)
		return st, err
	}

	st.SourcePath = record.Source
	st.Metric = record.Metric
	st.SamplesPlanned = len(record.Samples)
	st.CandidatesCount = len(record.Candidates)
	st.Attempt = record.Attempt
	st.CreatedAt = record.CreatedAt
	st.StartedAt = record.StartedAt
	st.FinishedAt = record.FinishedAt
	st.Error = record.Error
	st.Status = record.Status
	st.Progress = record.Progress
	st.Phase = record.Phase
	st.HeartbeatAt = record.HeartbeatAt
	if record.Evidence != nil && record.Evidence.Decision != nil {
		st.Decision = record.Evidence.Decision
	}

	if record.Status == "running" || record.Status == "queued" {
		if !IsBenchmarkExecutionAlive(record) {
			// Acquire job lock to ensure we do not race with in-flight spawn or transition
			jobLock, err := acquireJobLock(jobDir)
			if err == nil {
				defer jobLock.Unlock()
				latest, lErr := LoadBenchmark(benchFile)
				if lErr == nil && latest != nil && (latest.Status == "running" || latest.Status == "queued") {
					if latest.Evidence != nil && latest.Evidence.Decision != nil {
						st.Decision = latest.Evidence.Decision
					}
					if !IsBenchmarkExecutionAlive(latest) {
						latest.Status = "failed"
						latest.FinishedAt = time.Now().UTC()
						latest.Error = "process terminated unexpectedly"
						_ = SaveBenchmarkAtomic(benchFile, latest)
						_ = w.CleanBenchmarkSamples(cleanID)
						st.Status = latest.Status
						st.Error = latest.Error
						st.FinishedAt = latest.FinishedAt
						st.Progress = latest.Progress
						st.Phase = latest.Phase
						st.HeartbeatAt = latest.HeartbeatAt
					} else {
						st.Status = latest.Status
						st.Error = latest.Error
						st.StartedAt = latest.StartedAt
						st.FinishedAt = latest.FinishedAt
						st.Progress = latest.Progress
						st.Phase = latest.Phase
						st.HeartbeatAt = latest.HeartbeatAt
					}
				}
			}
		}
	}

	return st, nil
}

// BenchmarkCancel terminates an active benchmark job and idempotently cleans up the samples workspace.
func (w *Worker) BenchmarkCancel(ctx context.Context, jobID string) (transcode.BenchmarkCancelResponse, error) {
	cleanID := strings.TrimSpace(jobID)
	resp := transcode.BenchmarkCancelResponse{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              cleanID,
	}

	if err := transcode.ValidateBenchmarkJobID(cleanID); err != nil {
		resp.Error = err.Error()
		return resp, err
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || rel != cleanID || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		resp.Error = "invalid jobID: path traversal attempt"
		return resp, fmt.Errorf("invalid jobID: path traversal")
	}

	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		resp.Error = fmt.Sprintf("acquiring job lock: %v", err)
		return resp, err
	}
	defer jobLock.Unlock()

	benchFile := filepath.Join(jobDir, "benchmark.json")
	record, err := LoadBenchmark(benchFile)
	if err != nil {
		resp.Error = fmt.Sprintf("benchmark job not found: %v", err)
		return resp, err
	}

	if record.Status == "queued" || record.Status == "running" {
		// Fail-closed: only signal if the process is alive and strictly matches the job's RunToken identity
		if IsBenchmarkExecutionAlive(record) {
			proc, err := os.FindProcess(record.PID)
			if err == nil {
				_ = syscall.Kill(-record.PID, syscall.SIGTERM)
				_ = proc.Signal(syscall.SIGTERM)
				time.Sleep(150 * time.Millisecond)
				if IsBenchmarkExecutionAlive(record) {
					_ = syscall.Kill(-record.PID, syscall.SIGKILL)
					_ = proc.Signal(syscall.SIGKILL)
				}
			}
		}
		// Safely reconcile status to cancelled
		record.Status = "cancelled"
		record.FinishedAt = time.Now().UTC()
		record.Error = "cancelled by coordinator"
		_ = SaveBenchmarkAtomic(benchFile, record)
	}

	_ = w.CleanBenchmarkSamples(cleanID)

	resp.Status = "cancelled"
	return resp, nil
}

// InternalBenchmark is invoked by the background decoupled process to execute the benchmark.
func (w *Worker) InternalBenchmark(ctx context.Context, jobID, runToken string) error {
	if err := transcode.ValidateBenchmarkJobID(jobID); err != nil {
		return fmt.Errorf("invalid jobID: %w", err)
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	// Phase 1: Verify token, transition from queued -> running under lock
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return fmt.Errorf("acquiring lock for %s: %w", jobID, err)
	}

	record, err := LoadBenchmark(benchFile)
	if err != nil {
		jobLock.Unlock()
		return fmt.Errorf("loading benchmark %s: %w", jobID, err)
	}

	// Exact run token verification (FAIL CLOSED)
	if record.RunToken == "" || record.RunToken != runToken {
		jobLock.Unlock()
		return fmt.Errorf("run token mismatch for benchmark %s (expected %q, got %q): failing closed",
			jobID, record.RunToken, runToken)
	}

	// Check if already cancelled or terminal before we even started
	if record.Status != "queued" && record.Status != "running" {
		jobLock.Unlock()
		return fmt.Errorf("benchmark %s is in non-runnable state (status=%s)", jobID, record.Status)
	}

	record.PID = os.Getpid()
	if _, lstart, err := GetProcessTokens(record.PID); err == nil {
		record.ProcessStartTime = lstart
	}
	record.Status = "running"
	if record.StartedAt.IsZero() {
		record.StartedAt = time.Now().UTC()
	}
	record.Phase = "starting"
	record.HeartbeatAt = time.Now().UTC()
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		jobLock.Unlock()
		return fmt.Errorf("updating benchmark to running: %w", err)
	}
	jobLock.Unlock()

	// Phase 2: Execute benchmark runner outside lock so cancellation/status can acquire lock.
	// Direct-SMB sources are first materialized on local SSD; the durable Source
	// remains the semantic NAS path used by status and idempotency.
	var runErr error
	effectiveSource := strings.TrimSpace(record.EffectiveSource)
	if effectiveSource == "" {
		effectiveSource = record.Source
	}
	if effectiveSource != record.Source {
		if w.mediaStore == nil || !w.mediaStore.Maps(record.Source) {
			runErr = fmt.Errorf("benchmark requires configured SMB direct media store for %s", record.Source)
		} else {
			runErr = w.mediaStore.DownloadAtomic(ctx, record.Source, effectiveSource)
			defer os.Remove(effectiveSource)
		}
	}
	if runErr == nil {
		record.Source = effectiveSource
		runner := w.getBenchmarkRunner()
		runErr = runner.RunBenchmark(ctx, w, record)
	}

	// Clean samples workspace regardless of outcome
	_ = w.CleanBenchmarkSamples(jobID)

	// Phase 3: Transition to terminal state under lock
	jobLock, err = acquireJobLock(jobDir)
	if err != nil {
		return fmt.Errorf("re-acquiring lock for completion: %w", err)
	}
	defer jobLock.Unlock()

	latest, err := LoadBenchmark(benchFile)
	if err != nil {
		return fmt.Errorf("loading benchmark for completion %s: %w", jobID, err)
	}

	// If run token changed, do not touch state
	if latest.RunToken != runToken {
		return nil
	}

	// If job was cancelled while runner was executing, preserve cancelled status!
	// Copy partial evidence if available without resurrecting or modifying cancelled state.
	if latest.Status == "cancelled" {
		if record.Evidence != nil {
			latest.Evidence = record.Evidence
			if err := SaveBenchmarkAtomic(benchFile, latest); err != nil {
				return fmt.Errorf("persisting partial evidence on cancelled benchmark %s: %w", jobID, err)
			}
		}
		return nil
	}

	// Monotonic transition to completed or failed
	if runErr != nil {
		latest.Status = "failed"
		latest.Error = runErr.Error()
		latest.FinishedAt = time.Now().UTC()
		latest.HeartbeatAt = time.Now().UTC()
		if record.Progress > latest.Progress {
			latest.Progress = record.Progress
		}
		if record.Evidence != nil {
			latest.Evidence = record.Evidence
		}
		_ = SaveBenchmarkAtomic(benchFile, latest)
		return runErr
	}

	latest.Status = "completed"
	latest.FinishedAt = time.Now().UTC()
	latest.HeartbeatAt = time.Now().UTC()
	latest.Progress = 100
	latest.Phase = "completed"
	latest.Error = ""
	latest.Evidence = record.Evidence
	if err := SaveBenchmarkAtomic(benchFile, latest); err != nil {
		return fmt.Errorf("updating benchmark to completed: %w", err)
	}

	return nil
}
