package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

	type validatedCandidate struct {
		index       int
		candidate   transcode.BenchmarkCandidate
		plan        *transcode.Plan
		profile     string
		pixelFormat string
		bitDepth    int
		fileKey     string
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

	// 4. Extract reference samples sequentially
	for _, window := range record.Samples {
		if ctx.Err() != nil {
			return ctx.Err()
		}

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
	}

	// 5. Encode candidate samples sequentially
	for _, vc := range validatedCandidates {
		for _, window := range record.Samples {
			if ctx.Err() != nil {
				return ctx.Err()
			}

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
		}
	}

	// 6. Optional Phase 5 metrics hook before scratch cleanup
	if r.metricsHook != nil {
		if err := r.metricsHook(ctx, w, record, evidence); err != nil {
			return fmt.Errorf("metrics calculation failed: %w", err)
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
// 1. Fast & frame-accurate seeking: Placing `-accurate_seek -ss <startSec>` before `-i`
//    enables demuxer keyframe seeking immediately before the target timestamp, followed
//    by accurate frame-by-frame decoding and discarding up to the requested point. This avoids
//    decoding the entire media file from time 0 while guaranteeing deterministic frame boundaries.
// 2. Deterministic window duration: `-t <durationSec>` extracts the requested window,
//    which is deterministically frame-aligned and frame-quantized for VFR/timebase sources
//    rather than a mathematically continuous floating-point cut.
// 3. PTS normalization: `-avoid_negative_ts make_zero` resets stream and container timestamps
//    so that the extracted sample starts cleanly at PTS 0.
// 4. Lossless master: `-c:v ffv1` encodes losslessly, preserving raw decoded pixel format,
//    bit depth, and frame cadence with zero generational loss.
// 5. Clean elementary stream: `-an -sn -dn` strips audio, subtitles, and data streams.
// 6. Deterministic candidate alignment: Candidate samples are subsequently encoded from this
//    FFV1 reference master from frame 0 to end without seeking or trimming. While the source
//    window itself is frame-quantized, the candidate-to-reference frame correspondence is
//    strictly 1:1 and exact for downstream VMAF/SSIM metric evaluation.
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
	// 1. Mastering display metadata or content light level
	if ds.MasteringDisplay != nil || ds.ContentLightLevel != nil {
		return true
	}

	// 2. Codec and profile check for Dolby Vision / HDR
	codec := strings.ToLower(strings.TrimSpace(ds.Codec))
	profile := strings.ToLower(strings.TrimSpace(ds.Profile))
	if strings.Contains(codec, "dovi") || strings.Contains(codec, "dvh1") ||
		strings.Contains(codec, "dvhe") || strings.Contains(codec, "dva1") ||
		strings.Contains(codec, "dav1") {
		return true
	}
	if strings.Contains(profile, "dolby vision") || strings.Contains(profile, "dovi") ||
		strings.HasPrefix(profile, "dv") {
		return true
	}

	// 3. Color transfer characteristics
	transfer := strings.ToLower(strings.TrimSpace(ds.ColorTransfer))
	switch transfer {
	case "smpte2084", "arib-std-b67", "arib_std_b67", "hlg", "pq", "smpte428", "bt2020-10", "bt2020-12":
		return true
	}

	// 4. Color primaries
	primaries := strings.ToLower(strings.TrimSpace(ds.ColorPrimaries))
	switch primaries {
	case "bt2020", "bt2020nc", "bt2020c", "dci-p3":
		return true
	}

	// 5. Color space / matrix coefficients
	cs := strings.ToLower(strings.TrimSpace(ds.ColorSpace))
	switch cs {
	case "bt2020nc", "bt2020c":
		return true
	}

	// 6. Side data (DV RPU, mastering display, etc.)
	for _, sd := range ds.SideData {
		sdt := strings.ToLower(strings.TrimSpace(sd.SideDataType))
		if strings.Contains(sdt, "mastering display") ||
			strings.Contains(sdt, "content light") ||
			strings.Contains(sdt, "dovi") ||
			strings.Contains(sdt, "dolby vision") ||
			strings.Contains(sdt, "hdr") {
			return true
		}
	}

	// 7. Stream tags
	for k, v := range ds.Tags {
		kl := strings.ToLower(k)
		vl := strings.ToLower(v)
		if strings.Contains(kl, "dovi") || strings.Contains(kl, "dolby") ||
			strings.Contains(vl, "dovi") || strings.Contains(vl, "dolby vision") ||
			strings.Contains(vl, "dvh1") || strings.Contains(vl, "dvhe") ||
			strings.Contains(vl, "dva1") || strings.Contains(vl, "dav1") {
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
