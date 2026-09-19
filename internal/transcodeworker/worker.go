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

	"github.com/jakenesler/navigatorr/internal/smbdirect"
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

	// Operational storage settings (Phase 6B1). StagingPolicy is validated
	// fail-closed on config load; ExternalRoots defaults to a copy of
	// AllowedRoots when absent/empty; LocalWorkDir defaults to a deterministic
	// path beneath StateDir. None of these affect transcode.Plan or the
	// execution-spec digest.
	StagingPolicy StagingPolicy    `json:"staging_policy" yaml:"staging_policy"`
	ExternalRoots []string         `json:"external_roots" yaml:"external_roots"`
	LocalWorkDir  string           `json:"local_work_dir" yaml:"local_work_dir"`
	SMBDirect     smbdirect.Config `json:"smb_direct" yaml:"smb_direct"`

	// Shared source-cache settings (transcode I/O optimization). The cache lets
	// a benchmark and the subsequent full transcode for the SAME immutable
	// source share one NAS read. Conservative defaults preserve existing
	// behavior when unset: enabled, 20 GiB bound, 72h TTL. Setting
	// DisableSourceCache restores legacy per-job staging/downloads exactly.
	// None of these affect transcode.Plan or the execution-spec digest.
	DisableSourceCache  bool  `json:"disable_source_cache" yaml:"disable_source_cache"`
	SourceCacheMaxBytes int64 `json:"source_cache_max_bytes" yaml:"source_cache_max_bytes"`
	SourceCacheTTLHours int   `json:"source_cache_ttl_hours" yaml:"source_cache_ttl_hours"`
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
		StagingPolicy:   DefaultStagingPolicy(),
	}
}

// LoadWorkerConfig loads worker configuration from the given file or default path.
func LoadWorkerConfig(configPath string) (*WorkerConfig, error) {
	cfg := DefaultWorkerConfig()
	if configPath == "" {
		home, _ := os.UserHomeDir()
		configPath = filepath.Join(home, ".config", "navigatorr-transcode", "config.yaml")
	}

	if data, err := os.ReadFile(configPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading worker config %s: %w", configPath, err)
		}
	} else if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing worker config %s: %w", configPath, err)
	}

	if err := cfg.normalizeOperational(); err != nil {
		return nil, fmt.Errorf("parsing worker config %s: %w", configPath, err)
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

// normalizeOperational expands ~-prefixed paths, cleans roots, validates the
// staging policy (fail closed), and fills the operational defaults: ExternalRoots
// becomes a COPY of AllowedRoots when absent/empty, and LocalWorkDir becomes a
// deterministic path beneath StateDir when blank. The ExternalRoots copy is
// deliberately a distinct slice so later mutation of one never aliases the
// other.
func (cfg *WorkerConfig) normalizeOperational() error {
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if home != "" && strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}

	cfg.StateDir = expand(cfg.StateDir)
	cfg.HTTPTokenFile = expand(cfg.HTTPTokenFile)
	cfg.LocalWorkDir = expand(cfg.LocalWorkDir)
	cfg.SMBDirect.LocalRoot = expand(cfg.SMBDirect.LocalRoot)
	cfg.SMBDirect.PasswordFile = expand(cfg.SMBDirect.PasswordFile)
	for i, r := range cfg.AllowedRoots {
		cfg.AllowedRoots[i] = expand(r)
	}
	for i, r := range cfg.ExternalRoots {
		cfg.ExternalRoots[i] = expand(r)
	}

	policy := strings.TrimSpace(string(cfg.StagingPolicy))
	if policy == "" {
		cfg.StagingPolicy = DefaultStagingPolicy()
	} else {
		parsed, err := ParseStagingPolicy(policy)
		if err != nil {
			return err
		}
		cfg.StagingPolicy = parsed
	}

	cfg.AllowedRoots = cleanRootPaths(cfg.AllowedRoots)
	cfg.ExternalRoots = cleanRootPaths(cfg.ExternalRoots)

	if len(cfg.ExternalRoots) == 0 && len(cfg.AllowedRoots) > 0 {
		cfg.ExternalRoots = append([]string(nil), cfg.AllowedRoots...)
	}

	if strings.TrimSpace(cfg.LocalWorkDir) == "" && strings.TrimSpace(cfg.StateDir) != "" {
		cfg.LocalWorkDir = filepath.Join(filepath.Clean(cfg.StateDir), "_work")
	} else {
		cfg.LocalWorkDir = filepath.Clean(cfg.LocalWorkDir)
	}
	if cfg.SourceCacheMaxBytes < 0 {
		return fmt.Errorf("source_cache_max_bytes must not be negative (fail closed)")
	}
	if cfg.SourceCacheTTLHours < 0 {
		return fmt.Errorf("source_cache_ttl_hours must not be negative (fail closed)")
	}
	if err := cfg.SMBDirect.Normalize(cfg.AllowedRoots); err != nil {
		return err
	}

	return nil
}

