package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// BenchmarkSampleRef describes an extracted reference sample window.
type BenchmarkSampleRef struct {
	Index           int     `json:"index"`
	StartSeconds    float64 `json:"start_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	File            string  `json:"file"`
	SizeBytes       int64   `json:"size_bytes"`
}

// BenchmarkCandidateSampleResult describes one candidate encode for one sample window.
type BenchmarkCandidateSampleResult struct {
	CandidateID       string   `json:"candidate_id"`
	SampleIndex       int      `json:"sample_index"`
	File              string   `json:"file"`
	SizeBytes         int64    `json:"size_bytes"`
	EncodeDurationSec float64  `json:"encode_duration_sec"`
	Quality           int      `json:"quality"`
	VideoProfile      string   `json:"video_profile,omitempty"`
	PixelFormat       string   `json:"pixel_format,omitempty"`
	VMAF              *float64 `json:"vmaf,omitempty"`
	SSIM              *float64 `json:"ssim,omitempty"`
	MetricDurationSec float64  `json:"metric_duration_sec,omitempty"`
	Error             string   `json:"error,omitempty"`
}

// BenchmarkMetricSampleResult records perceptual quality metric measurements for one candidate sample.
type BenchmarkMetricSampleResult struct {
	CandidateID       string   `json:"candidate_id"`
	CandidateIndex    int      `json:"candidate_index"`
	SampleIndex       int      `json:"sample_index"`
	VMAF              *float64 `json:"vmaf,omitempty"`
	SSIM              *float64 `json:"ssim,omitempty"`
	VMAFDurationSec   float64  `json:"vmaf_duration_sec,omitempty"`
	SSIMDurationSec   float64  `json:"ssim_duration_sec,omitempty"`
	MetricDurationSec float64  `json:"metric_duration_sec"`
	Error             string   `json:"error,omitempty"`
}

// BenchmarkCandidateMetricAggregate records the aggregated metric result for one candidate.
type BenchmarkCandidateMetricAggregate struct {
	CandidateID    string                       `json:"candidate_id"`
	CandidateIndex int                          `json:"candidate_index"`
	MetricType     optimization.MetricType      `json:"metric_type"`
	Aggregate      optimization.MetricAggregate `json:"aggregate"`
}

// BenchmarkExecutionEvidence records all physical sample outcomes and metric measurements.
type BenchmarkExecutionEvidence struct {
	SourceVideoIndex  int                                 `json:"source_video_index"`
	SourceBitDepth    int                                 `json:"source_bit_depth"`
	SourcePixelFormat string                              `json:"source_pixel_format"`
	SourceResolution  string                              `json:"source_resolution"`
	ReferenceSamples  []BenchmarkSampleRef                `json:"reference_samples"`
	CandidateSamples  []BenchmarkCandidateSampleResult    `json:"candidate_samples"`
	MetricSamples     []BenchmarkMetricSampleResult       `json:"metric_samples,omitempty"`
	CandidateMetrics  []BenchmarkCandidateMetricAggregate `json:"candidate_metrics,omitempty"`
	Decision          *transcode.BenchmarkDecision        `json:"decision,omitempty"`
}

type benchmarkProgressReporter struct {
	mu             sync.Mutex
	w              *Worker
	record         *BenchmarkRecord
	totalUnits     int
	completedUnits int
	currentPhase   string
	lastProgress   float64
}

func newBenchmarkProgressReporter(w *Worker, record *BenchmarkRecord, totalUnits int) *benchmarkProgressReporter {
	return &benchmarkProgressReporter{
		w:          w,
		record:     record,
		totalUnits: totalUnits,
	}
}

func (p *benchmarkProgressReporter) StartUnit(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentPhase = phase
	now := time.Now().UTC()
	if p.record != nil {
		p.record.Phase = phase
		p.record.HeartbeatAt = now
	}
	if p.w != nil && p.record != nil && p.record.ID != "" && p.record.RunToken != "" {
		_ = p.w.UpdateBenchmarkProgress(p.record.ID, p.record.RunToken, p.lastProgress, phase)
	}
}

func (p *benchmarkProgressReporter) CompleteUnit() {
	p.ResolveUnits(1)
}

func (p *benchmarkProgressReporter) ResolveUnit() {
	p.ResolveUnits(1)
}

func (p *benchmarkProgressReporter) SkipUnits(n int) {
	p.ResolveUnits(n)
}

func (p *benchmarkProgressReporter) ResolveUnits(n int) {
	if p == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completedUnits += n
	pct := 0.0
	if p.totalUnits > 0 {
		pct = (float64(p.completedUnits) / float64(p.totalUnits)) * 100.0
	}
	if pct >= 100.0 {
		pct = 99.0
	}
	pct = math.Round(pct*10) / 10
	if pct >= 100.0 {
		pct = 99.0
	}
	if pct < p.lastProgress {
		pct = p.lastProgress
	}
	p.lastProgress = pct
	now := time.Now().UTC()
	if p.record != nil {
		p.record.Progress = pct
		p.record.HeartbeatAt = now
	}
	if p.w != nil && p.record != nil && p.record.ID != "" && p.record.RunToken != "" {
		_ = p.w.UpdateBenchmarkProgress(p.record.ID, p.record.RunToken, pct, p.currentPhase)
	}
}

func (p *benchmarkProgressReporter) TotalUnits() int {
	if p == nil {
		return 0
	}
	return p.totalUnits
}

func (p *benchmarkProgressReporter) CompletedUnits() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completedUnits
}

func (p *benchmarkProgressReporter) SetPhase(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentPhase = phase
	now := time.Now().UTC()
	if p.record != nil {
		p.record.Phase = phase
		p.record.HeartbeatAt = now
	}
	if p.w != nil && p.record != nil && p.record.ID != "" && p.record.RunToken != "" {
		_ = p.w.UpdateBenchmarkProgress(p.record.ID, p.record.RunToken, p.lastProgress, phase)
	}
}

type validatedCandidate struct {
	index       int
	candidate   transcode.BenchmarkCandidate
	plan        *transcode.Plan
	profile     string
	pixelFormat string
	bitDepth    int
	fileKey     string
}

// ProductionBenchmarkRunner implements BenchmarkRunner for Phase 4B, Phase 5, and Phase 6.
// It extracts lossless reference samples, encodes candidate samples using hevc_videotoolbox,
// computes perceptual quality metrics (VMAF/SSIM), and deterministically selects the winning candidate.
type ProductionBenchmarkRunner struct {
	metricsHook   func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error
	selectionHook func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error
}

// SetMetricsHook sets an optional metrics callback for Phase 5 integration before scratch cleanup.
func (r *ProductionBenchmarkRunner) SetMetricsHook(hook func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error) {
	r.metricsHook = hook
}

// SetSelectionHook sets an optional selection callback for Phase 6 integration before scratch cleanup.
func (r *ProductionBenchmarkRunner) SetSelectionHook(hook func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error) {
	r.selectionHook = hook
}

// RunBenchmark executes the Phase 4B sample extraction and candidate encoding pipeline:
// 1. Validates source stability (size and modtime snapshot).
// 2. Inspects source media with ffprobe model, enforcing single video stream, SDR-only, 4:2:0 chroma, and 8/10-bit support.
// 3. Probes worker capabilities and validates all candidate configurations upfront.
// 4. Sequentially extracts lossless reference samples for each sample window.
// 5. Sequentially encodes VideoToolbox candidate samples from reference samples.
// 6. Invokes optional metrics hook (Phase 5) before scratch cleanup.
// 7. Persists physical evidence and preserves partial work on failure.
func (r *ProductionBenchmarkRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	if record == nil {
		return errors.New("benchmark record cannot be nil (fail closed)")
	}
	if w == nil {
		return errors.New("worker instance cannot be nil (fail closed)")
	}

	// 1. Source stability snapshot before inspection
	sourceStat, err := os.Stat(record.Source)
	if err != nil {
		return fmt.Errorf("stat source media %s: %w", record.Source, err)
	}
	if !sourceStat.Mode().IsRegular() {
		return fmt.Errorf("source media %s is not a regular file (fail closed)", record.Source)
	}
	sourceInitialSize := sourceStat.Size()
	sourceInitialModTime := sourceStat.ModTime()

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, record.ID)
	samplesDir := filepath.Join(jobDir, "samples")

	if err := verifyChildPath(jobDir, samplesDir); err != nil {
		return fmt.Errorf("invalid samples directory: %w", err)
	}

	// Reject immediately if samplesDir itself is pre-existing as a symlink
	if fi, err := os.Lstat(samplesDir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("samples directory %s is a symlink (fail closed)", samplesDir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("samples path %s is not a directory (fail closed)", samplesDir)
		}
	}

	if err := os.MkdirAll(samplesDir, 0755); err != nil {
		return fmt.Errorf("creating samples directory: %w", err)
	}

	// Post-mkdir check on samplesDir
	if fi, err := os.Lstat(samplesDir); err != nil {
		return fmt.Errorf("stat samples directory: %w", err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("samples directory %s is a symlink (fail closed)", samplesDir)
	}

	// 2. Source inspection using ffprobe model
	if err := verifySourceUnchanged(record.Source, sourceInitialSize, sourceInitialModTime); err != nil {
		return err
	}

	rep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, record.Source)
	if err != nil {
		return fmt.Errorf("inspecting source media %s: %w", record.Source, err)
	}
	if !rep.Probed {
		return fmt.Errorf("source media %s could not be probed (fail closed)", record.Source)
	}
	if len(rep.Video) == 0 {
		return fmt.Errorf("source media %s has no video streams (fail closed)", record.Source)
	}
	if len(rep.Video) > 1 {
		return fmt.Errorf("source media %s has %d video streams; benchmark requires exactly 1 (fail closed)", record.Source, len(rep.Video))
	}
	sourceVideo := rep.Video[0]

	// HDR / Dolby Vision rejection: automatic benchmark optimization is strictly SDR-only
	if (rep.HDR != nil && rep.HDR.Present) || isHDRStreamDetailed(sourceVideo) {
		return errors.New("HDR/Dolby Vision source not eligible for automatic benchmark optimization (fail closed)")
	}

	// Source bit-depth validation
	sourceBitDepth := sourceVideo.BitDepth
	if sourceBitDepth != 8 && sourceBitDepth != 10 {
		return fmt.Errorf("unsupported source bit depth %d: only 8-bit and 10-bit sources are supported for benchmark", sourceBitDepth)
	}

	// Chroma subsampling gating: VideoToolbox candidates support standard limited-range 4:2:0 (yuv420p / nv12 / p010le / yuv420p10le).
	// Reject 4:4:4, 4:2:2, and full-range formats (e.g. yuvj420p) to prevent silent downsampling or range-conversion discrepancies.
	if !isSupportedSourceChroma(sourceVideo.PixelFormat, sourceBitDepth) {
		if strings.ToLower(strings.TrimSpace(sourceVideo.PixelFormat)) == "yuvj420p" {
			return fmt.Errorf("unsupported source pixel format %q (%d-bit): full-range yuvj420p is deferred until range-normalized metric pipeline support (fail closed)", sourceVideo.PixelFormat, sourceBitDepth)
		}
		return fmt.Errorf("unsupported source pixel format %q (%d-bit): automatic benchmark requires 4:2:0 chroma subsampling (e.g. yuv420p, nv12, yuv420p10le, p010le)", sourceVideo.PixelFormat, sourceBitDepth)
	}

	// 3. Capability probing and candidate validation upfront
	caps, err := ProbeVideoToolboxCapabilities(ctx, w.ffmpegPath)
	if err != nil {
		return fmt.Errorf("probing worker video capabilities: %w", err)
	}
	if !caps.Available {
		return errors.New("hevc_videotoolbox encoder is not available on worker (fail closed)")
	}

	validatedCandidates := make([]validatedCandidate, 0, len(record.Candidates))
	for candIdx, c := range record.Candidates {
		prof, pix, bd, err := resolveCandidateBitDepth(&c, sourceBitDepth)
		if err != nil {
			return err
		}
		plan := &transcode.Plan{
			VideoCodec:       videoToolboxEncoder,
			Quality:          c.Quality,
			VideoProfile:     prof,
			PixelFormat:      pix,
			ExpectedBitDepth: bd,
		}
		if err := ValidateVideoToolboxCapabilities(plan, caps); err != nil {
			return fmt.Errorf("candidate %q capability validation failed: %w", c.ID, err)
		}
		if _, err := BuildVideoEncoderArgs(plan); err != nil {
			return fmt.Errorf("candidate %q encoder args validation failed: %w", c.ID, err)
		}
		fileKey := candidateFileKey(candIdx, c.ID, c.Quality)
		validatedCandidates = append(validatedCandidates, validatedCandidate{
			index:       candIdx,
			candidate:   c,
			plan:        plan,
			profile:     prof,
			pixelFormat: pix,
			bitDepth:    bd,
			fileKey:     fileKey,
		})
	}

	// Initialize evidence and attach immediately so partial progress is preserved on failure
	resStr := fmt.Sprintf("%dx%d", sourceVideo.Width, sourceVideo.Height)
	evidence := &BenchmarkExecutionEvidence{
		SourceVideoIndex:  sourceVideo.Index,
		SourceBitDepth:    sourceBitDepth,
		SourcePixelFormat: sourceVideo.PixelFormat,
		SourceResolution:  resStr,
		ReferenceSamples:  make([]BenchmarkSampleRef, 0, len(record.Samples)),
		CandidateSamples:  make([]BenchmarkCandidateSampleResult, 0, len(record.Candidates)*len(record.Samples)),
	}
	record.Evidence = evidence

	// Resolve total work units for deterministic progress
	normMetric := strings.ToLower(strings.TrimSpace(record.Metric))
	if normMetric == "" {
		normMetric = "vmaf"
	}
	passesPerSample := 0
	if normMetric == "vmaf" || normMetric == "both" || normMetric == "vmaf+ssim" {
		passesPerSample++
	}
	if normMetric == "ssim" || normMetric == "both" || normMetric == "vmaf+ssim" {
		passesPerSample++
	}

	metricUnits := 0
	if r.metricsHook != nil {
		metricUnits = 1
	} else {
		metricUnits = len(validatedCandidates) * len(record.Samples) * passesPerSample
	}

	totalUnits := len(record.Samples) + (len(validatedCandidates) * len(record.Samples)) + metricUnits
	progressReporter := newBenchmarkProgressReporter(w, record, totalUnits)

	// 4. Extract reference samples sequentially
	for _, window := range record.Samples {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		progressReporter.StartUnit("extracting_samples")

		if err := verifySourceUnchanged(record.Source, sourceInitialSize, sourceInitialModTime); err != nil {
			return err
		}

		refFileName := fmt.Sprintf("ref_sample_%d.mkv", window.Index)
		refPath := filepath.Join(samplesDir, refFileName)
		if err := verifyChildPath(samplesDir, refPath); err != nil {
			return fmt.Errorf("invalid reference sample path: %w", err)
		}
		if err := prepareOutputFile(refPath); err != nil {
			return err
		}

		refArgs := BuildReferenceExtractionArgs(record.Source, refPath, sourceVideo.Index, window.StartSeconds, window.DurationSeconds)

		cmd := exec.CommandContext(ctx, w.ffmpegPath, refArgs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		cmd.WaitDelay = 2 * time.Second

		stderrBuf := newBoundedBuffer(16 * 1024)
		cmd.Stderr = stderrBuf
		cmd.Stdout = nil

		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("extracting reference sample %d failed: %w: %s",
				window.Index, err, boundedStderr(stderrBuf, 1024))
		}

		if err := verifySourceUnchanged(record.Source, sourceInitialSize, sourceInitialModTime); err != nil {
			return err
		}

		fi, err := os.Stat(refPath)
		if err != nil || fi.Size() == 0 {
			return fmt.Errorf("reference sample %d produced empty or missing file at %s", window.Index, refPath)
		}

		evidence.ReferenceSamples = append(evidence.ReferenceSamples, BenchmarkSampleRef{
			Index:           window.Index,
			StartSeconds:    window.StartSeconds,
			DurationSeconds: window.DurationSeconds,
			File:            filepath.Base(refPath),
			SizeBytes:       fi.Size(),
		})

		progressReporter.CompleteUnit()
	}

	// 5+6. Candidate encode and metric evaluation: adaptive probing with
	// exhaustive fallback, bounded pipelined execution for real runs, or the
	// legacy sequential path for test-hook injection.
	if isAdaptiveEnabled(record, r) {
		if err := r.runAdaptiveCandidates(ctx, w, record, evidence, samplesDir, validatedCandidates, rep, sourceInitialSize, sourceInitialModTime, progressReporter, passesPerSample); err != nil {
			return err
		}
	} else if r.metricsHook == nil {
		// Exhaustive real path: bounded concurrent encode->metric pipeline across
		// all candidates. Evidence is assembled deterministically in
		// (candidate, sample) order, so completion order never affects results.
		if err := r.runPipelinedEncodeMetrics(ctx, w, record, evidence, samplesDir, validatedCandidates, sourceInitialSize, sourceInitialModTime, progressReporter); err != nil {
			return err
		}
	} else {
		// 5. Encode candidate samples sequentially (legacy path for test-hook injection)
		for _, vc := range validatedCandidates {
			for _, window := range record.Samples {
				if ctx.Err() != nil {
					return ctx.Err()
				}

				progressReporter.StartUnit("encoding_candidates")

				if err := verifySourceUnchanged(record.Source, sourceInitialSize, sourceInitialModTime); err != nil {
					return err
				}

				refFileName := fmt.Sprintf("ref_sample_%d.mkv", window.Index)
				refPath := filepath.Join(samplesDir, refFileName)
				if _, err := os.Stat(refPath); err != nil {
					return fmt.Errorf("reference sample %d not found at %s: %w", window.Index, refPath, err)
				}

				candFileName := fmt.Sprintf("%s_sample_%d.mkv", vc.fileKey, window.Index)
				candPath := filepath.Join(samplesDir, candFileName)
				if err := verifyChildPath(samplesDir, candPath); err != nil {
					return fmt.Errorf("invalid candidate sample path: %w", err)
				}
				if err := prepareOutputFile(candPath); err != nil {
					return err
				}

				candArgs, err := BuildCandidateEncodeArgs(refPath, candPath, vc.plan)
				if err != nil {
					return fmt.Errorf("building candidate %q args: %w", vc.candidate.ID, err)
				}

				start := time.Now()
				cmd := exec.CommandContext(ctx, w.ffmpegPath, candArgs...)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Cancel = func() error {
					if cmd.Process != nil && cmd.Process.Pid > 0 {
						return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
					return nil
				}
				cmd.WaitDelay = 2 * time.Second

				stderrBuf := newBoundedBuffer(16 * 1024)
				cmd.Stderr = stderrBuf
				cmd.Stdout = nil

				if err := cmd.Run(); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					errStr := fmt.Sprintf("encoding candidate %q for sample %d failed: %v: %s",
						vc.candidate.ID, window.Index, err, boundedStderr(stderrBuf, 1024))
					evidence.CandidateSamples = append(evidence.CandidateSamples, BenchmarkCandidateSampleResult{
						CandidateID:  vc.candidate.ID,
						SampleIndex:  window.Index,
						File:         filepath.Base(candPath),
						Quality:      vc.candidate.Quality,
						VideoProfile: vc.profile,
						PixelFormat:  vc.pixelFormat,
						Error:        errStr,
					})
					return errors.New(errStr)
				}
				elapsed := time.Since(start).Seconds()

				fi, err := os.Stat(candPath)
				if err != nil || fi.Size() == 0 {
					errStr := fmt.Sprintf("candidate %q for sample %d produced empty or missing file at %s", vc.candidate.ID, window.Index, candPath)
					evidence.CandidateSamples = append(evidence.CandidateSamples, BenchmarkCandidateSampleResult{
						CandidateID:  vc.candidate.ID,
						SampleIndex:  window.Index,
						File:         filepath.Base(candPath),
						Quality:      vc.candidate.Quality,
						VideoProfile: vc.profile,
						PixelFormat:  vc.pixelFormat,
						Error:        errStr,
					})
					return errors.New(errStr)
				}

				evidence.CandidateSamples = append(evidence.CandidateSamples, BenchmarkCandidateSampleResult{
					CandidateID:       vc.candidate.ID,
					SampleIndex:       window.Index,
					File:              filepath.Base(candPath),
					SizeBytes:         fi.Size(),
					EncodeDurationSec: elapsed,
					Quality:           vc.candidate.Quality,
					VideoProfile:      vc.profile,
					PixelFormat:       vc.pixelFormat,
				})

				progressReporter.CompleteUnit()
			}
		}

		// 6. Phase 5 metrics calculation via injected hook (legacy test path;
		// this branch is only reached when r.metricsHook != nil).
		progressReporter.StartUnit("evaluating_metrics")
		if err := r.metricsHook(ctx, w, record, evidence); err != nil {
			return fmt.Errorf("metrics calculation failed: %w", err)
		}
		progressReporter.CompleteUnit()
	} // end legacy sequential encode+hook path

	// 7. Phase 6 candidate evaluation and selection before scratch cleanup
	progressReporter.SetPhase("selecting_candidate")
	if r.selectionHook != nil {
		if err := r.selectionHook(ctx, w, record, evidence); err != nil {
			return fmt.Errorf("candidate selection failed: %w", err)
		}
	} else {
		if err := r.runSelection(ctx, record, evidence, validatedCandidates, rep, sourceInitialSize); err != nil {
			return fmt.Errorf("candidate selection failed: %w", err)
		}
	}

	// Final verification that source was never mutated during the run
	if err := verifySourceUnchanged(record.Source, sourceInitialSize, sourceInitialModTime); err != nil {
		return err
	}

	return nil
}

// BuildReferenceExtractionArgs constructs the exact, safe FFmpeg argument slice
// for extracting a frame-aligned, timestamp-normalized, lossless reference sample.
//
// Frame alignment & windowing rationale:
//  1. Fast & frame-accurate seeking: Placing `-accurate_seek -ss <startSec>` before `-i`
//     enables demuxer keyframe seeking immediately before the target timestamp, followed
//     by accurate frame-by-frame decoding and discarding up to the requested point. This avoids
//     decoding the entire media file from time 0 while guaranteeing deterministic frame boundaries.
//  2. Deterministic window duration: `-t <durationSec>` extracts the requested window,
//     which is deterministically frame-aligned and frame-quantized for VFR/timebase sources
//     rather than a mathematically continuous floating-point cut.
//  3. PTS normalization: `-avoid_negative_ts make_zero` resets stream and container timestamps
//     so that the extracted sample starts cleanly at PTS 0.
//  4. Lossless master: `-c:v ffv1` encodes losslessly, preserving raw decoded pixel format,
//     bit depth, and frame cadence with zero generational loss.
//  5. Clean elementary stream: `-an -sn -dn` strips audio, subtitles, and data streams.
//  6. Deterministic candidate alignment: Candidate samples are subsequently encoded from this
//     FFV1 reference master from frame 0 to end without seeking or trimming. While the source
//     window itself is frame-quantized, the candidate-to-reference frame correspondence is
//     strictly 1:1 and exact for downstream VMAF/SSIM metric evaluation.
func BuildReferenceExtractionArgs(sourcePath, refPath string, videoIndex int, startSec, durationSec float64) []string {
	return []string{
		"-y",
		"-nostats",
		"-accurate_seek",
		"-ss", fmt.Sprintf("%.6f", startSec),
		"-i", sourcePath,
		"-t", fmt.Sprintf("%.6f", durationSec),
		"-avoid_negative_ts", "make_zero",
		"-map", fmt.Sprintf("0:%d", videoIndex),
		"-c:v", "ffv1",
		"-an",
		"-sn",
		"-dn",
		refPath,
	}
}

// BuildCandidateEncodeArgs constructs the exact, safe FFmpeg argument slice
// for encoding a candidate sample from the extracted reference sample using hevc_videotoolbox.
// Because the reference sample is already frame-accurate and timestamp-normalized,
// candidate encoding starts from frame 0 to end, guaranteeing exact 1:1 frame alignment.
func BuildCandidateEncodeArgs(refPath, candPath string, plan *transcode.Plan) ([]string, error) {
	if plan == nil {
		return nil, errors.New("transcode plan cannot be nil (fail closed)")
	}
	videoArgs, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-y",
		"-nostats",
		"-i", refPath,
		"-map", "0:v:0",
	}
	args = append(args, videoArgs...)
	args = append(args, "-an", "-sn", "-dn", candPath)
	return args, nil
}

// prepareOutputFile verifies that the target path does not pre-exist as a symlink.
// If a regular file pre-exists at the target path, it is removed explicitly before FFmpeg runs
// to prevent FFmpeg's -y flag from following pre-existing symlinks or writing into existing inodes.
func prepareOutputFile(targetPath string) error {
	fi, err := os.Lstat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking target output %s: %w", targetPath, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target output %s is a symlink (fail closed)", targetPath)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("target output %s is not a regular file (fail closed)", targetPath)
	}
	if err := os.Remove(targetPath); err != nil {
		return fmt.Errorf("removing existing target file %s: %w", targetPath, err)
	}
	return nil
}

// verifySourceUnchanged checks that the source file size and modification time have not changed.
func verifySourceUnchanged(sourcePath string, expectedSize int64, expectedModTime time.Time) error {
	fi, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("re-stating source file %s failed: %w (fail closed)", sourcePath, err)
	}
	if fi.Size() != expectedSize || !fi.ModTime().Equal(expectedModTime) {
		return fmt.Errorf("source file %s was concurrently modified (size %d->%d, mtime %v->%v): failing closed",
			sourcePath, expectedSize, fi.Size(), expectedModTime, fi.ModTime())
	}
	return nil
}

// isSupportedSourceChroma returns true if the source pixel format has safe 4:2:0 chroma subsampling.
// Full-range yuvj420p is deferred until Phase 5 range-normalized metric pipeline support to avoid
// implicit full-to-limited range conversion discrepancies during VideoToolbox candidate comparison.
func isSupportedSourceChroma(pixFmt string, bitDepth int) bool {
	p := strings.ToLower(strings.TrimSpace(pixFmt))
	if bitDepth == 8 {
		switch p {
		case "yuv420p", "nv12":
			return true
		}
		return false
	}
	if bitDepth == 10 {
		switch p {
		case "yuv420p10le", "p010le":
			return true
		}
		return false
	}
	return false
}

// candidateFileKey constructs an injective, collision-free filename prefix for a candidate sample.
// Includes the candidate index, sanitized label, deterministic short raw ID hash, and quality level.
func candidateFileKey(index int, rawID string, quality int) string {
	h := sha256.Sum256([]byte(rawID))
	shortHash := hex.EncodeToString(h[:4]) // 8 hex characters
	sanitized := sanitizeCandidateID(rawID)
	if len(sanitized) > 16 {
		sanitized = sanitized[:16]
	}
	return fmt.Sprintf("cand_%d_%s_%s_q%d", index, sanitized, shortHash, quality)
}

func resolveCandidateBitDepth(c *transcode.BenchmarkCandidate, sourceBitDepth int) (profile, pixFmt string, bitDepth int, err error) {
	prof := norm(c.VideoProfile)
	pix := norm(c.PixelFormat)

	if sourceBitDepth == 8 {
		if prof == "main10" || pix == "p010le" {
			return "", "", 0, fmt.Errorf("candidate %q specifies 10-bit (%s/%s) for 8-bit source: cannot convert 8-bit to 10-bit as optimization (fail closed)", c.ID, c.VideoProfile, c.PixelFormat)
		}
		if prof == "" {
			prof = "main"
		}
		if pix == "" {
			pix = "yuv420p"
		}
		if prof != "main" {
			return "", "", 0, fmt.Errorf("candidate %q has unsupported 8-bit HEVC profile %q", c.ID, c.VideoProfile)
		}
		if pix != "yuv420p" {
			return "", "", 0, fmt.Errorf("candidate %q has unsupported 8-bit pixel format %q", c.ID, c.PixelFormat)
		}
		return prof, pix, 8, nil
	}

	if sourceBitDepth == 10 {
		if prof == "main" || pix == "yuv420p" {
			return "", "", 0, fmt.Errorf("candidate %q specifies 8-bit (%s/%s) for 10-bit source: cannot downgrade 10-bit source to 8-bit (fail closed)", c.ID, c.VideoProfile, c.PixelFormat)
		}
		if prof == "" {
			prof = "main10"
		}
		if pix == "" {
			pix = "p010le"
		}
		if prof != "main10" {
			return "", "", 0, fmt.Errorf("candidate %q has unsupported 10-bit HEVC profile %q", c.ID, c.VideoProfile)
		}
		if pix != "p010le" {
			return "", "", 0, fmt.Errorf("candidate %q has unsupported 10-bit pixel format %q", c.ID, c.PixelFormat)
		}
		return prof, pix, 10, nil
	}

	return "", "", 0, fmt.Errorf("unsupported source bit depth %d", sourceBitDepth)
}

func isHDRStreamDetailed(ds mediainspect.DetailedStream) bool {
	return mediainspect.IsHDRorDolbyVisionStream(ds)
}

func sanitizeCandidateID(id string) string {
	var sb strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		}
	}
	res := sb.String()
	if len(res) > 32 {
		res = res[:32]
	}
	if res == "" {
		res = "candidate"
	}
	return res
}

func verifyChildPath(parentDir, targetPath string) error {
	cleanParent := filepath.Clean(parentDir)
	cleanTarget := filepath.Clean(targetPath)

	rel, err := filepath.Rel(cleanParent, cleanTarget)
	if err != nil {
		return fmt.Errorf("path %s cannot be related to %s: %w", targetPath, parentDir, err)
	}
	if rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("target %s escapes parent %s (rel: %s)", targetPath, parentDir, rel)
	}

	if resolvedParent, err := filepath.EvalSymlinks(cleanParent); err == nil {
		targetDir := filepath.Dir(cleanTarget)
		if resolvedDir, err := filepath.EvalSymlinks(targetDir); err == nil {
			relResolved, err := filepath.Rel(resolvedParent, resolvedDir)
			if err != nil || (relResolved == "." && filepath.Clean(targetDir) != cleanParent) || strings.HasPrefix(relResolved, "..") {
				return fmt.Errorf("resolved target dir %s escapes resolved parent %s", resolvedDir, resolvedParent)
			}
		}
	}
	return nil
}

// boundedBuffer is an io.Writer that keeps at most maxBytes in memory.
// If writes exceed maxBytes, the buffer is capped and marked as truncated.
type boundedBuffer struct {
	maxBytes  int
	buf       bytes.Buffer
	truncated bool
}

func newBoundedBuffer(maxBytes int) *boundedBuffer {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024
	}
	return &boundedBuffer{maxBytes: maxBytes}
}

func (b *boundedBuffer) Write(p []byte) (n int, err error) {
	n = len(p)
	if b.buf.Len() >= b.maxBytes {
		b.truncated = true
		return n, nil
	}
	remaining := b.maxBytes - b.buf.Len()
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
	} else {
		b.buf.Write(p)
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	str := strings.TrimSpace(b.buf.String())
	if b.truncated {
		return str + "\n... [stderr truncated]"
	}
	return str
}

func boundedStderr(buf *boundedBuffer, maxLen int) string {
	if buf == nil {
		return ""
	}
	str := buf.String()
	if maxLen > 0 && len(str) > maxLen {
		return str[:maxLen] + "..."
	}
	return str
}

// tailBuffer retains up to maxBytes of trailing output.
// It is thread-safe and preserves the end of process stdout/stderr diagnostics
// where summary lines and final aggregate statistics are printed.
type tailBuffer struct {
	mu       sync.Mutex
	buf      []byte
	maxBytes int
	total    int64
}

func newTailBuffer(maxBytes int) *tailBuffer {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024
	}
	return &tailBuffer{
		buf:      make([]byte, 0, maxBytes),
		maxBytes: maxBytes,
	}
}

func (t *tailBuffer) Write(p []byte) (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n = len(p)
	t.total += int64(n)
	if len(p) >= t.maxBytes {
		t.buf = append(t.buf[:0], p[len(p)-t.maxBytes:]...)
		return n, nil
	}
	overflow := (len(t.buf) + len(p)) - t.maxBytes
	if overflow > 0 {
		copy(t.buf, t.buf[overflow:])
		t.buf = t.buf[:len(t.buf)-overflow]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// MaxMetricLogSizeBytes defines the maximum allowed size of a metric log or stats file (5 MB)
// to prevent denial-of-service / memory exhaustion attacks.
const MaxMetricLogSizeBytes = 5 * 1024 * 1024

// escapeFFmpegFilterPath escapes an absolute filesystem path for safe use as a filter option
// value in an FFmpeg -filter_complex argument.
// FFmpeg filtergraph evaluation involves two distinct unescaping stages:
//  1. Filtergraph syntax parsing (avfilter_graph_parse2), which unescapes backslashes preceding
//     special syntax characters such as ':', '\”, '\\', '[', ']', ';', ','.
//  2. Filter option key=value parsing (av_opt_set / av_set_options_string).
//
// To safely survive both stages without unintended option splitting or quote stripping:
// - Backslash '\' becomes 4 backslashes "\\\\" (resolves to "\\" after stage 1, then "\" after stage 2)
// - Single quote '\” becomes 3 backslashes + quote "\\\'" (resolves to "\'" after stage 1, then "'" after stage 2)
// - Colon ':' becomes 2 backslashes + colon "\\:" (resolves to "\:" after stage 1, preventing option splitting, then ":" after stage 2)
// - Brackets '[', ']', commas ',', and semicolons ';' are prefixed with "\\".
func escapeFFmpegFilterPath(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '\'':
			b.WriteString(`\\\'`)
		case ':':
			b.WriteString(`\\:`)
		case '[', ']', ';', ',':
			b.WriteString(`\\`)
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// BuildVMAFArgs constructs the exact, safe FFmpeg argument slice
// for calculating VMAF between a candidate sample (distorted, 0:v)
// and an FFV1 reference sample (reference, 1:v).
func BuildVMAFArgs(candPath, refPath, logPath string) []string {
	return []string{
		"-nostats",
		"-i", candPath,
		"-i", refPath,
		"-filter_complex", fmt.Sprintf("[0:v][1:v]libvmaf=log_fmt=json:log_path=%s", escapeFFmpegFilterPath(logPath)),
		"-f", "null",
		"-",
	}
}

// BuildSSIMArgs constructs the exact, safe FFmpeg argument slice
// for calculating SSIM between a candidate sample (distorted, 0:v)
// and an FFV1 reference sample (reference, 1:v).
func BuildSSIMArgs(candPath, refPath, statsPath string) []string {
	filter := "[0:v][1:v]ssim"
	if statsPath != "" {
		filter = fmt.Sprintf("[0:v][1:v]ssim=stats_file=%s", escapeFFmpegFilterPath(statsPath))
	}
	return []string{
		"-nostats",
		"-i", candPath,
		"-i", refPath,
		"-filter_complex", filter,
		"-f", "null",
		"-",
	}
}

// derivedMetricLogPath constructs a collision-free, verified scratch path for metric logs
// derived from candidate index, sample index, safe hash, and metric type.
func derivedMetricLogPath(samplesDir string, metric string, candIdx int, candidateID string, sampleIdx int) (string, error) {
	h := sha256.Sum256([]byte(candidateID))
	shortHash := hex.EncodeToString(h[:4]) // 8 hex chars
	var ext string
	if metric == "vmaf" {
		ext = "json"
	} else {
		ext = "log"
	}
	filename := fmt.Sprintf("metric_%s_cand_%d_%s_sample_%d.%s", metric, candIdx, shortHash, sampleIdx, ext)
	logPath := filepath.Join(samplesDir, filename)
	if err := verifyChildPath(samplesDir, logPath); err != nil {
		return "", fmt.Errorf("resolving metric log path %s: %w", filename, err)
	}
	return logPath, nil
}

type vmafJSONLog struct {
	Version       string                      `json:"version"`
	PooledMetrics map[string]vmafPooledMetric `json:"pooled_metrics"`
	Frames        []vmafFrame                 `json:"frames"`
}

type vmafPooledMetric struct {
	Min          *float64 `json:"min"`
	Max          *float64 `json:"max"`
	Mean         *float64 `json:"mean"`
	HarmonicMean *float64 `json:"harmonic_mean"`
}

type vmafFrame struct {
	FrameNum int                `json:"frameNum"`
	Metrics  map[string]float64 `json:"metrics"`
}

// ParseVMAFJSON parses libvmaf JSON log data, validating bounds and finite float.
func ParseVMAFJSON(data []byte) (float64, error) {
	if len(data) == 0 {
		return 0, errors.New("empty vmaf log data (fail closed)")
	}
	if len(data) > MaxMetricLogSizeBytes {
		return 0, fmt.Errorf("oversized vmaf log data (%d bytes exceeds %d max) (fail closed)", len(data), MaxMetricLogSizeBytes)
	}

	var log vmafJSONLog
	if err := json.Unmarshal(data, &log); err != nil {
		return 0, fmt.Errorf("malformed vmaf json log: %w (fail closed)", err)
	}

	var score float64
	found := false

	if log.PooledMetrics != nil {
		if pm, ok := log.PooledMetrics["vmaf"]; ok && pm.Mean != nil {
			score = *pm.Mean
			found = true
		}
	}

	if !found && len(log.Frames) > 0 {
		var sum float64
		for i, f := range log.Frames {
			// Verify frame numbers are strictly increasing and contiguous
			if i == 0 {
				if f.FrameNum < 0 {
					return 0, fmt.Errorf("invalid initial vmaf frame number %d (fail closed)", f.FrameNum)
				}
			} else {
				expectedFrame := log.Frames[i-1].FrameNum + 1
				if f.FrameNum != expectedFrame {
					return 0, fmt.Errorf("vmaf frame numbers not contiguous: frame at index %d has frameNum %d, expected %d (missing, duplicate, or gapped frames; fail closed)", i, f.FrameNum, expectedFrame)
				}
			}

			s, ok := f.Metrics["vmaf"]
			if !ok {
				return 0, fmt.Errorf("missing vmaf metric in frame %d (fail closed)", f.FrameNum)
			}
			if math.IsNaN(s) || math.IsInf(s, 0) {
				return 0, fmt.Errorf("vmaf frame %d score is NaN or Inf (fail closed)", f.FrameNum)
			}
			if s < 0.0 || s > 100.0 {
				return 0, fmt.Errorf("vmaf frame %d score %v out of range [0, 100] (fail closed)", f.FrameNum, s)
			}
			sum += s
		}
		score = sum / float64(len(log.Frames))
		found = true
	}

	if !found {
		return 0, errors.New("vmaf score not found in pooled_metrics or frames (fail closed)")
	}

	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0, errors.New("vmaf score is NaN or Inf (fail closed)")
	}
	if score < 0.0 || score > 100.0 {
		return 0, fmt.Errorf("vmaf score %v out of range [0, 100] (fail closed)", score)
	}

	return score, nil
}

