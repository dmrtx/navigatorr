package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"gopkg.in/yaml.v3"
)

// WorkerConfig holds the configuration for the remote worker node.
type WorkerConfig struct {
	FFmpeg          string   `json:"ffmpeg" yaml:"ffmpeg"`
	FFprobe         string   `json:"ffprobe" yaml:"ffprobe"`
	StateDir        string   `json:"state_dir" yaml:"state_dir"`
	AllowedRoots    []string `json:"allowed_roots" yaml:"allowed_roots"`
	MaxParallelJobs int      `json:"max_parallel_jobs" yaml:"max_parallel_jobs"`
	Quality         int      `json:"quality" yaml:"quality"`
	// HTTP daemon (PR1 `serve` mode) settings. Empty tokens mean "no auth",
	// which is only permitted on loopback binds (enforced in http.go).
	HTTPListen    string `json:"http_listen" yaml:"http_listen"`
	HTTPToken     string `json:"http_token" yaml:"http_token"`
	HTTPTokenFile string `json:"http_token_file" yaml:"http_token_file"`
}

// DefaultWorkerConfig returns sane defaults for an Apple Silicon Mac.
func DefaultWorkerConfig() *WorkerConfig {
	home, _ := os.UserHomeDir()
	return &WorkerConfig{
		FFmpeg:          "/opt/homebrew/bin/ffmpeg",
		FFprobe:         "/opt/homebrew/bin/ffprobe",
		StateDir:        filepath.Join(home, ".local", "share", "navigatorr-transcode", "jobs"),
		AllowedRoots:    []string{"/Volumes/media"},
		MaxParallelJobs: 1,
		Quality:         65,
		HTTPListen:      "127.0.0.1:8097",
	}
}

// LoadWorkerConfig loads worker configuration from the given file or default path.
func LoadWorkerConfig(configPath string) (*WorkerConfig, error) {
	cfg := DefaultWorkerConfig()
	if configPath == "" {
		home, _ := os.UserHomeDir()
		configPath = filepath.Join(home, ".config", "navigatorr-transcode", "config.yaml")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading worker config %s: %w", configPath, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing worker config %s: %w", configPath, err)
	}

	// Expand ~ in state_dir and roots if needed
	home, _ := os.UserHomeDir()
	if home != "" {
		if strings.HasPrefix(cfg.StateDir, "~/") {
			cfg.StateDir = filepath.Join(home, cfg.StateDir[2:])
		}
		for i, r := range cfg.AllowedRoots {
			if strings.HasPrefix(r, "~/") {
				cfg.AllowedRoots[i] = filepath.Join(home, r[2:])
			}
		}
		if strings.HasPrefix(cfg.HTTPTokenFile, "~/") {
			cfg.HTTPTokenFile = filepath.Join(home, cfg.HTTPTokenFile[2:])
		}
	}

	if cfg.MaxParallelJobs <= 0 {
		cfg.MaxParallelJobs = 1
	}
	if cfg.Quality <= 0 {
		cfg.Quality = 65
	}
	if strings.TrimSpace(cfg.HTTPListen) == "" {
		cfg.HTTPListen = "127.0.0.1:8097"
	}

	return cfg, nil
}

// Worker manages the local transcode execution on the node.
type Worker struct {
	cfg             *WorkerConfig
	ffmpegPath      string
	ffprobePath     string
	benchmarkRunner BenchmarkRunner
	afterSpawnHook  func(jobDir string, pid int)
	// spawnTranscode spawns the detached runner for a queued job. Nil selects
	// the production exec.Command path. Tests inject a stub that records the
	// spawn without forking so queue/idempotency behavior is deterministic
	// without real ffmpeg.
	spawnTranscode func(selfExe, configPath, jobID string) (pid int, startTime string, err error)
	// isAlive overrides process-liveness checks. Nil selects the production
	// IsJobProcessAlive identity check (argv + start-time anti-recycling).
	// Tests inject a stub-consistent function so stub-spawned jobs are not
	// judged by the test binary's argv. Production identity checks are never
	// weakened: the default path is unchanged.
	isAlive func(*JobRecord) bool
}

// SetTranscodeSpawner injects a custom detached-spawn function (tests).
func (w *Worker) SetTranscodeSpawner(fn func(selfExe, configPath, jobID string) (int, string, error)) {
	w.spawnTranscode = fn
}

// SetAliveFunc injects a custom job-liveness function (tests). Nil restores
// the production IsJobProcessAlive check.
func (w *Worker) SetAliveFunc(fn func(*JobRecord) bool) {
	w.isAlive = fn
}

// jobAlive reports whether a job's runner process is alive. Production uses
// IsJobProcessAlive (signal + argv/start-time identity); tests may inject a
// stub-consistent equivalent.
func (w *Worker) jobAlive(job *JobRecord) bool {
	if w.isAlive != nil {
		return w.isAlive(job)
	}
	return IsJobProcessAlive(job)
}

// NewWorker initializes a new transcode worker.
func NewWorker(cfg *WorkerConfig) *Worker {
	if cfg == nil {
		cfg = DefaultWorkerConfig()
	}
	return &Worker{
		cfg:         cfg,
		ffmpegPath:  ResolveToolPath(cfg.FFmpeg, "ffmpeg"),
		ffprobePath: ResolveToolPath(cfg.FFprobe, "ffprobe"),
	}
}