// cleanRootPaths trims surrounding whitespace, drops blank entries, and
// filesystem-cleans each remaining root while preserving order.
func cleanRootPaths(roots []string) []string {
	if len(roots) == 0 {
		return roots
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		out = append(out, filepath.Clean(r))
	}
	return out
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

	// Phase 6B2 operational execution seams. Nil selects the production
	// implementations (RunFFmpegPaths / ProbeSourceStreams / FinalizeOutputAtomic).
	// Tests inject stubs so the state machine is deterministic without real
	// ffmpeg/ffprobe. They never change the semantic Source/Candidate or the
	// execution-spec digest.
	runFFmpeg             func(ctx context.Context, execPlan *ExecutionPlan, job *JobRecord, inputPath, outputPath, progressPath, logPath string) error
	probeSource           func(ctx context.Context, path string) ([]SourceStream, float64, error)
	probeDetails          func(ctx context.Context, path string) (SourceProbe, error)
	finalizeOutput        func(ctx context.Context, localCandidate, destination, jobID string) error
	afterEncodeCheckpoint func(jobDir string, job *JobRecord)
	// leaseManager is optional: when set, external staging sources and external
	// finalization destinations are driven healthy through the existing lease
	// primitive before mutation. Nil is a no-op ("where applicable").
	leaseManager *LeaseManager
	mediaStore   directMediaStore
}

type directMediaStore interface {
	Maps(string) bool
	Stat(context.Context, string) (os.FileInfo, error)
	DownloadAtomic(context.Context, string, string) error
	Publish(context.Context, string, string, string) error
	CheckRoot(context.Context) error
}

// SetRunFFmpeg injects a custom encoder runner (tests). Nil restores the
// production RunFFmpegPaths path.
func (w *Worker) SetRunFFmpeg(fn func(ctx context.Context, execPlan *ExecutionPlan, job *JobRecord, inputPath, outputPath, progressPath, logPath string) error) {
	w.runFFmpeg = fn
}

// SetProbeSource injects a custom stream probe (tests). Nil restores
// ProbeSourceStreams.
func (w *Worker) SetProbeSource(fn func(ctx context.Context, path string) ([]SourceStream, float64, error)) {
	w.probeSource = fn
}

// SetProbeDetails injects a custom detailed probe (tests). Nil falls back to
// the injected stream probe (with unknown chapter count) or the production
// ProbeSourceDetails.
func (w *Worker) SetProbeDetails(fn func(ctx context.Context, path string) (SourceProbe, error)) {
	w.probeDetails = fn
}

// SetFinalizeOutput injects a custom output finalizer (tests). Nil restores
// FinalizeOutputAtomic.
func (w *Worker) SetFinalizeOutput(fn func(ctx context.Context, localCandidate, destination, jobID string) error) {
	w.finalizeOutput = fn
}

// SetAfterEncodeCheckpoint injects a hook invoked after the EncodeComplete
// checkpoint is durably persisted and before finalization (tests). Nil disables
// the hook.
func (w *Worker) SetAfterEncodeCheckpoint(fn func(jobDir string, job *JobRecord)) {
	w.afterEncodeCheckpoint = fn
}