// ParseSSIMStatsFile parses an FFmpeg ssim stats_file output, averaging the per-frame All scores.
func ParseSSIMStatsFile(data []byte) (float64, error) {
	if len(data) == 0 {
		return 0, errors.New("empty ssim stats data (fail closed)")
	}
	if len(data) > MaxMetricLogSizeBytes {
		return 0, fmt.Errorf("oversized ssim stats data (%d bytes exceeds %d max) (fail closed)", len(data), MaxMetricLogSizeBytes)
	}

	lines := strings.Split(string(data), "\n")
	var sum float64
	var count int

	re := regexp.MustCompile(`\bAll:([0-9.]+|nan|inf|-inf)\b`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		matches := re.FindStringSubmatch(line)
		if len(matches) < 2 {
			continue
		}
		valStr := matches[1]
		if strings.EqualFold(valStr, "nan") || strings.EqualFold(valStr, "inf") || strings.EqualFold(valStr, "-inf") {
			return 0, errors.New("ssim frame score is NaN or Inf (fail closed)")
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing ssim frame score %q: %w (fail closed)", valStr, err)
		}
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, errors.New("ssim frame score is NaN or Inf (fail closed)")
		}
		if val < 0.0 || val > 1.0 {
			return 0, fmt.Errorf("ssim frame score %v out of range [0, 1] (fail closed)", val)
		}
		sum += val
		count++
	}

	if count == 0 {
		return 0, errors.New("no valid ssim frames found in stats file (fail closed)")
	}

	avg := sum / float64(count)
	if math.IsNaN(avg) || math.IsInf(avg, 0) {
		return 0, errors.New("ssim average score is NaN or Inf (fail closed)")
	}
	if avg < 0.0 || avg > 1.0 {
		return 0, fmt.Errorf("ssim average score %v out of range [0, 1] (fail closed)", avg)
	}

	return avg, nil
}