// RootStatus records the accessibility of an allowed root directory.
type RootStatus struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`
	Error    string `json:"error,omitempty"`
}

// DoctorResult contains the report from worker diagnostic checks.
type DoctorResult struct {
	OK           bool         `json:"ok"`
	OS           string       `json:"os"`
	Arch         string       `json:"arch"`
	FFmpeg       string       `json:"ffmpeg"`
	FFprobe      string       `json:"ffprobe"`
	Encoder      string       `json:"encoder"`
	AllowedRoots []RootStatus `json:"allowed_roots"`
	Error        string       `json:"error,omitempty"`
}

// Doctor executes environmental and capability checks.
func (w *Worker) Doctor(ctx context.Context) DoctorResult {
	res := DoctorResult{
		OK:      true,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		FFmpeg:  w.ffmpegPath,
		FFprobe: w.ffprobePath,
		Encoder: "hevc_videotoolbox",
	}

	var errs []string

	if runtime.GOOS != "darwin" {
		res.OK = false
		errs = append(errs, fmt.Sprintf("unsupported OS %q: worker requires darwin (macOS)", runtime.GOOS))
	}
	if runtime.GOARCH != "arm64" {
		res.OK = false
		errs = append(errs, fmt.Sprintf("unsupported architecture %q: worker requires arm64 (Apple Silicon)", runtime.GOARCH))
	}

	// Check ffmpeg
	if fi, err := os.Stat(w.ffmpegPath); err != nil || fi.IsDir() {
		res.OK = false
		errs = append(errs, fmt.Sprintf("ffmpeg not accessible at %s", w.ffmpegPath))
	}

	// Check ffprobe
	if fi, err := os.Stat(w.ffprobePath); err != nil || fi.IsDir() {
		res.OK = false
		errs = append(errs, fmt.Sprintf("ffprobe not accessible at %s", w.ffprobePath))
	}

	// Check hevc_videotoolbox encoder
	if res.OK || (runtime.GOOS == "darwin" && w.ffmpegPath != "") {
		hasEncoder, err := CheckVideoToolboxEncoder(ctx, w.ffmpegPath)
		if err != nil || !hasEncoder {
			res.OK = false
			errs = append(errs, fmt.Sprintf("hevc_videotoolbox encoder not available in ffmpeg (%v)", err))
		}
	}

	// Check allowed roots
	if len(w.cfg.AllowedRoots) == 0 {
		res.OK = false
		errs = append(errs, "no allowed_roots configured")
	}

	for _, root := range w.cfg.AllowedRoots {
		rs := RootStatus{Path: root}
		fi, err := os.Stat(root)
		if err != nil {
			rs.Error = err.Error()
			res.AllowedRoots = append(res.AllowedRoots, rs)
			res.OK = false
			errs = append(errs, fmt.Sprintf("allowed root %s not accessible: %v", root, err))
			continue
		}
		if !fi.IsDir() {
			rs.Error = "not a directory"
			res.AllowedRoots = append(res.AllowedRoots, rs)
			res.OK = false
			errs = append(errs, fmt.Sprintf("allowed root %s is not a directory", root))
			continue
		}
		rs.Exists = true

		// Check readable
		if entries, err := os.ReadDir(root); err == nil {
			rs.Readable = true
			_ = entries
		} else {
			rs.Error = fmt.Sprintf("read error: %v", err)
			res.OK = false
			errs = append(errs, fmt.Sprintf("allowed root %s not readable: %v", root, err))
		}

		// Check writable via temp file
		testFile := filepath.Join(root, fmt.Sprintf(".doctor_test_%d", time.Now().UnixNano()))
		if err := os.WriteFile(testFile, []byte("ok"), 0644); err == nil {
			rs.Writable = true
			_ = os.Remove(testFile)
		} else {
			rs.Error = fmt.Sprintf("write error: %v", err)
			res.OK = false
			errs = append(errs, fmt.Sprintf("allowed root %s not writable: %v", root, err))
		}

		res.AllowedRoots = append(res.AllowedRoots, rs)
	}

	if len(errs) > 0 {
		res.Error = strings.Join(errs, "; ")
	}

	return res
}

// SubmitRequest defines the JSON input for the submit command.
type SubmitRequest struct {
	ID                  string          `json:"id"`
	SourcePath          string          `json:"source_path"`
	CandidatePath       string          `json:"candidate_path"`
	Profile             string          `json:"profile"`
	Plan                *transcode.Plan `json:"plan,omitempty"`
	IdempotencyKey      string          `json:"idempotency_key,omitempty"`
	ExecutionSpecDigest string          `json:"execution_spec_digest,omitempty"`
}

// SubmitResponse defines the JSON output for the submit command.
type SubmitResponse struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	CandidatePath       string          `json:"candidate_path,omitempty"`
	Plan                *transcode.Plan `json:"plan,omitempty"`
	IdempotencyKey      string          `json:"idempotency_key,omitempty"`
	ExecutionSpecDigest string          `json:"execution_spec_digest,omitempty"`
	Reused              bool            `json:"-"`
	Error               string          `json:"error,omitempty"`
}

// IdempotencyConflictError is a deterministic strong-idempotency conflict:
// the same idempotency key was already persisted with a different canonical
// execution-spec digest. It maps to HTTP 409 and is definitive (never
// UncertainError).
type IdempotencyConflictError struct {
	Key             string
	ExistingJobID   string
	ExistingDigest  string
	RequestedDigest string
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("idempotency_conflict: key %q already persists job %q with different execution_spec_digest (existing=%s requested=%s)",
		e.Key, e.ExistingJobID, e.ExistingDigest, e.RequestedDigest)
}

// IsIdempotencyConflict reports whether err is an *IdempotencyConflictError.
func IsIdempotencyConflict(err error) bool {
	var ce *IdempotencyConflictError
	return errors.As(err, &ce)
}

// IsPathWithinAllowedRoots verifies that the given path is strictly inside one of the allowed roots.
func IsPathWithinAllowedRoots(path string, allowedRoots []string) bool {
	if len(allowedRoots) == 0 {
		return false
	}
	clean := filepath.Clean(path)
	for _, root := range allowedRoots {
		cleanRoot := filepath.Clean(root)
		if clean == cleanRoot {
			return true
		}
		if strings.HasPrefix(clean, cleanRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Submit initiates a detached transcode job.
func (w *Worker) Submit(ctx context.Context, req SubmitRequest, selfExe, configPath string) (SubmitResponse, error) {
	trimmedID := strings.TrimSpace(req.ID)
	if trimmedID == "" {
		return SubmitResponse{Error: "missing request id"}, errors.New("missing request id")
	}
	if strings.HasPrefix(strings.ToLower(trimmedID), "bench-") {
		return SubmitResponse{ID: trimmedID, Error: "transcode job ID cannot use reserved benchmark prefix 'bench-' (fail closed)"},
			errors.New("transcode job ID cannot use reserved benchmark prefix 'bench-'")
	}
	if strings.TrimSpace(req.SourcePath) == "" {
		return SubmitResponse{ID: trimmedID, Error: "missing source_path"}, errors.New("missing source_path")
	}
	if strings.TrimSpace(req.CandidatePath) == "" {
		return SubmitResponse{ID: trimmedID, Error: "missing candidate_path"}, errors.New("missing candidate_path")
	}

	cleanSource := filepath.Clean(req.SourcePath)
	cleanCandidate := filepath.Clean(req.CandidatePath)

	// Reject candidate == source (FAIL CLOSED)
	if cleanSource == cleanCandidate {
		return SubmitResponse{ID: trimmedID, Error: "candidate_path cannot equal source_path (fail closed)"},
			errors.New("candidate_path cannot equal source_path")
	}

	// Reject source outside allowed roots (FAIL CLOSED)
	if !IsPathWithinAllowedRoots(cleanSource, w.cfg.AllowedRoots) {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("source_path %q is outside allowed roots %v (fail closed)", cleanSource, w.cfg.AllowedRoots)},
			fmt.Errorf("source_path outside allowed roots: %s", cleanSource)
	}

	// Reject candidate outside allowed roots (FAIL CLOSED)
	if !IsPathWithinAllowedRoots(cleanCandidate, w.cfg.AllowedRoots) {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("candidate_path %q is outside allowed roots %v (fail closed)", cleanCandidate, w.cfg.AllowedRoots)},
			fmt.Errorf("candidate_path outside allowed roots: %s", cleanCandidate)
	}

	// Verify source exists and is not directory
	fi, err := os.Stat(cleanSource)
	if err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("source_path %q not accessible: %v", cleanSource, err)},
			fmt.Errorf("source_path not accessible: %w", err)
	}
	if fi.IsDir() {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("source_path %q is a directory", cleanSource)},
			fmt.Errorf("source_path %q is a directory", cleanSource)
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, trimmedID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel == "." || rel != trimmedID || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("invalid transcode job id %q (path traversal attempt)", trimmedID)},
			fmt.Errorf("invalid transcode job id: path traversal")
	}

	profile := req.Profile
	if strings.TrimSpace(profile) == "" {
		profile = "hevc-vt"
	}

	plan, err := ResolveWorkerPlan(profile, req.Plan)
	if err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("invalid transcode profile or plan: %v", err)}, err
	}

	// Canonical execution-spec digest over the immutable resolved request.
	// Never trust caller-supplied digest text: recompute and verify.
	canonicalDigest, err := transcode.DigestTranscodeExecutionSpec(cleanSource, cleanCandidate, profile, plan)
	if err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("computing execution spec digest: %v", err)}, err
	}
	if strings.TrimSpace(req.ExecutionSpecDigest) != "" && strings.TrimSpace(req.ExecutionSpecDigest) != canonicalDigest {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("execution_spec_digest mismatch (fail closed): caller=%q canonical=%q", strings.TrimSpace(req.ExecutionSpecDigest), canonicalDigest)},
			fmt.Errorf("execution_spec_digest mismatch (fail closed)")
	}
	effKey := transcode.DefaultTranscodeIdempotencyKey(trimmedID, req.IdempotencyKey)
	if err := transcode.ValidateTranscodeIdempotencyKey(effKey); err != nil {
		return SubmitResponse{ID: trimmedID, Error: err.Error()}, err
	}

	// Shared capacity lock serializes slot accounting, strong-idempotency
	// scans, and scheduler passes across all benchmark and transcode jobs.
	// Lock order everywhere is capLock -> jobLock.
	capLock, err := acquireCapacityLock(cleanStateDir)
	if err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("acquiring capacity lock: %v", err)}, err
	}
	defer capLock.Unlock()

	// Strong idempotency: same key + same digest => existing job (200, reused,
	// never requeue/respawn); same key + different digest => deterministic
	// 409 conflict. The scan is durable (job.json source of truth). Any scan
	// failure is definitive (fail closed): never create a duplicate.
	match, merr := w.findJobByIdempotencyKeyLocked(effKey)
	if merr != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("scanning durable idempotency index: %v", merr)}, merr
	}
	if match != nil {
		// Serialize with Cancel (which holds only the per-job lock) using
		// lock order capLock -> jobLock. The scan above may be stale, so
		// reload under the matched job's lock before any decision/mutation.
		matchDir := filepath.Join(cleanStateDir, match.ID)
		matchLock, err := acquireJobLock(matchDir)
		if err != nil {
			return SubmitResponse{ID: match.ID, Error: fmt.Sprintf("acquiring job lock for idempotent job %q: %v", match.ID, err)}, err
		}
		matchUnlocked := false
		unlockMatch := func() {
			if !matchUnlocked {
				matchUnlocked = true
				matchLock.Unlock()
			}
		}
		defer unlockMatch()

		matchFile := filepath.Join(cleanStateDir, match.ID, "job.json")
		fresh, ferr := LoadJob(matchFile)
		if ferr != nil {
			return SubmitResponse{ID: match.ID, Error: fmt.Sprintf("reloading durable job %q under lock (fail closed): %v", match.ID, ferr)}, ferr
		}
		if fresh == nil || fresh.ID != match.ID {
			ierr := fmt.Errorf("inconsistent durable job record %s (fail closed): id mismatch", matchFile)
			return SubmitResponse{ID: match.ID, Error: ierr.Error()}, ierr
		}
		storedKey := strings.TrimSpace(fresh.IdempotencyKey)
		if storedKey == "" {
			if fresh.ID != effKey {
				ierr := fmt.Errorf("idempotency index inconsistent for job %q (fail closed): legacy key mismatch", fresh.ID)
				return SubmitResponse{ID: fresh.ID, Error: ierr.Error()}, ierr
			}
		} else if storedKey != effKey {
			ierr := fmt.Errorf("idempotency index inconsistent for job %q (fail closed): key mismatch", fresh.ID)
			return SubmitResponse{ID: fresh.ID, Error: ierr.Error()}, ierr
		}
		match = fresh
		persistedDigest := match.ExecutionSpecDigest
		var persistedPlan *transcode.Plan
		if persistedDigest == "" {
			// Legacy record: recompute from the PERSISTED spec, never adopt
			// the new request's digest blindly. Compute effective resolved
			// plan + digest without mutation; compare first, backfill only
			// after equality is proven.
			pd, pp, perr := w.persistedExecutionDigest(match)
			if perr != nil {
				return SubmitResponse{ID: match.ID, Status: match.Status, CandidatePath: match.Candidate,
					Error: fmt.Sprintf("recomputing persisted execution spec digest for job %q: %v", match.ID, perr)}, perr
			}
			persistedDigest = pd
			persistedPlan = pp
		}
		if persistedDigest != canonicalDigest {
			ce := &IdempotencyConflictError{Key: effKey, ExistingJobID: match.ID, ExistingDigest: persistedDigest, RequestedDigest: canonicalDigest}
			return SubmitResponse{ID: match.ID, Status: match.Status, CandidatePath: match.Candidate, Error: ce.Error()}, ce
		}
		if match.ExecutionSpecDigest == "" || match.IdempotencyKey == "" {
			match.ExecutionSpecDigest = persistedDigest
			if match.IdempotencyKey == "" {
				match.IdempotencyKey = effKey
			}
			if persistedPlan != nil {
				match.Plan = persistedPlan
			}
			if serr := SaveJobAtomic(matchFile, match); serr != nil {
				berr := fmt.Errorf("persisting idempotency backfill failed for job %q: %w", match.ID, serr)
				return SubmitResponse{ID: match.ID, Status: match.Status, CandidatePath: match.Candidate, Error: berr.Error()}, berr
			}
		}
		// Reconcile liveness without respawning: running with a dead process
		// becomes failed; queued with a stale PID is reset to schedulable
		// queued (PID 0) so a restart can schedule it.
		if match.Status == "running" || match.Status == "queued" {
			if match.PID > 0 && !w.jobAlive(match) {
				if match.Status == "running" {
					match.Status = "failed"
					match.FinishedAt = time.Now().UTC()
					match.Error = "process terminated unexpectedly"
					match.FailureClassification = "runner_killed"
					_ = SaveJobAtomic(matchFile, match)
				} else {
					match.PID = 0
					match.ProcessStartTime = ""
					_ = SaveJobAtomic(matchFile, match)
				}
			}
		}
		// Opportunistically schedule a still-queued match (single spawn at
		// most; the scheduler holds capLock+jobLock so concurrent same-key
		// submits yield one durable job and one spawn). Release the matched
		// job lock first: the scheduler re-acquires per-job locks and flock
		// is non-reentrant. capLock stays held.
		if match.Status == "queued" {
			unlockMatch()
			_ = w.scheduleQueuedLocked(ctx, selfExe, configPath)
			if refreshed, rerr := LoadJob(matchFile); rerr == nil && refreshed != nil {
				match = refreshed
			}
		}
		return SubmitResponse{
			ID: match.ID, Status: match.Status, CandidatePath: match.Candidate,
			Plan: match.Plan, IdempotencyKey: match.IdempotencyKey,
			ExecutionSpecDigest: match.ExecutionSpecDigest, Reused: true,
		}, nil
	}

	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("acquiring job lock: %v", err)}, err
	}
	// flock is non-reentrant across separate open() file descriptions: the
	// scheduler below re-acquires this same job lock, so it must be released
	// first. capLock stays held throughout, preserving capLock -> jobLock
	// ordering and serializing concurrent idempotency scans.
	unlocked := false
	unlockJob := func() {
		if !unlocked {
			unlocked = true
			jobLock.Unlock()
		}
	}
	defer unlockJob()

	benchFile := filepath.Join(jobDir, "benchmark.json")
	if _, err := os.Stat(benchFile); err == nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("job ID collision: %q already exists as a benchmark job (fail closed)", trimmedID)},
			fmt.Errorf("job ID collision with benchmark job")
	}

	jobFile := filepath.Join(jobDir, "job.json")

	// Legacy same-ID handling (distinct from strong idempotency when callers
	// use explicit keys): a live queued/running record is reused; a dead
	// running record becomes failed; completed/failed/cancelled records are
	// returned as-is when the digest matches, otherwise the key scan above
	// already produced a conflict for same-key callers. A same-ID record
	// carrying a different key/digest fails closed as a conflict.
	if existing, err := LoadJob(jobFile); err == nil && existing != nil {
		if existing.IdempotencyKey != "" && existing.IdempotencyKey != effKey {
			ce := &IdempotencyConflictError{Key: effKey, ExistingJobID: existing.ID, ExistingDigest: existing.ExecutionSpecDigest, RequestedDigest: canonicalDigest}
			return SubmitResponse{ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate, Error: ce.Error()}, ce
		}
		// Fail closed on legacy records: recompute the digest from the
		// PERSISTED spec before comparing; never adopt the new digest
		// blindly. Uncomputable persisted specs are definitive errors.
		// Compute effective resolved plan + digest without mutation; compare
		// first, backfill only after equality is proven.
		persistedDigest := existing.ExecutionSpecDigest
		var persistedPlan *transcode.Plan
		if persistedDigest == "" {
			pd, pp, perr := w.persistedExecutionDigest(existing)
			if perr != nil {
				return SubmitResponse{ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate,
					Error: fmt.Sprintf("recomputing persisted execution spec digest for job %q: %v", existing.ID, perr)}, perr
			}
			persistedDigest = pd
			persistedPlan = pp
		}
		if persistedDigest != canonicalDigest {
			ce := &IdempotencyConflictError{Key: effKey, ExistingJobID: existing.ID, ExistingDigest: persistedDigest, RequestedDigest: canonicalDigest}
			return SubmitResponse{ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate, Error: ce.Error()}, ce
		}
		if existing.ExecutionSpecDigest == "" || existing.IdempotencyKey == "" {
			existing.ExecutionSpecDigest = persistedDigest
			if existing.IdempotencyKey == "" {
				existing.IdempotencyKey = effKey
			}
			if persistedPlan != nil {
				existing.Plan = persistedPlan
			}
			if serr := SaveJobAtomic(jobFile, existing); serr != nil {
				berr := fmt.Errorf("persisting idempotency backfill failed for job %q: %w", existing.ID, serr)
				return SubmitResponse{ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate, Error: berr.Error()}, berr
			}
		}
		if existing.Status == "running" || existing.Status == "queued" {
			if existing.PID > 0 && w.jobAlive(existing) {
				return SubmitResponse{
					ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate,
					Plan: existing.Plan, IdempotencyKey: existing.IdempotencyKey,
					ExecutionSpecDigest: existing.ExecutionSpecDigest, Reused: true,
				}, nil
			}
			if existing.PID > 0 && !w.jobAlive(existing) {
				if existing.Status == "running" {
					existing.Status = "failed"
					existing.FinishedAt = time.Now().UTC()
					existing.Error = "process terminated unexpectedly"
					existing.FailureClassification = "runner_killed"
					_ = SaveJobAtomic(jobFile, existing)
					return SubmitResponse{
						ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate,
						Plan: existing.Plan, IdempotencyKey: existing.IdempotencyKey,
						ExecutionSpecDigest: existing.ExecutionSpecDigest, Reused: true,
					}, nil
				}
				// Queued with a stale PID: fall through and re-persist below
				// (same job ID, same spec) rather than failing.
			} else if existing.PID == 0 && existing.Status == "queued" {
				// Durable queued job resubmitted (e.g. after restart): reuse
				// and let the scheduler start it; never double-persist.
				// Release the job lock first: the scheduler re-acquires it
				// (flock is non-reentrant; holding it would self-deadlock).
				unlockJob()
				_ = w.scheduleQueuedLocked(ctx, selfExe, configPath)
				if refreshed, rerr := LoadJob(jobFile); rerr == nil && refreshed != nil {
					existing = refreshed
				}
				return SubmitResponse{
					ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate,
					Plan: existing.Plan, IdempotencyKey: existing.IdempotencyKey,
					ExecutionSpecDigest: existing.ExecutionSpecDigest, Reused: true,
				}, nil
			}
		} else if existing.Status == "completed" || existing.Status == "failed" || existing.Status == "cancelled" {
			// Key/digest already backfilled above on spec match; reuse as-is.
			return SubmitResponse{
				ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate,
				Plan: existing.Plan, IdempotencyKey: existing.IdempotencyKey,
				ExecutionSpecDigest: existing.ExecutionSpecDigest, Reused: true,
			}, nil
		}
	}

	// Authoritative durable queue: persist as queued even when all slots are
	// occupied. Capacity exhaustion is NOT "worker busy" for transcode submit.
	// Ensure job directory and candidate directory exist
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("creating job directory: %v", err)}, err
	}
	if err := os.MkdirAll(filepath.Dir(cleanCandidate), 0755); err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("creating candidate directory: %v", err)}, err
	}

	job := &JobRecord{
		ID:                  trimmedID,
		Status:              "queued",
		Source:              cleanSource,
		Candidate:           cleanCandidate,
		Profile:             profile,
		Plan:                plan,
		IdempotencyKey:      effKey,
		ExecutionSpecDigest: canonicalDigest,
		CreatedAt:           time.Now().UTC(),
		Attempt:             1,
		RetryCount:          0,
	}
	if plan != nil && len(plan.AppliedFallbacks) > 0 {
		job.AppliedFallbacks = append([]string(nil), plan.AppliedFallbacks...)
		job.FallbackCount = len(plan.AppliedFallbacks)
	}

	if err := SaveJobAtomic(jobFile, job); err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("saving initial job state: %v", err)}, err
	}

	// Scheduler: start persisted queued jobs (including this one) while slots
	// are free, never exceeding max_parallel_jobs and never double-spawning.
	// Release the job lock first: the scheduler re-acquires per-job locks
	// (flock is non-reentrant; holding it would self-deadlock). capLock stays
	// held, so no concurrent submit can interleave.
	unlockJob()
	_ = w.scheduleQueuedLocked(ctx, selfExe, configPath)
	if refreshed, rerr := LoadJob(jobFile); rerr == nil && refreshed != nil {
		job = refreshed
	}

	return SubmitResponse{
		ID: trimmedID, Status: job.Status, CandidatePath: cleanCandidate, Plan: plan,
		IdempotencyKey: effKey, ExecutionSpecDigest: canonicalDigest,
	}, nil
}

// spawnTranscodeProcess spawns the detached runner, using the injected test
// stub when present.
func (w *Worker) spawnTranscodeProcess(selfExe, configPath, jobID string) (int, string, error) {
	if w.spawnTranscode != nil {
		return w.spawnTranscode(selfExe, configPath, jobID)
	}
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, "_internal_run", jobID)

	cmd := exec.Command(selfExe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Independent session/pgid so it survives SSH disconnect
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	pid := cmd.Process.Pid
	_, lstart, _ := GetProcessIdentity(pid)
	return pid, lstart, nil
}

// killTranscodeProcess best-efforts termination of a spawned runner whose
// identity persistence failed (fail closed: never orphan without identity).
func killTranscodeProcess(pid int) {
	if pid <= 1 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
	}
}

// persistedExecutionDigest recomputes the canonical digest for a persisted
// record's OWN spec through the SAME worker resolution path Submit uses
// (ResolveWorkerPlan over the record's Profile/Plan). Legacy profile-only
// records (nil Plan) resolve to the same default plan a fresh identical
// submit resolves to, so unchanged resubmits backfill/reuse instead of
// falsely conflicting. Changed candidate/profile/resolved-plan still
// conflicts. Unresolvable persisted specs are definitive errors (fail
// closed); the new request's digest is never adopted blindly.
//
// It returns both the canonical digest and the resolved effective plan
// (carrying its canonical PlanDigest) without mutating the record, so
// callers can backfill job.json consistently after equality is proven.
func (w *Worker) persistedExecutionDigest(rec *JobRecord) (string, *transcode.Plan, error) {
	if rec == nil {
		return "", nil, fmt.Errorf("nil job record for execution spec digest (fail closed)")
	}
	resolved, err := ResolveWorkerPlan(rec.Profile, rec.Plan)
	if err != nil {
		return "", nil, fmt.Errorf("resolving persisted execution spec for job %q: %w", rec.ID, err)
	}
	d, err := transcode.DigestTranscodeExecutionSpec(rec.Source, rec.Candidate, rec.Profile, resolved)
	if err != nil {
		return "", nil, err
	}
	return d, resolved, nil
}

// findJobByIdempotencyKeyLocked scans durable job.json records for a matching
// idempotency key. Callers must hold the capacity lock. Directories without
// job.json (benchmark-only or empty dirs) are skipped; a present but
// unloadable or inconsistent job.json is a definitive error (fail closed) so
// strong idempotency can never duplicate under corrupted state.
func (w *Worker) findJobByIdempotencyKeyLocked(key string) (*JobRecord, error) {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		jobPath := filepath.Join(w.cfg.StateDir, name, "job.json")
		if _, statErr := os.Stat(jobPath); statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, fmt.Errorf("statting durable job record %s (fail closed): %w", jobPath, statErr)
		}
		job, err := LoadJob(jobPath)
		if err != nil {
			return nil, fmt.Errorf("loading durable job record %s (fail closed): %w", jobPath, err)
		}
		if job == nil || job.ID != name {
			return nil, fmt.Errorf("inconsistent durable job record %s (fail closed): id mismatch", jobPath)
		}
		stored := strings.TrimSpace(job.IdempotencyKey)
		if stored == "" {
			// Backfill compatibility: legacy records without a key match
			// their own job ID.
			if name != key {
				continue
			}
		} else if stored != key {
			continue
		}
		cp := *job
		return &cp, nil
	}
	return nil, nil
}

// listQueuedJobsLocked returns persisted queued jobs in durable FIFO order
// (CreatedAt, then ID). Callers must hold the capacity lock.
func (w *Worker) listQueuedJobsLocked() []*JobRecord {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return nil
	}
	var out []*JobRecord
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		jobPath := filepath.Join(w.cfg.StateDir, name, "job.json")
		job, err := LoadJob(jobPath)
		if err != nil || job == nil || job.ID != name || job.Status != "queued" {
			continue
		}
		cp := *job
		out = append(out, &cp)
	}
	sortQueuedJobs(out)
	return out
}

func sortQueuedJobs(jobs []*JobRecord) {
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
		}
		return jobs[i].ID < jobs[j].ID
	})
}

// runningCountLocked counts live transcode running slots plus live benchmark
// slots (one global ceiling). Queued-but-unspawned jobs (PID 0) do not
// consume slots. Callers must hold the capacity lock.
func (w *Worker) runningCountLocked(excludeID string) (int, error) {
	return w.countActiveJobs(excludeID)
}

// scheduleQueuedLocked starts persisted queued jobs FIFO while global slots
// are free. It never exceeds max_parallel_jobs (transcodes + benchmarks in
// aggregate) and never double-spawns: each spawn happens under capLock +
// per-job lock after re-verifying status==queued and slot availability.
// Cancelled jobs are skipped and never spawn. Returns the number started.
func (w *Worker) scheduleQueuedLocked(ctx context.Context, selfExe, configPath string) int {
	_ = ctx
	maxJobs := w.cfg.MaxParallelJobs
	if maxJobs <= 0 {
		maxJobs = 1
	}
	started := 0
	for {
		active, err := w.runningCountLocked("")
		if err != nil || active >= maxJobs {
			return started
		}
		queued := w.listQueuedJobsLocked()
		if len(queued) == 0 {
			return started
		}
		progressed := false
		for _, q := range queued {
			if active >= maxJobs {
				return started
			}
			jobDir := filepath.Join(w.cfg.StateDir, q.ID)
			jobLock, err := acquireJobLock(jobDir)
			if err != nil {
				continue
			}
			func() {
				defer jobLock.Unlock()
				jobFile := filepath.Join(jobDir, "job.json")
				latest, err := LoadJob(jobFile)
				if err != nil || latest == nil || latest.Status != "queued" {
					return // cancelled/terminal/running: never spawn
				}
				if latest.PID > 0 && w.jobAlive(latest) {
					return // already spawned: never double-spawn
				}
				if latest.PID > 0 && !w.jobAlive(latest) {
					// Stale PID from a crashed spawner: reset to schedulable.
					latest.PID = 0
					latest.ProcessStartTime = ""
					_ = SaveJobAtomic(jobFile, latest)
				}
				// Re-check capacity under both locks before spawning.
				cur, err := w.runningCountLocked("")
				if err != nil || cur >= maxJobs {
					active = cur
					return
				}
				pid, lstart, err := w.spawnTranscodeProcess(selfExe, configPath, latest.ID)
				if err != nil {
					latest.Status = "failed"
					latest.Error = fmt.Sprintf("spawning worker process: %v", err)
					latest.FinishedAt = time.Now().UTC()
					_ = SaveJobAtomic(jobFile, latest)
					return
				}
				if w.afterSpawnHook != nil {
					w.afterSpawnHook(jobDir, pid)
				}
				latest.PID = pid
				latest.ProcessStartTime = lstart
				if err := SaveJobAtomic(jobFile, latest); err != nil {
					killTranscodeProcess(pid)
					latest.Status = "failed"
					latest.Error = fmt.Sprintf("persisting transcode process identity: %v", err)
					latest.FinishedAt = time.Now().UTC()
					latest.PID = 0
					latest.ProcessStartTime = ""
					_ = SaveJobAtomic(jobFile, latest)
					return
				}
				active = cur + 1
				started++
				progressed = true
			}()
		}
		if !progressed {
			return started
		}
	}
}

// ScheduleQueued is the public scheduler entry: discover persisted queued
// jobs and start them as global slots open. Safe for daemon/worker startup:
// queued jobs (including PID-0 jobs persisted before a restart) become
// schedulable; running jobs are left untouched (phase 5 reconciliation is
// deliberately out of scope).
func (w *Worker) ScheduleQueued(ctx context.Context, selfExe, configPath string) (int, error) {
	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	capLock, err := acquireCapacityLock(cleanStateDir)
	if err != nil {
		return 0, err
	}
	defer capLock.Unlock()
	return w.scheduleQueuedLocked(ctx, selfExe, configPath), nil
}

func (w *Worker) countActiveJobs(excludeID string) (int, error) {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == excludeID || strings.HasPrefix(name, ".") {
			continue
		}
		entryDir := filepath.Join(w.cfg.StateDir, name)

		// 1. Check for active transcode job
		jobPath := filepath.Join(entryDir, "job.json")
		if fi, err := os.Stat(jobPath); err == nil && !fi.IsDir() {
			job, err := LoadJob(jobPath)
			if err == nil && job != nil && job.ID == name {
				if job.Status == "running" || job.Status == "queued" {
					if job.PID == 0 {
						// Durable queued but never spawned: schedulable, not
						// occupying a slot.
						continue
					}
					if w.jobAlive(job) {
						count++
						continue
					}
					if job.Status == "running" {
						// Clean stale crash or recycled PID
						job.Status = "failed"
						job.FinishedAt = time.Now().UTC()
						job.Error = "process terminated unexpectedly"
						job.FailureClassification = "runner_killed"
						_ = SaveJobAtomic(jobPath, job)
					} else {
						// Queued with a stale PID: treat as non-active without
						// persisting here. Ownership: scheduler and locked
						// Submit own queued stale PID normalization under
						// capLock + per-job lock. Persisting here (capLock
						// only) could resurrect a concurrently cancelled job.
						continue
					}
				}
			}
		}

		// 2. Check for active benchmark job sharing worker capacity
		benchPath := filepath.Join(entryDir, "benchmark.json")
		if fi, err := os.Stat(benchPath); err == nil && !fi.IsDir() {
			bench, err := LoadBenchmark(benchPath)
			if err == nil && bench != nil && bench.ID == name {
				if bench.Status == "running" || bench.Status == "queued" {
					if IsBenchmarkExecutionAlive(bench) {
						count++
					} else if bench.PID > 0 {
						bench.Status = "failed"
						bench.FinishedAt = time.Now().UTC()
						bench.Error = "process terminated unexpectedly"
						_ = SaveBenchmarkAtomic(benchPath, bench)
						_ = w.CleanBenchmarkSamples(name)
					}
				}
			}
		}
	}
	return count, nil
}

// InternalRun is invoked by the background decoupled process.
func (w *Worker) InternalRun(ctx context.Context, jobID string) error {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")

	job, err := LoadJob(jobFile)
	if err != nil {
		return fmt.Errorf("loading job %s: %w", jobID, err)
	}

	if job.Status != "queued" && job.Status != "running" {
		return nil
	}

	job.PID = os.Getpid()
	_, startTime, _ := GetProcessIdentity(job.PID)
	job.ProcessStartTime = startTime
	job.Status = "running"
	job.StartedAt = time.Now().UTC()
	_ = SaveJobAtomic(jobFile, job)

	// Ensure plan is resolved
	if job.Plan == nil {
		plan, err := ResolveWorkerPlan(job.Profile, nil)
		if err != nil {
			job.Status = "failed"
			job.FinishedAt = time.Now().UTC()
			job.ExitCode = 1
			job.Error = fmt.Sprintf("resolving plan: %v", err)
			_ = SaveJobAtomic(jobFile, job)
			return err
		}
		job.Plan = plan
	}

	// Probe source streams and duration with ffprobe
	streams, dur, probeErr := ProbeSourceStreams(ctx, w.ffprobePath, job.Source)
	if dur > 0 {
		job.DurationSec = dur
	}
	if probeErr != nil {
		job.Status = "failed"
		job.FinishedAt = time.Now().UTC()
		job.ExitCode = 1
		job.Error = fmt.Sprintf("probing source streams: %v", probeErr)
		_ = SaveJobAtomic(jobFile, job)
		return probeErr
	}

	// Build stream-by-stream execution plan
	execPlan, planErr := BuildExecutionPlan(job.Plan, streams, job.DurationSec)
	if planErr != nil {
		job.Status = "failed"
		job.FinishedAt = time.Now().UTC()
		job.ExitCode = 1
		job.Error = fmt.Sprintf("building execution plan: %v", planErr)
		_ = SaveJobAtomic(jobFile, job)
		return planErr
	}

	job.Conversions = execPlan.Conversions
	_ = SaveJobAtomic(jobFile, job)

	progressPath := filepath.Join(jobDir, "progress.txt")
	logPath := filepath.Join(jobDir, "ffmpeg.log")

	ffmpegErr := RunFFmpeg(ctx, w.ffmpegPath, execPlan, job, progressPath, logPath)

	// Reload in case cancel was called
	latestJob, loadErr := LoadJob(jobFile)
	if loadErr == nil && latestJob != nil && latestJob.Status == "cancelled" {
		return nil
	}

	if ffmpegErr == nil {
		job.Status = "completed"
		job.FinishedAt = time.Now().UTC()
		job.ExitCode = 0
		job.Error = ""
	} else {
		job.Status = "failed"
		job.FinishedAt = time.Now().UTC()
		job.ExitCode = 1
		job.Error = ffmpegErr.Error()
	}

	return SaveJobAtomic(jobFile, job)
}

// JobStatusResponse is returned by the status subcommand.
type JobStatusResponse struct {
	ID                    string                       `json:"id"`
	Status                string                       `json:"status"`
	Progress              float64                      `json:"progress"`
	FPS                   float64                      `json:"fps"`
	Speed                 float64                      `json:"speed"`
	CandidatePath         string                       `json:"candidate_path"`
	Error                 string                       `json:"error,omitempty"`
	Profile               string                       `json:"profile,omitempty"`
	RecipeVersion         string                       `json:"recipe_version,omitempty"`
	RecipeDigest          string                       `json:"recipe_digest,omitempty"`
	PlanDigest            string                       `json:"plan_digest,omitempty"`
	Container             string                       `json:"container,omitempty"`
	VideoCodec            string                       `json:"video_codec,omitempty"`
	Quality               int                          `json:"quality,omitempty"`
	VideoProfile          string                       `json:"video_profile,omitempty"`
	PixelFormat           string                       `json:"pixel_format,omitempty"`
	PrioritizeSpeed       *bool                        `json:"prioritize_speed,omitempty"`
	SpatialAQ             *bool                        `json:"spatial_aq,omitempty"`
	Realtime              *bool                        `json:"realtime,omitempty"`
	ExpectedBitDepth      int                          `json:"expected_bit_depth,omitempty"`
	Attempt               int                          `json:"attempt,omitempty"`
	RetryCount            int                          `json:"retry_count,omitempty"`
	FallbackCount         int                          `json:"fallback_count,omitempty"`
	AppliedFallbacks      []string                     `json:"applied_fallbacks,omitempty"`
	FailureClassification string                       `json:"failure_classification,omitempty"`
	Conversions           []transcode.ConversionRecord `json:"conversions,omitempty"`
}

// Status reads the current status of a job.
func (w *Worker) Status(ctx context.Context, jobID string) (JobStatusResponse, error) {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")

	job, err := LoadJob(jobFile)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, err
	}

	// Detect crashed process or recycled PID. Running with a dead process
	// becomes failed. Queued with a stale PID is reported as queued without
	// persisting here; ownership: scheduler and locked Submit own queued
	// stale PID normalization under capLock + per-job lock. Persisting here
	// (no jobLock) could resurrect a concurrently cancelled job. PID-0
	// queued jobs report queued correctly without liveness checks.
	if job.Status == "running" && job.PID > 0 {
		if !w.jobAlive(job) {
			job.Status = "failed"
			job.FinishedAt = time.Now().UTC()
			job.Error = "process terminated unexpectedly"
			job.FailureClassification = "runner_killed"
			_ = SaveJobAtomic(jobFile, job)
		}
	} else if job.Status == "queued" && job.PID > 0 {
		// Intentionally no durable rewrite: treat stale PID as schedulable
		// queued for reporting/counting; scheduler normalizes under lock.
	}

	progressPath := filepath.Join(jobDir, "progress.txt")
	metrics := ParseProgress(progressPath, job.DurationSec)

	if job.Status == "completed" {
		metrics.Progress = 100.0
	}

	var (
		container, videoCodec                   string
		quality                                 int
		recipeVersion, recipeDigest, planDigest string
		videoProfile, pixelFormat               string
		prioritizeSpeed, spatialAQ, realtime    *bool
		expectedBitDepth                        int
	)
	if job.Plan != nil {
		container = job.Plan.Container
		videoCodec = job.Plan.VideoCodec
		quality = job.Plan.Quality
		recipeVersion = job.Plan.RecipeVersion
		recipeDigest = job.Plan.RecipeDigest
		planDigest = job.Plan.PlanDigest
		videoProfile = job.Plan.VideoProfile
		pixelFormat = job.Plan.PixelFormat
		prioritizeSpeed = job.Plan.PrioritizeSpeed
		spatialAQ = job.Plan.SpatialAQ
		realtime = job.Plan.Realtime
		expectedBitDepth = job.Plan.ExpectedBitDepth
	}

	return JobStatusResponse{
		ID:                    job.ID,
		Status:                job.Status,
		Progress:              metrics.Progress,
		FPS:                   metrics.FPS,
		Speed:                 metrics.Speed,
		CandidatePath:         job.Candidate,
		Error:                 job.Error,
		Profile:               job.Profile,
		RecipeVersion:         recipeVersion,
		RecipeDigest:          recipeDigest,
		PlanDigest:            planDigest,
		Container:             container,
		VideoCodec:            videoCodec,
		Quality:               quality,
		VideoProfile:          videoProfile,
		PixelFormat:           pixelFormat,
		PrioritizeSpeed:       prioritizeSpeed,
		SpatialAQ:             spatialAQ,
		Realtime:              realtime,
		ExpectedBitDepth:      expectedBitDepth,
		Attempt:               job.Attempt,
		RetryCount:            job.RetryCount,
		FallbackCount:         job.FallbackCount,
		AppliedFallbacks:      job.AppliedFallbacks,
		FailureClassification: job.FailureClassification,
		Conversions:           job.Conversions,
	}, nil
}

// Cancel terminates a running job. Cancel on a durable queued (unspawned)
// job marks it cancelled under the per-job lock and guarantees the scheduler
// never spawns it (the scheduler only spawns status==queued under capLock +
// jobLock). Running cancel keeps existing signal semantics.
func (w *Worker) Cancel(ctx context.Context, jobID string) (JobStatusResponse, error) {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")

	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, err
	}
	defer jobLock.Unlock()

	job, err := LoadJob(jobFile)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, err
	}

	// Queued and never spawned: no process exists; mark cancelled so any
	// concurrent scheduler pass skips it.
	if job.Status == "queued" && job.PID <= 1 {
		job.Status = "cancelled"
		job.FinishedAt = time.Now().UTC()
		_ = SaveJobAtomic(jobFile, job)
		if job.Candidate != "" && job.Candidate != job.Source {
			_ = os.Remove(job.Candidate)
		}
		return JobStatusResponse{
			ID:            job.ID,
			Status:        "cancelled",
			CandidatePath: job.Candidate,
		}, nil
	}

	if job.PID > 1 && (job.Status == "running" || job.Status == "queued") {
		// Only signal if the running process truly matches this job (protects against PID recycling)
		if w.jobAlive(job) {
			// Attempt graceful SIGTERM
			_ = syscall.Kill(-job.PID, syscall.SIGTERM)
			_ = syscall.Kill(job.PID, syscall.SIGTERM)

			// Wait briefly, then force SIGKILL if still alive
			time.Sleep(100 * time.Millisecond)
			if w.jobAlive(job) {
				_ = syscall.Kill(-job.PID, syscall.SIGKILL)
				_ = syscall.Kill(job.PID, syscall.SIGKILL)
			}
		}
	}

	job.Status = "cancelled"
	job.FinishedAt = time.Now().UTC()
	_ = SaveJobAtomic(jobFile, job)

	// Clean up incomplete candidate if it exists, never touching source!
	if job.Candidate != "" && job.Candidate != job.Source {
		_ = os.Remove(job.Candidate)
	}

	return JobStatusResponse{
		ID:            job.ID,
		Status:        "cancelled",
		CandidatePath: job.Candidate,
	}, nil
}