// SetLeaseManager injects the operational storage lease manager (tests). Nil
// disables external health/repair driving.
func (w *Worker) SetLeaseManager(m *LeaseManager) {
	w.leaseManager = m
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
	w := &Worker{
		cfg:         cfg,
		ffmpegPath:  ResolveToolPath(cfg.FFmpeg, "ffmpeg"),
		ffprobePath: ResolveToolPath(cfg.FFprobe, "ffprobe"),
	}
	if cfg.SMBDirect.Enabled {
		w.mediaStore = smbdirect.New(cfg.SMBDirect)
	}
	return w
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
		if w.mediaStore != nil && w.mediaStore.Maps(filepath.Join(root, ".navigatorr-root-check")) {
			if err := w.mediaStore.CheckRoot(ctx); err != nil {
				rs.Error = err.Error()
				res.OK = false
				errs = append(errs, fmt.Sprintf("allowed SMB root %s not accessible: %v", root, err))
			} else {
				rs.Exists = true
				rs.Readable = true
				rs.Writable = true
			}
			res.AllowedRoots = append(res.AllowedRoots, rs)
			continue
		}
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

func (w *Worker) statMedia(ctx context.Context, name string) (os.FileInfo, error) {
	if err := w.requireMediaBackend(name); err != nil {
		return nil, err
	}
	if w.mediaStore != nil && w.mediaStore.Maps(name) {
		return w.mediaStore.Stat(ctx, name)
	}
	return os.Stat(name)
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
	// SourceSHA256 is the coordinator preflight digest of the original source
	// bytes. Optional for legacy callers; carried into the shared source cache
	// identity so unchanged-content reuse is provable.
	SourceSHA256 string `json:"source_sha256,omitempty"`
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

// operationalMetadata is the operational storage metadata resolved for a NEW
// job. It is never fed into transcode.Plan or the execution-spec digest.
type operationalMetadata struct {
	StagingPolicy       StagingPolicy
	StagingState        StagingState
	EffectiveInputPath  string
	StagedInputPath     string
	LocalCandidatePath  string
	IntendedDestination string
	FinalizationState   FinalizationState
	PartialPath         string
}

// localWorkDir resolves the operational local work directory: the configured
// LocalWorkDir, or a deterministic "<StateDir>/_work" when blank. It never
// mutates the config.
func (w *Worker) localWorkDir() string {
	dir := strings.TrimSpace(w.cfg.LocalWorkDir)
	if dir == "" {
		dir = filepath.Join(filepath.Clean(w.cfg.StateDir), "_work")
	}
	return filepath.Clean(dir)
}

// operationalMetadataFor resolves the operational storage metadata for a job
// from the worker config. The semantic source and candidate are treated as
// immutable originals: the returned IntendedDestination always equals the
// candidate and staging/finalization only add operational paths. A blank
// StagingPolicy falls back to the operational default ("auto") so records
// persisted before Phase 6B1 and directly-constructed configs keep working.
func (w *Worker) operationalMetadataFor(source, candidate, jobID string) (*operationalMetadata, error) {
	policy := w.cfg.StagingPolicy
	if strings.TrimSpace(string(policy)) == "" {
		policy = DefaultStagingPolicy()
	}
	parsed, err := ParseStagingPolicy(string(policy))
	if err != nil {
		return nil, err
	}

	meta := &operationalMetadata{
		StagingPolicy:       parsed,
		IntendedDestination: candidate,
	}

	stage, err := ShouldStageInput(parsed, source, w.cfg.ExternalRoots)
	if err != nil {
		return nil, err
	}
	// Direct SMB addresses are logical names, never mounted input paths. They
	// always require SSD staging irrespective of the legacy mount policy.
	if w.mediaStore != nil && w.mediaStore.Maps(source) {
		stage = true
	}
	if stage {
		staged, err := StagedInputPath(w.localWorkDir(), jobID, source)
		if err != nil {
			return nil, err
		}
		meta.StagingState = StagingStatePending
		meta.StagedInputPath = staged
		meta.EffectiveInputPath = staged
	} else {
		meta.StagingState = StagingStateNotRequired
		meta.EffectiveInputPath = source
	}

	if IsExternalPath(candidate, w.cfg.ExternalRoots) || (w.mediaStore != nil && w.mediaStore.Maps(candidate)) {
		localCandidate, err := LocalCandidatePath(w.localWorkDir(), jobID, candidate)
		if err != nil {
			return nil, err
		}
		meta.LocalCandidatePath = localCandidate
		meta.FinalizationState = FinalizationStatePending
		meta.PartialPath = PartialPathFor(candidate, jobID)
	} else {
		meta.LocalCandidatePath = candidate
		meta.FinalizationState = FinalizationStateNotRequired
	}

	return meta, nil
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
	// Optional content identity. A malformed digest fails closed rather than
	// being silently ignored, so callers cannot accidentally disable the
	// content check by sending garbage.
	sourceSHA, err := transcode.NormalizeSourceSHA256(req.SourceSHA256)
	if err != nil {
		return SubmitResponse{ID: trimmedID, Error: err.Error()}, err
	}
	req.SourceSHA256 = sourceSHA

	cleanSource := filepath.Clean(req.SourcePath)
	cleanCandidate := filepath.Clean(req.CandidatePath)
	for _, name := range []string{cleanSource, cleanCandidate} {
		if err := w.requireMediaBackend(name); err != nil {
			return SubmitResponse{ID: trimmedID, Error: err.Error()}, err
		}
	}

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
	fi, err := w.statMedia(ctx, cleanSource)
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
					if IsPostEncodeFinalizationPending(match) {
						// Phase 6B1: encoding finished but finalization is
						// still pending and the runner is gone. This must
						// never become runner_killed (nor be respawned): keep
						// it nonterminal running with the runner identity
						// cleared, exactly like reconcileJob.
						match.PID = 0
						match.ProcessStartTime = ""
						if serr := SaveJobAtomic(matchFile, match); serr != nil {
							perr := fmt.Errorf("persisting post-encode normalization for job %q: %w", match.ID, serr)
							return SubmitResponse{ID: match.ID, Status: match.Status, CandidatePath: match.Candidate, Error: perr.Error()}, perr
						}
					} else {
						match.Status = "failed"
						match.FinishedAt = time.Now().UTC()
						match.Error = "process terminated unexpectedly"
						match.FailureClassification = "runner_killed"
						_ = SaveJobAtomic(matchFile, match)
					}
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
					if IsPostEncodeFinalizationPending(existing) {
						// Phase 6B1: same post-encode protection as the
						// strong-idempotency path and reconcileJob. Never
						// runner_killed, never respawned; keep nonterminal
						// running with the runner identity cleared.
						existing.PID = 0
						existing.ProcessStartTime = ""
						if serr := SaveJobAtomic(jobFile, existing); serr != nil {
							perr := fmt.Errorf("persisting post-encode normalization for job %q: %w", existing.ID, serr)
							return SubmitResponse{ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate, Error: perr.Error()}, perr
						}
						return SubmitResponse{
							ID: existing.ID, Status: existing.Status, CandidatePath: existing.Candidate,
							Plan: existing.Plan, IdempotencyKey: existing.IdempotencyKey,
							ExecutionSpecDigest: existing.ExecutionSpecDigest, Reused: true,
						}, nil
					}
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
	// Ensure job directory exists. The semantic Source and Candidate are never
	// rewritten here.
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("creating job directory: %v", err)}, err
	}

	// Resolve operational storage metadata after the semantic digest and
	// idempotency decisions above, so it can never influence them. No file is
	// copied and no ffmpeg state changes: this is metadata only.
	meta, metaErr := w.operationalMetadataFor(cleanSource, cleanCandidate, trimmedID)
	if metaErr != nil {
		return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("resolving operational storage metadata: %v", metaErr)}, metaErr
	}

	// Phase 6 is local-output-first: an external destination must not require
	// (or create) its remote directory at Submit time. Only local destinations
	// keep the historical pre-create of the candidate directory.
	if meta.FinalizationState == FinalizationStateNotRequired {
		if err := os.MkdirAll(filepath.Dir(cleanCandidate), 0755); err != nil {
			return SubmitResponse{ID: trimmedID, Error: fmt.Sprintf("creating candidate directory: %v", err)}, err
		}
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
		SourceSHA256:        sourceSHA,
		CreatedAt:           time.Now().UTC(),
		Attempt:             1,
		RetryCount:          0,

		StagingPolicy:       string(meta.StagingPolicy),
		StagingState:        string(meta.StagingState),
		EffectiveInputPath:  meta.EffectiveInputPath,
		StagedInputPath:     meta.StagedInputPath,
		LocalCandidatePath:  meta.LocalCandidatePath,
		IntendedDestination: meta.IntendedDestination,
		FinalizationState:   string(meta.FinalizationState),
		PartialPath:         meta.PartialPath,
	}
	job.Phase = "queued"
	w.initializeStorageTelemetry(job)
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
						latest, err := w.reconcileStoppedRunner(entryDir, jobPath)
						if err != nil {
							return 0, err
						}
						if latest.Status == "running" && latest.PID > 0 {
							count++ // a runner claimed the job after the initial read
						}
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
						latest, err := w.reconcileStoppedBenchmark(entryDir, bench)
						if err != nil {
							return 0, err
						}
						if (latest.Status == "running" || latest.Status == "queued") && latest.PID > 0 {
							count++ // execution changed after the initial read; retain its slot
						}
					}
				}
			}
		}
	}
	return count, nil
}

