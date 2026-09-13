package transcodeworker

import (
	"context"
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

// BenchmarkRecord represents the persistent state stored in benchmark.json on the worker.
type BenchmarkRecord struct {
	ProtocolVersion  int                               `json:"protocol_version"`
	ID               string                            `json:"id"`
	Status           string                            `json:"status"` // queued, running, completed, failed, cancelled
	Source           string                            `json:"source"`
	SourceDuration   float64                           `json:"source_duration,omitempty"`
	Metric           string                            `json:"metric"`
	Samples          []transcode.BenchmarkSampleWindow `json:"samples"`
	Candidates       []transcode.BenchmarkCandidate    `json:"candidates"`
	RequestDigest    string                            `json:"request_digest"`
	PID              int                               `json:"pid"`
	ProcessStartTime string                            `json:"process_start_time,omitempty"`
	CreatedAt        time.Time                         `json:"created_at"`
	StartedAt        time.Time                         `json:"started_at,omitempty"`
	FinishedAt       time.Time                         `json:"finished_at,omitempty"`
	ExitCode         int                               `json:"exit_code,omitempty"`
	Error            string                            `json:"error,omitempty"`
}

// BenchmarkRunner defines the pluggable executor interface for running benchmarks.
type BenchmarkRunner interface {
	RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error
}

// defaultBenchmarkRunner is the production runner for Phase 4A which fails closed.
type defaultBenchmarkRunner struct{}

func (r *defaultBenchmarkRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	return errors.New("benchmark runner not implemented (fail closed; awaiting Phase 4B)")
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

// IsBenchmarkProcessAlive checks whether the background process for a benchmark job is alive and matches its identity.
func IsBenchmarkProcessAlive(b *BenchmarkRecord) bool {
	if b == nil || b.PID <= 1 {
		return false
	}
	if !IsProcessAlive(b.PID) {
		return false
	}
	cmd, startTime, err := GetProcessIdentity(b.PID)
	if err != nil {
		return false
	}
	if b.ProcessStartTime != "" && startTime != "" && b.ProcessStartTime != startTime {
		return false
	}
	if strings.Contains(cmd, "_internal_benchmark") && !strings.Contains(cmd, b.ID) {
		return false
	}
	return true
}

func (w *Worker) getBenchmarkRunner() BenchmarkRunner {
	if w.benchmarkRunner != nil {
		return w.benchmarkRunner
	}
	return &defaultBenchmarkRunner{}
}

// SetBenchmarkRunner injects a custom runner for lifecycle testing or future phases.
func (w *Worker) SetBenchmarkRunner(runner BenchmarkRunner) {
	w.benchmarkRunner = runner
}

// BenchmarkSubmit initiates a detached benchmark job with strict validation, idempotency, and workspace management.
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
	fi, err := os.Stat(cleanSource)
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
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		resp.Error = fmt.Sprintf("invalid benchmark job id %q (path traversal attempt)", req.ID)
		return resp, fmt.Errorf("invalid benchmark job id: path traversal")
	}

	// Collision check: reject if a transcode job already exists with this ID
	transcodeJobFile := filepath.Join(jobDir, "job.json")
	if _, err := os.Stat(transcodeJobFile); err == nil {
		resp.Error = fmt.Sprintf("job ID collision: %q already exists as a transcode job", req.ID)
		return resp, fmt.Errorf("job ID collision with transcode job")
	}

	benchFile := filepath.Join(jobDir, "benchmark.json")
	reqDigest, err := transcode.DigestBenchmarkRequest(&req)
	if err != nil {
		resp.Error = fmt.Sprintf("computing request digest: %v", err)
		return resp, err
	}

	// Idempotency check: if benchmark record already exists
	if existing, err := LoadBenchmark(benchFile); err == nil && existing != nil {
		if existing.RequestDigest != reqDigest {
			resp.Error = fmt.Sprintf("job ID collision: existing benchmark job %q has different request digest (fail closed)", req.ID)
			return resp, fmt.Errorf("job ID collision with mismatched digest")
		}

		if existing.Status == "running" || existing.Status == "queued" {
			if IsBenchmarkProcessAlive(existing) {
				resp.Status = existing.Status
				return resp, nil
			}
			// Process died without updating status
			existing.Status = "failed"
			existing.FinishedAt = time.Now().UTC()
			existing.Error = "process terminated unexpectedly"
			_ = SaveBenchmarkAtomic(benchFile, existing)
			_ = w.CleanBenchmarkSamples(req.ID)
		} else {
			resp.Status = existing.Status
			return resp, nil
		}
	}

	// Check concurrency / busy status against shared worker slot capacity
	activeJobs, err := w.countActiveJobs(req.ID)
	if err != nil {
		resp.Error = fmt.Sprintf("checking active jobs: %v", err)
		return resp, err
	}
	if activeJobs >= w.cfg.MaxParallelJobs {
		resp.Error = fmt.Sprintf("worker busy: maximum parallel jobs (%d) reached", w.cfg.MaxParallelJobs)
		return resp, fmt.Errorf("worker busy: max parallel jobs reached")
	}

	// Create workspace: only StateDir/<benchmark-id>/ and a child samples/ workspace
	samplesDir := filepath.Join(jobDir, "samples")
	if err := os.MkdirAll(samplesDir, 0755); err != nil {
		resp.Error = fmt.Sprintf("creating samples workspace: %v", err)
		return resp, err
	}

	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              req.ID,
		Status:          "queued",
		Source:          cleanSource,
		SourceDuration:  req.SourceDuration,
		Metric:          strings.ToLower(strings.TrimSpace(req.Metric)),
		Samples:         req.Samples,
		Candidates:      req.Candidates,
		RequestDigest:   reqDigest,
		CreatedAt:       time.Now().UTC(),
	}

	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		resp.Error = fmt.Sprintf("saving initial benchmark state: %v", err)
		_ = w.CleanBenchmarkSamples(req.ID)
		return resp, err
	}

	// Launch decoupled runner process
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, "_internal_benchmark", req.ID)

	cmd := exec.Command(selfExe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Independent session so it survives SSH disconnect
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		record.Status = "failed"
		record.Error = fmt.Sprintf("spawning benchmark process: %v", err)
		record.FinishedAt = time.Now().UTC()
		_ = SaveBenchmarkAtomic(benchFile, record)
		_ = w.CleanBenchmarkSamples(req.ID)
		resp.Error = record.Error
		return resp, err
	}

	resp.Status = "queued"
	return resp, nil
}