// ParseSSIMStderr extracts aggregate SSIM score from FFmpeg stderr output, choosing the last match (final aggregate).
func ParseSSIMStderr(stderr string) (float64, error) {
	if strings.TrimSpace(stderr) == "" {
		return 0, errors.New("empty stderr output (fail closed)")
	}
	re := regexp.MustCompile(`SSIM\s+.*?\bAll:([0-9.]+|nan|inf|-inf)\b`)
	allMatches := re.FindAllStringSubmatch(stderr, -1)
	if len(allMatches) == 0 {
		return 0, errors.New("ssim aggregate score not found in stderr (fail closed)")
	}
	// Select the last match which represents the final summary
	lastMatch := allMatches[len(allMatches)-1]
	valStr := lastMatch[1]
	if strings.EqualFold(valStr, "nan") || strings.EqualFold(valStr, "inf") || strings.EqualFold(valStr, "-inf") {
		return 0, errors.New("ssim aggregate score is NaN or Inf (fail closed)")
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing ssim score %q: %w (fail closed)", valStr, err)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, errors.New("ssim aggregate score is NaN or Inf (fail closed)")
	}
	if val < 0.0 || val > 1.0 {
		return 0, fmt.Errorf("ssim aggregate score %v out of range [0, 1] (fail closed)", val)
	}
	return val, nil
}