// InternalRun is invoked by the background decoupled process.
//
// Phase 6B2: it resolves the stored operational metadata up front and fails
// closed (before any mutation) on malformed/unknown state. If encoding already
// completed (EncodeComplete), it resumes finalization only and never reruns
// ffmpeg or probes the source. Otherwise it drives staging -> encode ->
// checkpoint -> finalization -> completion.
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

	// Stored operational metadata is authoritative. Unknown nonblank states
	// and missing required operational paths fail closed here, before any
	// runner identity, staging, probe, or finalization mutation.
	resolved, err := resolveOperationalForExecution(job)
	if err != nil {
		return fmt.Errorf("job %s: %w", jobID, err)
	}
	if err := w.validateJobStorageBackend(job, resolved); err != nil {
		return w.failJobTerminal(jobDir, jobFile, job, err)
	}

	// Post-encode restart resume: encoding already finished, so claiming the
	// runner and finalizing is safe and must never re-probe or re-encode.
	postEncodeResume := isPostEncodeResume(job)

	// Claim the runner under the per-job lock. This serializes with the parent
	// spawner's post-spawn identity stamp in ResumePostEncode so a child can
	// never overwrite (nor be overwritten by) a stale parent snapshot.
	claimed, err := w.claimRunner(jobDir, jobFile, job)
	if err != nil {
		return err
	}
	if !claimed {
		// Cancellation wins over a concurrently claimed runner.
		return nil
	}
	stopHeartbeat := w.startJobHeartbeat(ctx, jobDir, jobFile, jobHeartbeatInterval)
	defer stopHeartbeat()

	if postEncodeResume || job.EncodeComplete {
		return w.finalizeOperational(ctx, jobDir, jobFile, job, resolved)
	}
	return w.executeOperational(ctx, jobDir, jobFile, job, resolved)
}

