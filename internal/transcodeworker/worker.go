package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	}

	if cfg.MaxParallelJobs <= 0 {
		cfg.MaxParallelJobs = 1
	}
	if cfg.Quality <= 0 {
		cfg.Quality = 65
	}

	return cfg, nil
}

// Worker manages the local transcode execution on the node.
type Worker struct {
	cfg             *WorkerConfig
	ffmpegPath      string
	ffprobePath     string
	benchmarkRunner BenchmarkRunner
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
	ID            string          `json:"id"`
	SourcePath    string          `json:"source_path"`
	CandidatePath string          `json:"candidate_path"`
	Profile       string          `json:"profile"`
	Plan          *transcode.Plan `json:"plan,omitempty"`
}

// SubmitResponse defines the JSON output for the submit command.
type SubmitResponse struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	CandidatePath string          `json:"candidate_path,omitempty"`
	Plan          *transcode.Plan `json:"plan,omitempty"`
	Error         string          `json:"error,omitempty"`
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
	if strings.TrimSpace(req.ID) == "" {
		return SubmitResponse{Error: "missing request id"}, errors.New("missing request id")
	}
	if strings.TrimSpace(req.SourcePath) == "" {
		return SubmitResponse{ID: req.ID, Error: "missing source_path"}, errors.New("missing source_path")
	}
	if strings.TrimSpace(req.CandidatePath) == "" {
		return SubmitResponse{ID: req.ID, Error: "missing candidate_path"}, errors.New("missing candidate_path")
	}

	cleanSource := filepath.Clean(req.SourcePath)
	cleanCandidate := filepath.Clean(req.CandidatePath)

	// Reject candidate == source (FAIL CLOSED)
	if cleanSource == cleanCandidate {
		return SubmitResponse{ID: req.ID, Error: "candidate_path cannot equal source_path (fail closed)"},
			errors.New("candidate_path cannot equal source_path")
	}

	// Reject source outside allowed roots (FAIL CLOSED)
	if !IsPathWithinAllowedRoots(cleanSource, w.cfg.AllowedRoots) {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("source_path %q is outside allowed roots %v (fail closed)", cleanSource, w.cfg.AllowedRoots)},
			fmt.Errorf("source_path outside allowed roots: %s", cleanSource)
	}

	// Reject candidate outside allowed roots (FAIL CLOSED)
	if !IsPathWithinAllowedRoots(cleanCandidate, w.cfg.AllowedRoots) {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("candidate_path %q is outside allowed roots %v (fail closed)", cleanCandidate, w.cfg.AllowedRoots)},
			fmt.Errorf("candidate_path outside allowed roots: %s", cleanCandidate)
	}

	// Verify source exists and is not directory
	fi, err := os.Stat(cleanSource)
	if err != nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("source_path %q not accessible: %v", cleanSource, err)},
			fmt.Errorf("source_path not accessible: %w", err)
	}
	if fi.IsDir() {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("source_path %q is a directory", cleanSource)},
			fmt.Errorf("source_path %q is a directory", cleanSource)
	}

	jobDir := filepath.Join(w.cfg.StateDir, req.ID)
	benchFile := filepath.Join(jobDir, "benchmark.json")
	if _, err := os.Stat(benchFile); err == nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("job ID collision: %q already exists as a benchmark job (fail closed)", req.ID)},
			fmt.Errorf("job ID collision with benchmark job")
	}

	jobFile := filepath.Join(jobDir, "job.json")

	// Idempotency: if job already exists
	if existing, err := LoadJob(jobFile); err == nil && existing != nil {
		if existing.Status == "running" || existing.Status == "queued" {
			if IsProcessAlive(existing.PID) {
				return SubmitResponse{
					ID:            existing.ID,
					Status:        existing.Status,
					CandidatePath: existing.Candidate,
				}, nil
			}
			// Process died without updating job.json
			existing.Status = "failed"
			existing.FinishedAt = time.Now().UTC()
			existing.Error = "process terminated unexpectedly"
			_ = SaveJobAtomic(jobFile, existing)
		} else if existing.Status == "completed" {
			return SubmitResponse{
				ID:            existing.ID,
				Status:        "completed",
				CandidatePath: existing.Candidate,
			}, nil
		}
	}

	// Check concurrency / busy status
	activeJobs, err := w.countActiveJobs(req.ID)
	if err != nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("checking active jobs: %v", err)}, err
	}
	if activeJobs >= w.cfg.MaxParallelJobs {
		return SubmitResponse{
			ID:    req.ID,
			Error: fmt.Sprintf("worker busy: maximum parallel jobs (%d) reached", w.cfg.MaxParallelJobs),
		}, fmt.Errorf("worker busy: max parallel jobs reached")
	}

	profile := req.Profile
	if strings.TrimSpace(profile) == "" {
		profile = "hevc-vt"
	}

	plan, err := ResolveWorkerPlan(profile, req.Plan)
	if err != nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("invalid transcode profile or plan: %v", err)}, err
	}

	// Ensure job directory and candidate directory exist
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("creating job directory: %v", err)}, err
	}
	if err := os.MkdirAll(filepath.Dir(cleanCandidate), 0755); err != nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("creating candidate directory: %v", err)}, err
	}

	job := &JobRecord{
		ID:        req.ID,
		Status:    "queued",
		Source:    cleanSource,
		Candidate: cleanCandidate,
		Profile:   profile,
		Plan:      plan,
		CreatedAt: time.Now().UTC(),
	}

	if err := SaveJobAtomic(jobFile, job); err != nil {
		return SubmitResponse{ID: req.ID, Error: fmt.Sprintf("saving initial job state: %v", err)}, err
	}

	// Launch decoupled runner process
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, "_internal_run", req.ID)

	cmd := exec.Command(selfExe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Independent session/pgid so it survives SSH disconnect
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		job.Status = "failed"
		job.Error = fmt.Sprintf("spawning worker process: %v", err)
		job.FinishedAt = time.Now().UTC()
		_ = SaveJobAtomic(jobFile, job)
		return SubmitResponse{ID: req.ID, Error: job.Error}, err
	}

	return SubmitResponse{
		ID:            req.ID,
		Status:        "queued",
		CandidatePath: cleanCandidate,
		Plan:          plan,
	}, nil
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
		if !entry.IsDir() || entry.Name() == excludeID {
			continue
		}
		entryDir := filepath.Join(w.cfg.StateDir, entry.Name())
		jobPath := filepath.Join(entryDir, "job.json")
		job, err := LoadJob(jobPath)
		if err == nil {
			if job.Status == "running" || job.Status == "queued" {
				if IsJobProcessAlive(job) {
					count++
					continue
				} else if job.PID > 0 {
					// Clean stale crash or recycled PID
					job.Status = "failed"
					job.FinishedAt = time.Now().UTC()
					job.Error = "process terminated unexpectedly"
					_ = SaveJobAtomic(jobPath, job)
				}
			}
		}

		// Also check for active benchmark jobs sharing the worker capacity
		benchPath := filepath.Join(entryDir, "benchmark.json")
		bench, err := LoadBenchmark(benchPath)
		if err == nil {
			if bench.Status == "running" || bench.Status == "queued" {
				if IsBenchmarkProcessAlive(bench) {
					count++
				} else if bench.PID > 0 {
					bench.Status = "failed"
					bench.FinishedAt = time.Now().UTC()
					bench.Error = "process terminated unexpectedly"
					_ = SaveBenchmarkAtomic(benchPath, bench)
					_ = w.CleanBenchmarkSamples(entry.Name())
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
	ID               string                       `json:"id"`
	Status           string                       `json:"status"`
	Progress         float64                      `json:"progress"`
	FPS              float64                      `json:"fps"`
	Speed            float64                      `json:"speed"`
	CandidatePath    string                       `json:"candidate_path"`
	Error            string                       `json:"error,omitempty"`
	Profile          string                       `json:"profile,omitempty"`
	RecipeVersion    string                       `json:"recipe_version,omitempty"`
	RecipeDigest     string                       `json:"recipe_digest,omitempty"`
	PlanDigest       string                       `json:"plan_digest,omitempty"`
	Container        string                       `json:"container,omitempty"`
	VideoCodec       string                       `json:"video_codec,omitempty"`
	Quality          int                          `json:"quality,omitempty"`
	VideoProfile     string                       `json:"video_profile,omitempty"`
	PixelFormat      string                       `json:"pixel_format,omitempty"`
	PrioritizeSpeed  *bool                        `json:"prioritize_speed,omitempty"`
	SpatialAQ        *bool                        `json:"spatial_aq,omitempty"`
	Realtime         *bool                        `json:"realtime,omitempty"`
	ExpectedBitDepth int                          `json:"expected_bit_depth,omitempty"`
	Conversions      []transcode.ConversionRecord `json:"conversions,omitempty"`
}

// Status reads the current status of a job.
func (w *Worker) Status(ctx context.Context, jobID string) (JobStatusResponse, error) {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")

	job, err := LoadJob(jobFile)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, err
	}

	// Detect crashed process or recycled PID
	if (job.Status == "running" || job.Status == "queued") && job.PID > 0 {
		if !IsJobProcessAlive(job) {
			job.Status = "failed"
			job.FinishedAt = time.Now().UTC()
			job.Error = "process terminated unexpectedly"
			_ = SaveJobAtomic(jobFile, job)
		}
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
		ID:               job.ID,
		Status:           job.Status,
		Progress:         metrics.Progress,
		FPS:              metrics.FPS,
		Speed:            metrics.Speed,
		CandidatePath:    job.Candidate,
		Error:            job.Error,
		Profile:          job.Profile,
		RecipeVersion:    recipeVersion,
		RecipeDigest:     recipeDigest,
		PlanDigest:       planDigest,
		Container:        container,
		VideoCodec:       videoCodec,
		Quality:          quality,
		VideoProfile:     videoProfile,
		PixelFormat:      pixelFormat,
		PrioritizeSpeed:  prioritizeSpeed,
		SpatialAQ:        spatialAQ,
		Realtime:         realtime,
		ExpectedBitDepth: expectedBitDepth,
		Conversions:      job.Conversions,
	}, nil
}

// Cancel terminates a running job.
func (w *Worker) Cancel(ctx context.Context, jobID string) (JobStatusResponse, error) {
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")

	job, err := LoadJob(jobFile)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, err
	}

	if job.PID > 1 && (job.Status == "running" || job.Status == "queued") {
		// Only signal if the running process truly matches this job (protects against PID recycling)
		if IsJobProcessAlive(job) {
			// Attempt graceful SIGTERM
			_ = syscall.Kill(-job.PID, syscall.SIGTERM)
			_ = syscall.Kill(job.PID, syscall.SIGTERM)

			// Wait briefly, then force SIGKILL if still alive
			time.Sleep(100 * time.Millisecond)
			if IsJobProcessAlive(job) {
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