func readMetricLogFile(logPath string) ([]byte, error) {
	f, err := openNoFollow(logPath)
	if err != nil {
		return nil, fmt.Errorf("open metric log %s: %w (symlinks or unreadable files rejected, fail closed)", logPath, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat open metric log %s: %w (fail closed)", logPath, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("metric log %s is not a regular file (symlink or special file rejected, fail closed)", logPath)
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("metric log %s is empty (fail closed)", logPath)
	}
	if fi.Size() > MaxMetricLogSizeBytes {
		return nil, fmt.Errorf("metric log %s size %d exceeds maximum allowed %d (fail closed)", logPath, fi.Size(), MaxMetricLogSizeBytes)
	}

	lr := io.LimitReader(f, int64(MaxMetricLogSizeBytes)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read metric log %s: %w (fail closed)", logPath, err)
	}
	if int64(len(data)) > int64(MaxMetricLogSizeBytes) {
		return nil, fmt.Errorf("metric log %s exceeded limit of %d bytes while reading (fail closed)", logPath, MaxMetricLogSizeBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("metric log %s contained 0 bytes after read (fail closed)", logPath)
	}
	return data, nil
}

func (r *ProductionBenchmarkRunner) runSelection(
	ctx context.Context,
	record *BenchmarkRecord,
	evidence *BenchmarkExecutionEvidence,
	validatedCandidates []validatedCandidate,
	rep mediainspect.DetailedReport,
	sourceInitialSize int64,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	normMetric := strings.ToLower(strings.TrimSpace(record.Metric))
	if normMetric == "" {
		normMetric = "vmaf"
	}

	// Resolve quality policies (VMAF and SSIM)
	vmafPolicy := optimization.DefaultVMAFPolicy()
	if record.Quality != nil && record.Quality.VMAF != nil {
		tol := vmafPolicy.Tolerance()
		if record.Quality.VMAF.MarginalTolerance != nil {
			tol = *record.Quality.VMAF.MarginalTolerance
		}
		vmafPolicy = optimization.NewVMAFPolicy(record.Quality.VMAF.Target, record.Quality.VMAF.Minimum, tol)
	}

	ssimPolicy := optimization.DefaultSSIMPolicy()
	if record.Quality != nil && record.Quality.SSIM != nil {
		tol := ssimPolicy.Tolerance()
		if record.Quality.SSIM.MarginalTolerance != nil {
			tol = *record.Quality.SSIM.MarginalTolerance
		}
		ssimPolicy = optimization.NewSSIMPolicy(record.Quality.SSIM.Target, record.Quality.SSIM.Minimum, tol)
	}

	// Validate policies fail closed
	if err := optimization.ValidatePolicy(vmafPolicy); err != nil {
		return fmt.Errorf("validating vmaf policy: %w (fail closed)", err)
	}
	if err := optimization.ValidatePolicy(ssimPolicy); err != nil {
		return fmt.Errorf("validating ssim policy: %w (fail closed)", err)
	}

	// Determine primary selector policy based on requested metric and availability
	var selectorPolicy optimization.QualityPolicy
	switch normMetric {
	case "vmaf":
		selectorPolicy = vmafPolicy
	case "ssim":
		selectorPolicy = ssimPolicy
	case "both", "vmaf+ssim":
		preferred := "vmaf"
		if record.Quality != nil && strings.ToLower(strings.TrimSpace(record.Quality.PreferredMetric)) != "" {
			preferred = strings.ToLower(strings.TrimSpace(record.Quality.PreferredMetric))
		}
		hasValidVMAF := false
		hasValidSSIM := false
		for _, cm := range evidence.CandidateMetrics {
			if cm.MetricType == optimization.MetricTypeVMAF && cm.Aggregate.Valid {
				hasValidVMAF = true
			}
			if cm.MetricType == optimization.MetricTypeSSIM && cm.Aggregate.Valid {
				hasValidSSIM = true
			}
		}
		if preferred == "ssim" {
			if hasValidSSIM || !hasValidVMAF {
				selectorPolicy = ssimPolicy
			} else {
				selectorPolicy = vmafPolicy
			}
		} else {
			if hasValidVMAF || !hasValidSSIM {
				selectorPolicy = vmafPolicy
			} else {
				selectorPolicy = ssimPolicy
			}
		}
	default:
		selectorPolicy = vmafPolicy
	}

	// Assemble ColorInfo
	colorInfo := optimization.ColorInfo{
		BitDepth: evidence.SourceBitDepth,
	}
	if len(rep.Video) > 0 {
		colorInfo.ColorPrimaries = rep.Video[0].ColorPrimaries
		colorInfo.ColorTransfer = rep.Video[0].ColorTransfer
		colorInfo.ColorSpace = rep.Video[0].ColorSpace
		colorInfo.PixelFormat = rep.Video[0].PixelFormat
		if colorInfo.BitDepth == 0 {
			colorInfo.BitDepth = rep.Video[0].BitDepth
		}
	}

	// Media size and duration
	sourceSize := rep.SizeBytes
	if sourceSize <= 0 {
		sourceSize = sourceInitialSize
	}
	totalDuration := record.SourceDuration
	if totalDuration <= 0 {
		totalDuration = rep.DurationSec
	}

	// Total planned sample duration
	var totalSampleDurationSec float64
	for _, s := range record.Samples {
		totalSampleDurationSec += s.DurationSeconds
	}
	if totalSampleDurationSec <= 0 {
		for _, ref := range evidence.ReferenceSamples {
			totalSampleDurationSec += ref.DurationSeconds
		}
	}

	// Assemble stream estimates (audio, subtitle, attachments)
	audioEstimates := make([]optimization.AudioStreamEstimate, 0, len(rep.Audio))
	for _, a := range rep.Audio {
		ae := optimization.AudioStreamEstimate{
			Index:      a.Index,
			Codec:      a.Codec,
			Channels:   a.Channels,
			BitrateBps: a.BitRate,
			Copied:     true,
		}
		if ae.BitrateBps == 0 && a.Tags != nil {
			if s, ok := a.Tags["BPS"]; ok {
				if bps, err := strconv.ParseInt(s, 10, 64); err == nil && bps > 0 {
					ae.BitrateBps = bps
				}
			}
			if ae.BitrateBps == 0 {
				if s, ok := a.Tags["NUMBER_OF_BYTES"]; ok {
					if bytes, err := strconv.ParseInt(s, 10, 64); err == nil && bytes > 0 {
						ae.SizeBytes = bytes
					}
				}
			}
		}
		if ae.BitrateBps == 0 && ae.SizeBytes == 0 && record.FallbackAudioBitrateBps > 0 {
			ae.FallbackBitrateBps = record.FallbackAudioBitrateBps
		}
		audioEstimates = append(audioEstimates, ae)
	}

	subEstimates := make([]optimization.SubtitleStreamEstimate, 0, len(rep.Subtitles))
	for _, s := range rep.Subtitles {
		se := optimization.SubtitleStreamEstimate{
			Index: s.Index,
			Codec: s.Codec,
		}
		if s.Tags != nil {
			if sBytes, ok := s.Tags["NUMBER_OF_BYTES"]; ok {
				if b, err := strconv.ParseInt(sBytes, 10, 64); err == nil && b > 0 {
					se.SizeBytes = b
				}
			}
		}
		if se.SizeBytes == 0 && record.FallbackSubtitleSizeBytes > 0 {
			se.FallbackSizeBytes = record.FallbackSubtitleSizeBytes
		}
		subEstimates = append(subEstimates, se)
	}

	var attachmentBytes int64
	if record.AttachmentBytes > 0 {
		attachmentBytes = record.AttachmentBytes
	} else {
		for _, att := range rep.Attachments {
			if att.Tags != nil {
				if sBytes, ok := att.Tags["NUMBER_OF_BYTES"]; ok {
					if b, err := strconv.ParseInt(sBytes, 10, 64); err == nil && b > 0 {
						attachmentBytes += b
					}
				}
			}
		}
	}

	candidateInputs := make([]optimization.CandidateInput, 0, len(validatedCandidates))
	candidateEstimates := make([]optimization.EstimationResult, len(validatedCandidates))

	for i, vc := range validatedCandidates {
		// Determine candidate encode success and total sample bytes
		encodeSuccess := true
		var candSampleBytes int64
		sampleCount := 0
		for _, cs := range evidence.CandidateSamples {
			if cs.CandidateID == vc.candidate.ID {
				if cs.Error != "" || cs.SizeBytes <= 0 {
					encodeSuccess = false
				} else {
					candSampleBytes += cs.SizeBytes
					sampleCount++
				}
			}
		}
		if len(record.Samples) > 0 && sampleCount != len(record.Samples) {
			encodeSuccess = false
		}

		// Estimate candidate output size
		estInput := optimization.OutputEstimateInput{
			SourceSizeBytes:         sourceSize,
			TotalDurationSeconds:    totalDuration,
			SampleDurationSeconds:   totalSampleDurationSec,
			SampleVideoBytes:        candSampleBytes,
			DeclaredVideoBitrateBps: record.DeclaredVideoBitrateBps,
			AudioStreams:            audioEstimates,
			SubtitleStreams:         subEstimates,
			AttachmentBytes:         attachmentBytes,
			ContainerOverheadRate:   optimization.DefaultContainerOverheadRate,
		}
		est, err := optimization.EstimateOutput(estInput)
		if err != nil {
			est.SuitableForSelection = false
			if est.UnusableReason == "" {
				est.UnusableReason = optimization.ReasonInvalidEstimatorInput
			}
		}
		candidateEstimates[i] = est

		// Find candidate's aggregate for the selector policy metric
		var candAgg optimization.MetricAggregate
		foundAgg := false
		for _, cm := range evidence.CandidateMetrics {
			if cm.CandidateID == vc.candidate.ID && cm.MetricType == selectorPolicy.Metric() {
				candAgg = cm.Aggregate
				foundAgg = true
				break
			}
		}
		if !foundAgg {
			candAgg = optimization.MetricAggregate{
				MetricType:       selectorPolicy.Metric(),
				Valid:            false,
				IneligibleReason: optimization.ReasonMetricMissing,
			}
		}

		candidateInputs = append(candidateInputs, optimization.CandidateInput{
			CandidateID:     vc.candidate.ID,
			Profile:         vc.profile,
			EncodeSuccess:   encodeSuccess,
			AggregateResult: candAgg,
			ColorInfo:       colorInfo,
			EstimatedOutput: est,
		})
	}

	// Deterministic selection
	selIn := optimization.SelectorInput{
		Policy:     selectorPolicy,
		Candidates: candidateInputs,
	}
	selRes := optimization.SelectCandidate(selIn)

	decision := &transcode.BenchmarkDecision{
		DecisionReason: selRes.DecisionReason,
		Evaluations:    make([]transcode.BenchmarkCandidateEvaluation, 0, len(validatedCandidates)),
	}

	for i, vc := range validatedCandidates {
		ec := selRes.AllEvaluated[i]
		decision.Evaluations = append(decision.Evaluations, transcode.BenchmarkCandidateEvaluation{
			CandidateID:      vc.candidate.ID,
			CandidateIndex:   vc.index,
			Quality:          vc.candidate.Quality,
			VideoProfile:     vc.profile,
			PixelFormat:      vc.pixelFormat,
			ExpectedBitDepth: vc.bitDepth,
			Score:            ec.Score,
			MetricType:       string(selectorPolicy.Metric()),
			Eligible:         ec.Eligible,
			TargetReached:    ec.TargetReached,
			MinimumMet:       ec.MinimumMet,
			EvaluationReason: ec.EvaluationReason,
			EstimatedBytes:   ec.EstimatedBytes,
			EstimatedMB:      ec.EstimatedMB,
			SavingsPercent:   ec.SavingsPercent,
			Uncertainties:    ec.Uncertainties,
		})
	}

	if selRes.Winner != nil {
		for i, vc := range validatedCandidates {
			if vc.candidate.ID == selRes.Winner.CandidateID {
				est := candidateEstimates[i]
				decision.Winner = &transcode.BenchmarkWinner{
					CandidateID:               vc.candidate.ID,
					CandidateIndex:            vc.index,
					Quality:                   vc.candidate.Quality,
					VideoProfile:              vc.profile,
					PixelFormat:               vc.pixelFormat,
					ExpectedBitDepth:          vc.bitDepth,
					MetricType:                string(selectorPolicy.Metric()),
					Score:                     selRes.Winner.Score,
					TargetReached:             selRes.Winner.TargetReached,
					MinimumMet:                selRes.Winner.MinimumMet,
					EstimatedVideoBytes:       est.EstimatedVideoBytes,
					EstimatedAudioBytes:       est.EstimatedAudioBytes,
					EstimatedSubtitleBytes:    est.EstimatedSubtitleBytes,
					EstimatedAttachmentBytes:  est.EstimatedAttachmentBytes,
					EstimatedMuxOverheadBytes: est.EstimatedMuxOverheadBytes,
					EstimatedTotalBytes:       est.EstimatedTotalBytes,
					EstimatedTotalMB:          est.EstimatedTotalMB,
					SavingsBytes:              est.SavingsBytes,
					SavingsPercent:            est.SavingsPercent,
					Uncertainties:             est.Uncertainties,
				}
				break
			}
		}
	}

	evidence.Decision = decision
	return nil
}