// claimRunner durably records this process as the job's runner under the
// per-job lock. Cancellation wins: a concurrently cancelled record is never
// overwritten and claimed=false is returned. On success the latest durable
// record is copied back into job.
func (w *Worker) claimRunner(jobDir, jobFile string, job *JobRecord) (bool, error) {
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return false, fmt.Errorf("acquiring job lock for runner claim: %w", err)
	}
	defer jobLock.Unlock()

	latest, err := LoadJob(jobFile)
	if err != nil {
		return false, fmt.Errorf("reloading job for runner claim: %w", err)
	}
	if latest == nil {
		return false, fmt.Errorf("reloading job for runner claim: nil record")
	}
	if latest.Status == "cancelled" {
		return false, nil
	}
	latest.PID = os.Getpid()
	_, startTime, _ := GetProcessIdentity(latest.PID)
	latest.ProcessStartTime = startTime
	latest.Status = "running"
	if latest.StartedAt.IsZero() {
		latest.StartedAt = time.Now().UTC()
	}
	latest.Phase = "preparing"
	if latest.EncodeComplete {
		latest.Phase = "publishing"
	}
	latest.WorkerHeartbeatAt = time.Now().UTC()
	if serr := SaveJobAtomic(jobFile, latest); serr != nil {
		return false, fmt.Errorf("persisting runner claim for job %q: %w", latest.ID, serr)
	}
	*job = *latest
	return true, nil
}

// terminalMarkerFromJob builds a TerminalMarker mirroring a terminal job.
func terminalMarkerFromJob(job *JobRecord) *TerminalMarker {
	if job == nil {
		return nil
	}
	return &TerminalMarker{
		JobID:                 job.ID,
		ExecutionSpecDigest:   job.ExecutionSpecDigest,
		Status:                job.Status,
		FinishedAt:            job.FinishedAt,
		ExitCode:              job.ExitCode,
		Error:                 job.Error,
		FailureClassification: job.FailureClassification,
	}
}

// terminalMarkerMatchesJob reports whether marker is valid terminal evidence
// for job: the job must carry a nonblank digest and the marker must agree on
// job ID and digest and itself be terminal.
func terminalMarkerMatchesJob(job *JobRecord, marker *TerminalMarker) bool {
	if job == nil || marker == nil {
		return false
	}
	if strings.TrimSpace(job.ExecutionSpecDigest) == "" {
		return false
	}
	if marker.JobID != job.ID {
		return false
	}
	if marker.ExecutionSpecDigest != job.ExecutionSpecDigest {
		return false
	}
	return isTerminalStatus(marker.Status)
}

// applyTerminalMarker copies marker terminal fields onto job.
func applyTerminalMarker(job *JobRecord, marker *TerminalMarker) {
	job.Status = marker.Status
	job.FinishedAt = marker.FinishedAt
	job.ExitCode = marker.ExitCode
	job.Error = marker.Error
	job.FailureClassification = marker.FailureClassification
}