// CleanBenchmarkSamples idempotently removes only the child samples/ directory of the benchmark job.
// Never deletes the job record or the source file.
func (w *Worker) CleanBenchmarkSamples(jobID string) error {
	cleanID := strings.TrimSpace(jobID)
	if cleanID == "" {
		return errors.New("jobID is required")
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)

	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("invalid jobID for cleanup: path traversal")
	}

	samplesDir := filepath.Join(jobDir, "samples")
	// Double check that samplesDir is strictly child of jobDir and basename is "samples"
	if filepath.Dir(samplesDir) != jobDir || filepath.Base(samplesDir) != "samples" {
		return fmt.Errorf("refusing to clean non-sample directory: %s", samplesDir)
	}

	return os.RemoveAll(samplesDir)
}

// BenchmarkStatus returns the current status and metadata of a benchmark job.
func (w *Worker) BenchmarkStatus(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
	cleanID := strings.TrimSpace(jobID)
	st := transcode.BenchmarkStatus{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              cleanID,
	}

	if cleanID == "" {
		st.Error = "jobID is required"
		return st, errors.New("jobID is required")
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
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
	st.CreatedAt = record.CreatedAt
	st.StartedAt = record.StartedAt
	st.FinishedAt = record.FinishedAt
	st.Error = record.Error
	st.Status = record.Status

	if record.Status == "running" || record.Status == "queued" {
		if !IsBenchmarkProcessAlive(record) && record.PID > 0 {
			record.Status = "failed"
			record.FinishedAt = time.Now().UTC()
			record.Error = "process terminated unexpectedly"
			_ = SaveBenchmarkAtomic(benchFile, record)
			_ = w.CleanBenchmarkSamples(cleanID)
			st.Status = record.Status
			st.Error = record.Error
			st.FinishedAt = record.FinishedAt
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

	if cleanID == "" {
		resp.Error = "jobID is required"
		return resp, errors.New("jobID is required")
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, cleanID)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	record, err := LoadBenchmark(benchFile)
	if err != nil {
		resp.Error = fmt.Sprintf("benchmark job not found: %v", err)
		return resp, err
	}

	if record.Status == "queued" || record.Status == "running" {
		if record.PID > 1 && IsProcessAlive(record.PID) {
			proc, err := os.FindProcess(record.PID)
			if err == nil {
				_ = proc.Signal(syscall.SIGTERM)
				// Small grace check
				time.Sleep(50 * time.Millisecond)
				if IsProcessAlive(record.PID) {
					_ = proc.Signal(syscall.SIGKILL)
				}
			}
		}
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
func (w *Worker) InternalBenchmark(ctx context.Context, jobID string) error {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	benchFile := filepath.Join(jobDir, "benchmark.json")

	record, err := LoadBenchmark(benchFile)
	if err != nil {
		return fmt.Errorf("loading benchmark %s: %w", jobID, err)
	}

	if record.Status != "queued" && record.Status != "running" {
		return fmt.Errorf("benchmark %s is not in runnable state (status=%s)", jobID, record.Status)
	}

	record.PID = os.Getpid()
	if _, lstart, err := GetProcessIdentity(record.PID); err == nil {
		record.ProcessStartTime = lstart
	}
	record.Status = "running"
	record.StartedAt = time.Now().UTC()
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		return fmt.Errorf("updating benchmark to running: %w", err)
	}

	runner := w.getBenchmarkRunner()
	runErr := runner.RunBenchmark(ctx, w, record)

	// Clean samples workspace regardless of outcome; record persists
	_ = w.CleanBenchmarkSamples(jobID)

	if runErr != nil {
		record.Status = "failed"
		record.Error = runErr.Error()
		record.FinishedAt = time.Now().UTC()
		_ = SaveBenchmarkAtomic(benchFile, record)
		return runErr
	}

	record.Status = "completed"
	record.FinishedAt = time.Now().UTC()
	if err := SaveBenchmarkAtomic(benchFile, record); err != nil {
		return fmt.Errorf("updating benchmark to completed: %w", err)
	}

	return nil
}
