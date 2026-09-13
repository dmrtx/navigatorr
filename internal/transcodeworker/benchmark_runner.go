package transcodeworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
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
	CandidateID       string  `json:"candidate_id"`
	SampleIndex       int     `json:"sample_index"`
	File              string  `json:"file"`
	SizeBytes         int64   `json:"size_bytes"`
	EncodeDurationSec float64 `json:"encode_duration_sec"`
	Quality           int     `json:"quality"`
	VideoProfile      string  `json:"video_profile,omitempty"`
	PixelFormat       string  `json:"pixel_format,omitempty"`
	Error             string  `json:"error,omitempty"`
}

// BenchmarkExecutionEvidence records all physical sample outcomes produced in Phase 4B.
type BenchmarkExecutionEvidence struct {
	SourceVideoIndex  int                              `json:"source_video_index"`
	SourceBitDepth    int                              `json:"source_bit_depth"`
	SourcePixelFormat string                           `json:"source_pixel_format"`
	SourceResolution  string                           `json:"source_resolution"`
	ReferenceSamples  []BenchmarkSampleRef             `json:"reference_samples"`
	CandidateSamples  []BenchmarkCandidateSampleResult `json:"candidate_samples"`
}

// ProductionBenchmarkRunner implements BenchmarkRunner for Phase 4B.
// It extracts lossless reference samples and encodes candidate samples using hevc_videotoolbox.
type ProductionBenchmarkRunner struct {
	metricsHook func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error
}

// SetMetricsHook sets an optional metrics callback for Phase 5 integration before scratch cleanup.
func (r *ProductionBenchmarkRunner) SetMetricsHook(hook func(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence) error) {
	r.metricsHook = hook
}

// RunBenchmark executes the Phase 4B sample extraction and candidate encoding pipeline:
// 1. Inspects source media with ffprobe model, enforcing single video stream, SDR-only, and 8/10-bit support.
// 2. Gating check of worker capabilities and candidate compatibility before encoding.
// 3. Sequential extraction of lossless reference samples for each sample window.
// 4. Sequential encoding of VideoToolbox candidate samples from reference samples.
// 5. Invokes optional metrics hook (Phase 5) before scratch cleanup.
// 6. Persists complete physical evidence into the record.
func (r *ProductionBenchmarkRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	if record == nil {
		return errors.New("benchmark record cannot be nil (fail closed)")
	}
	if w == nil {
		return errors.New("worker instance cannot be nil (fail closed)")
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, record.ID)
	samplesDir := filepath.Join(jobDir, "samples")

	if err := verifyChildPath(jobDir, samplesDir); err != nil {
		return fmt.Errorf("invalid samples directory: %w", err)
	}
	if err := os.MkdirAll(samplesDir, 0755); err != nil {
		return fmt.Errorf("creating samples directory: %w", err)
	}

	// 1. Source inspection using ffprobe model
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

	// HDR / Dolby Vision rejection: automatic benchmark optimization is SDR-only
	if (rep.HDR != nil && rep.HDR.Present) || isHDRStreamDetailed(sourceVideo) {
		return errors.New("HDR/Dolby Vision source not eligible for automatic benchmark optimization (fail closed)")
	}

	// Source bit-depth validation
	sourceBitDepth := sourceVideo.BitDepth
	if sourceBitDepth != 8 && sourceBitDepth != 10 {
		return fmt.Errorf("unsupported source bit depth %d: only 8-bit and 10-bit sources are supported for benchmark", sourceBitDepth)
	}

	// 2. Capability probing and candidate validation upfront
	caps, err := ProbeVideoToolboxCapabilities(ctx, w.ffmpegPath)
	if err != nil {
		return fmt.Errorf("probing worker video capabilities: %w", err)
	}
	if !caps.Available {
		return errors.New("hevc_videotoolbox encoder is not available on worker (fail closed)")
	}

	type validatedCandidate struct {
		candidate   transcode.BenchmarkCandidate
		plan        *transcode.Plan
		profile     string
		pixelFormat string
		bitDepth    int
	}

	validatedCandidates := make([]validatedCandidate, 0, len(record.Candidates))
	for _, c := range record.Candidates {
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
		validatedCandidates = append(validatedCandidates, validatedCandidate{
			candidate:   c,
			plan:        plan,
			profile:     prof,
			pixelFormat: pix,
			bitDepth:    bd,
		})
	}

	// Initialize evidence
	resStr := fmt.Sprintf("%dx%d", sourceVideo.Width, sourceVideo.Height)
	evidence := &BenchmarkExecutionEvidence{
		SourceVideoIndex:  sourceVideo.Index,
		SourceBitDepth:    sourceBitDepth,
		SourcePixelFormat: sourceVideo.PixelFormat,
		SourceResolution:  resStr,
		ReferenceSamples:  make([]BenchmarkSampleRef, 0, len(record.Samples)),
		CandidateSamples:  make([]BenchmarkCandidateSampleResult, 0, len(record.Candidates)*len(record.Samples)),
	}

	// 3. Extract reference samples sequentially
	for _, window := range record.Samples {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		refFileName := fmt.Sprintf("ref_sample_%d.mkv", window.Index)
		refPath := filepath.Join(samplesDir, refFileName)
		if err := verifyChildPath(samplesDir, refPath); err != nil {
			return fmt.Errorf("invalid reference sample path: %w", err)
		}

		refArgs := BuildReferenceExtractionArgs(record.Source, refPath, sourceVideo.Index, window.StartSeconds, window.DurationSeconds)

		cmd := exec.CommandContext(ctx, w.ffmpegPath, refArgs...)
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		cmd.Stdout = nil

		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("extracting reference sample %d failed: %w: %s",
				window.Index, err, boundedStderr(&stderrBuf, 1024))
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
	}

	// 4. Encode candidate samples sequentially
	for _, vc := range validatedCandidates {
		for _, window := range record.Samples {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			refFileName := fmt.Sprintf("ref_sample_%d.mkv", window.Index)
			refPath := filepath.Join(samplesDir, refFileName)
			if _, err := os.Stat(refPath); err != nil {
				return fmt.Errorf("reference sample %d not found at %s: %w", window.Index, refPath, err)
			}

			candFileName := fmt.Sprintf("cand_%s_sample_%d.mkv", sanitizeCandidateID(vc.candidate.ID), window.Index)
			candPath := filepath.Join(samplesDir, candFileName)
			if err := verifyChildPath(samplesDir, candPath); err != nil {
				return fmt.Errorf("invalid candidate sample path: %w", err)
			}

			candArgs, err := BuildCandidateEncodeArgs(refPath, candPath, vc.plan)
			if err != nil {
				return fmt.Errorf("building candidate %q args: %w", vc.candidate.ID, err)
			}

			start := time.Now()
			cmd := exec.CommandContext(ctx, w.ffmpegPath, candArgs...)
			var stderrBuf bytes.Buffer
			cmd.Stderr = &stderrBuf
			cmd.Stdout = nil

			if err := cmd.Run(); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("encoding candidate %q for sample %d failed: %w: %s",
					vc.candidate.ID, window.Index, err, boundedStderr(&stderrBuf, 1024))
			}
			elapsed := time.Since(start).Seconds()

			fi, err := os.Stat(candPath)
			if err != nil || fi.Size() == 0 {
				return fmt.Errorf("candidate %q for sample %d produced empty or missing file at %s", vc.candidate.ID, window.Index, candPath)
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
		}
	}

	// 5. Optional Phase 5 metrics hook before cleanup
	if r.metricsHook != nil {
		if err := r.metricsHook(ctx, w, record, evidence); err != nil {
			return fmt.Errorf("metrics calculation failed: %w", err)
		}
	}

	record.Evidence = evidence
	return nil
}