// persistTerminalJob durably persists a terminal job transition under the
// per-job lock. Cancellation always wins: a concurrently cancelled latest
// record is never overwritten. For a normal terminal transition job.json is
// saved first and only then a matching terminal marker; a marker failure is
// returned and markers are never written before the job record.
func (w *Worker) persistTerminalJob(jobDir, jobFile string, job *JobRecord) error {
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return fmt.Errorf("acquiring job lock for terminal persistence: %w", err)
	}
	defer jobLock.Unlock()

	latest, err := LoadJob(jobFile)
	if err != nil {
		return fmt.Errorf("reloading job for terminal persistence: %w", err)
	}
	if latest == nil {
		return fmt.Errorf("reloading job for terminal persistence: nil record")
	}

	if latest.Status == "cancelled" {
		// Cancellation wins: job.json is never overwritten. Refresh a matching
		// cancelled marker only for records carrying a normal digest and
		// timestamp; legacy cancelled records stay untouched with no marker.
		if strings.TrimSpace(latest.ExecutionSpecDigest) != "" && !latest.FinishedAt.IsZero() {
			marker := terminalMarkerFromJob(latest)
			if err := SaveTerminalMarkerAtomic(filepath.Join(jobDir, "terminal.json"), marker); err != nil {
				return fmt.Errorf("saving cancelled terminal marker for job %q: %w", latest.ID, err)
			}
		}
		return nil
	}

	// A heartbeat may have advanced since the runner's last phase transition.
	// Preserve its measurements without replacing the terminal decision.
	if latest.LastProgressAt.After(job.LastProgressAt) {
		job.LastProgressAt = latest.LastProgressAt
		job.LastKnownProgress = latest.LastKnownProgress
	}
	job.WorkerHeartbeatAt = job.FinishedAt
	job.Phase = job.Status
	job.ProgressIsStale = false
	job.NextFinalizationAt = time.Time{}
	job.JobTelemetry = telemetryFor(job, time.Now().UTC())
	if err := SaveJobAtomic(jobFile, job); err != nil {
		return fmt.Errorf("saving terminal job %q: %w", job.ID, err)
	}
	marker := terminalMarkerFromJob(job)
	if err := SaveTerminalMarkerAtomic(filepath.Join(jobDir, "terminal.json"), marker); err != nil {
		return fmt.Errorf("saving terminal marker for job %q: %w", job.ID, err)
	}
	return nil
}

// ReconcileStartup reconciles persisted jobs after a worker restart. Running
// jobs are resolved from a valid matching terminal marker, else by runner
// liveness: a live runner stays running, a dead runner becomes failed
// (runner_killed) with a matching failed marker. Completed/failed/cancelled
// and queued jobs are left unchanged; unknown statuses fail closed.
func (w *Worker) ReconcileStartup(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	stateDir := filepath.Clean(w.cfg.StateDir)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading state dir %s for reconciliation: %w", stateDir, err)
	}

	for _, entry := range entries {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		jobDir := filepath.Join(stateDir, entry.Name())
		jobFile := filepath.Join(jobDir, "job.json")
		if _, err := os.Stat(jobFile); err != nil {
			if os.IsNotExist(err) {
				continue // benchmark-only or empty dir
			}
			return fmt.Errorf("stat job file %s: %w", jobFile, err)
		}
		if err := w.reconcileJob(jobDir, jobFile); err != nil {
			return err
		}
	}
	return nil
}

// reconcileJob reconciles a single persisted job under its per-job lock.
func (w *Worker) reconcileJob(jobDir, jobFile string) error {
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return fmt.Errorf("acquiring job lock for reconciliation: %w", err)
	}
	defer jobLock.Unlock()

	job, err := LoadJob(jobFile)
	if err != nil {
		return fmt.Errorf("loading job for reconciliation: %w", err)
	}
	if job == nil {
		return fmt.Errorf("reconciling job in %s: nil record (fail closed)", jobDir)
	}
	if strings.TrimSpace(job.ID) == "" {
		return fmt.Errorf("reconciling job in %s: blank job id (fail closed)", jobDir)
	}
	if job.ID != filepath.Base(jobDir) {
		return fmt.Errorf("reconciling job %s: id %q does not match directory (fail closed)", jobDir, job.ID)
	}

	switch job.Status {
	case "completed", "failed", "cancelled":
		// Terminal states are immutable: unknown operational fields must not
		// make reconciliation fail or mutate them.
		return nil
	case "queued", "running":
		// Nonterminal: strict operational-state validation below.
	default:
		return fmt.Errorf("reconciling job %s: unknown status %q (fail closed)", job.ID, job.Status)
	}

	// Malformed operational state on a nonterminal job fails closed rather
	// than being silently downgraded to runner_killed. Blank/zero is
	// preserved as legacy.
	if err := ValidateStagingState(job.StagingState); err != nil {
		return fmt.Errorf("reconciling job %s: %w", job.ID, err)
	}
	if err := ValidateFinalizationState(job.FinalizationState); err != nil {
		return fmt.Errorf("reconciling job %s: %w", job.ID, err)
	}

	if job.Status == "queued" {
		return nil
	}

	markerPath := filepath.Join(jobDir, "terminal.json")
	if marker, merr := LoadTerminalMarker(markerPath); merr == nil && terminalMarkerMatchesJob(job, marker) {
		applyTerminalMarker(job, marker)
		if err := SaveJobAtomic(jobFile, job); err != nil {
			return fmt.Errorf("persisting reconciled job %s: %w", job.ID, err)
		}
		return nil
	}

	// Missing/corrupt/invalid/mismatched markers are never completion
	// evidence: fall through to runner liveness.
	if w.jobAlive(job) {
		return nil
	}

	// Post-encode restart-safe normalization (Phase 6B1): encoding already
	// completed and the output still needs staging finalization, but no runner
	// is alive. Re-running encode would be wrong, so this is never
	// runner_killed and gets no terminal marker. We deliberately keep the
	// existing public status "running" (no new client-visible Status) with the
	// runner identity cleared to PID 0. InternalRun is not wired to resume
	// finalization in this PR; 6B2 will use LocalCandidatePath/PartialPath.
	if IsPostEncodeFinalizationPending(job) {
		job.PID = 0
		job.ProcessStartTime = ""
		if err := SaveJobAtomic(jobFile, job); err != nil {
			return fmt.Errorf("normalizing post-encode job %s for restart: %w", job.ID, err)
		}
		return nil
	}

	job.Status = "failed"
	job.FinishedAt = time.Now().UTC()
	job.Error = "process terminated unexpectedly"
	job.FailureClassification = "runner_killed"
	if err := SaveJobAtomic(jobFile, job); err != nil {
		return fmt.Errorf("persisting failed reconciliation for job %s: %w", job.ID, err)
	}
	marker := terminalMarkerFromJob(job)
	if err := SaveTerminalMarkerAtomic(markerPath, marker); err != nil {
		return fmt.Errorf("saving failed terminal marker for job %s: %w", job.ID, err)
	}
	return nil
}

// JobStatusResponse is returned by the status subcommand.
type JobStatusResponse struct {
	transcode.JobTelemetry
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
	// Accepted candidate identity (cheap post-publish verification metadata).
	CandidateSizeBytes int64  `json:"candidate_size_bytes,omitempty"`
	CandidateSHA256    string `json:"candidate_sha256,omitempty"`

	// Operational storage observability (Phase 6B1). Additive: never changes
	// the meaning of existing fields/statuses.
	StagingPolicy       string `json:"staging_policy,omitempty"`
	StagingState        string `json:"staging_state,omitempty"`
	EffectiveInputPath  string `json:"effective_input_path,omitempty"`
	StagedInputPath     string `json:"staged_input_path,omitempty"`
	LocalCandidatePath  string `json:"local_candidate_path,omitempty"`
	IntendedDestination string `json:"intended_destination,omitempty"`
	EncodeComplete      bool   `json:"encode_complete,omitempty"`
	FinalizationState   string `json:"finalization_state,omitempty"`
	PartialPath         string `json:"partial_path,omitempty"`
}

// Status reads the current status of a job.
func (w *Worker) Status(ctx context.Context, jobID string) (JobStatusResponse, error) {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")

	job, err := LoadJob(jobFile)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, err
	}

	// Re-read under lock before persisting liveness failures: the initial
	// snapshot may race the runner's completion or a user's cancellation.
	if job.Status == "running" && job.PID > 0 && !w.jobAlive(job) {
		job, err = w.reconcileStoppedRunner(jobDir, jobFile)
		if err != nil {
			return JobStatusResponse{}, err
		}
	}
	metrics := observeJobProgress(jobDir, job, time.Now().UTC())
	telemetry := telemetryFor(job, time.Now().UTC())
	telemetry.WorkerSlotsTotal, telemetry.WorkerSlotsUsed, telemetry.QueuePosition = w.slotSnapshot(jobID)

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
		JobTelemetry:          telemetry,
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
		CandidateSizeBytes:    job.CandidateSizeBytes,
		CandidateSHA256:       job.CandidateSHA256,

		StagingPolicy:       job.StagingPolicy,
		StagingState:        job.StagingState,
		EffectiveInputPath:  job.EffectiveInput(),
		StagedInputPath:     job.StagedInputPath,
		LocalCandidatePath:  job.LocalCandidate(),
		IntendedDestination: job.Destination(),
		EncodeComplete:      job.EncodeComplete,
		FinalizationState:   job.FinalizationState,
		PartialPath:         job.PartialPath,
	}, nil
}