// BuildReferenceExtractionArgs constructs the exact, safe FFmpeg argument slice
// for extracting a frame-accurate, timestamp-normalized, lossless reference sample.
// - `-accurate_seek` with `-ss` before `-i` ensures exact seeking up to requested timestamp.
// - `-t` limits reading to the exact sample duration.
// - `-avoid_negative_ts make_zero` resets timestamps so the sample starts cleanly at zero.
// - `-map 0:<videoIndex>` explicitly maps only the primary video stream.
// - `-c:v ffv1` encodes losslessly, preserving exact pixel format, bit depth, and frames.
// - `-an -sn -dn` explicitly strips audio, subtitles, and data streams.
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
	if ds.MasteringDisplay != nil || ds.ContentLightLevel != nil {
		return true
	}
	transfer := strings.ToLower(strings.TrimSpace(ds.ColorTransfer))
	switch transfer {
	case "smpte2084", "arib-std-b67", "arib_std_b67", "hlg", "pq", "smpte428", "bt2020-10", "bt2020-12":
		return true
	}
	primaries := strings.ToLower(strings.TrimSpace(ds.ColorPrimaries))
	switch primaries {
	case "bt2020", "bt2020nc", "bt2020c", "dci-p3":
		return true
	}
	cs := strings.ToLower(strings.TrimSpace(ds.ColorSpace))
	switch cs {
	case "bt2020nc", "bt2020c":
		return true
	}
	for _, sd := range ds.SideData {
		sdt := strings.ToLower(strings.TrimSpace(sd.SideDataType))
		if strings.Contains(sdt, "mastering display") || strings.Contains(sdt, "content light") || strings.Contains(sdt, "dovi") || strings.Contains(sdt, "hdr") {
			return true
		}
	}
	return false
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

func boundedStderr(buf *bytes.Buffer, maxLen int) string {
	if buf == nil {
		return ""
	}
	str := strings.TrimSpace(buf.String())
	if maxLen > 0 && len(str) > maxLen {
		return str[:maxLen] + "..."
	}
	return str
}