// Cancel terminates a running job. Cancel on a durable queued (unspawned)
// job marks it cancelled under the per-job lock and guarantees the scheduler
// never spawns it (the scheduler only spawns status==queued under capLock +
// jobLock). Running cancel keeps existing signal semantics.
//
// Terminal immutability: completed/failed records are returned unchanged. An
// already-cancelled record is returned unchanged, but its cancelled terminal
// marker is (re)written for modern records carrying durable evidence (nonblank
// digest + finished_at); legacy blank-digest records stay untouched with no
// marker.
//
// For queued/running cancellation job.json is saved first and a persistence
// failure fails closed (no marker, no artifact cleanup); the cancelled terminal
// marker is written only after job.json is durable. Only worker-owned
// incomplete artifacts are removed: never the semantic source and never a
// finalized Phase 6 destination.
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

	switch job.Status {
	case "completed", "failed":
		// Terminal states are immutable: never rewrite, revive, or clean them.
		return JobStatusResponse{
			ID:            job.ID,
			Status:        job.Status,
			CandidatePath: job.Candidate,
			Error:         job.Error,
		}, nil
	case "cancelled":
		// Idempotent: the durable record is never rewritten. A cancelled marker
		// is ensured only when the record carries durable evidence.
		if err := w.ensureCancelledTerminalMarker(jobDir, job); err != nil {
			return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate, Error: err.Error()}, err
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
	job.Phase = "cancelled"
	job.ProgressIsStale = false
	job.NextFinalizationAt = time.Time{}

	// job.json is the durable source of truth and must be persisted (fail
	// closed) before any terminal marker is written.
	if err := SaveJobAtomic(jobFile, job); err != nil {
		return JobStatusResponse{ID: job.ID, Status: "failed", CandidatePath: job.Candidate, Error: err.Error()}, err
	}
	if err := w.ensureCancelledTerminalMarker(jobDir, job); err != nil {
		return JobStatusResponse{ID: job.ID, Status: "cancelled", CandidatePath: job.Candidate, Error: err.Error()}, err
	}

	w.cleanupCancelledArtifacts(job)

	return JobStatusResponse{
		ID:            job.ID,
		Status:        "cancelled",
		CandidatePath: job.Candidate,
	}, nil
}

// ensureCancelledTerminalMarker writes (or refreshes) the cancelled terminal
// marker for an already-durable cancelled record. Legacy records with a blank
// execution-spec digest or a missing finished_at cannot yield a valid marker
// and are deliberately left with no marker, preserving backward compatibility.
func (w *Worker) ensureCancelledTerminalMarker(jobDir string, job *JobRecord) error {
	if job == nil {
		return nil
	}
	if strings.TrimSpace(job.ExecutionSpecDigest) == "" || job.FinishedAt.IsZero() {
		return nil
	}
	if err := SaveTerminalMarkerAtomic(filepath.Join(jobDir, "terminal.json"), terminalMarkerFromJob(job)); err != nil {
		return fmt.Errorf("ensuring cancelled terminal marker for job %s: %w", job.ID, err)
	}
	return nil
}

// cleanupCancelledArtifacts removes only worker-owned incomplete artifacts of a
// newly-cancelled job. It never removes the semantic source. For Phase 6
// external destinations the encoder output is the operational local candidate
// and finalization stages through the job's exact own partial path; both are
// removed, while the atomically published destination is left untouched. For
// local-only and legacy jobs (finalization not_required or blank) the candidate
// IS the encoder output, so an incomplete candidate is removed as before.
func (w *Worker) cleanupCancelledArtifacts(job *JobRecord) {
	if job == nil {
		return
	}
	source := strings.TrimSpace(job.Source)
	candidate := strings.TrimSpace(job.Candidate)
	destination := strings.TrimSpace(job.Destination())

	if fs := FinalizationState(strings.TrimSpace(job.FinalizationState)); fs != "" && fs != FinalizationStateNotRequired {
		local := strings.TrimSpace(job.LocalCandidatePath)
		if local != "" && local != source && local != destination && !w.isCachePath(local) {
			_ = os.Remove(local)
		}
		partial := strings.TrimSpace(job.PartialPath)
		if partial == "" && destination != "" {
			partial = PartialPathFor(destination, job.ID)
		}
		if partial != "" && partial != source && partial != destination {
			_ = os.Remove(partial)
		}
		return
	}

	if candidate != "" && candidate != source {
		_ = os.Remove(candidate)
	}
}
