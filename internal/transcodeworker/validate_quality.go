package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// validateFinalPerceptual validates deterministic windows from the immutable
// benchmark plan against the exact completed local candidate before publish.
// Both extracted windows are FFV1 (lossless), then checked frame-for-frame.
func (w *Worker) validateFinalPerceptual(ctx context.Context, jobDir, source, candidate string, plan *transcode.Plan, candidateSHA string, candidateSize int64) (*transcode.FinalQualityEvidence, error) {
	if plan == nil || plan.QualityValidation == nil {
		return nil, nil
	}
	q := plan.QualityValidation
	e := &transcode.FinalQualityEvidence{Verdict: "fail", CandidateSHA256: candidateSHA, CandidateSizeBytes: candidateSize, PlanDigest: plan.PlanDigest, BenchmarkRequestDigest: q.BenchmarkRequestDigest}
	fail := func(code string, cause error) (*transcode.FinalQualityEvidence, error) {
		e.ReasonCodes = append(e.ReasonCodes, code)
		return e, fmt.Errorf("%s: %w", code, cause)
	}
	if q.Quality.FinalValidation == nil || q.Quality.FinalValidation.Mode != "sampled" {
		return fail("quality_final_plan_invalid", fmt.Errorf("unsupported final validation mode"))
	}
	caps, err := w.Capabilities(ctx)
	if err != nil {
		return fail("quality_capability_probe_failed", err)
	}
	e.CapabilityFingerprint = caps.CapabilityFingerprint
	var model *quality.VMAFModel
	runVMAF := q.Metric == "vmaf" || q.Metric == "both" || q.Metric == "vmaf+ssim"
	runSSIM := q.Metric == "ssim" || q.Metric == "both" || q.Metric == "vmaf+ssim"
	runCAMBI := q.Quality.Banding != nil && q.Quality.Banding.Enabled
	if runVMAF {
		if q.Quality.VMAF == nil || q.Quality.VMAF.Model == "" {
			return fail("quality_vmaf_model_unavailable", fmt.Errorf("explicit model required"))
		}
		m, err := quality.Model(q.Quality.VMAF.Model)
		if err != nil {
			return fail("quality_vmaf_model_unavailable", err)
		}
		if caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.Models[m.ID].Available {
			return fail("quality_vmaf_model_unavailable", fmt.Errorf("worker cannot execute %s", m.ID))
		}
		model = &m
		e.ModelID = m.ID
	}
	if runSSIM && !caps.Filters["ssim"] {
		return fail("quality_ssim_unavailable", fmt.Errorf("worker has no ssim filter"))
	}
	if runCAMBI && (caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.CAMBIFullRef) {
		return fail("quality_cambi_full_ref_unavailable", fmt.Errorf("worker cannot execute full-reference CAMBI"))
	}
	sourceRep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, source)
	if err != nil || len(sourceRep.Video) != 1 {
		return fail("quality_final_source_probe_failed", fmt.Errorf("source video probe: %v", err))
	}
	candidateRep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, candidate)
	if err != nil || len(candidateRep.Video) != 1 {
		return fail("quality_final_candidate_probe_failed", fmt.Errorf("candidate video probe: %v", err))
	}
	e.SourceBitDepth = sourceRep.Video[0].BitDepth
	e.CandidateBitDepth = candidateRep.Video[0].BitDepth
	if sourceRep.Video[0].FPS <= 0 || !isFiniteQuality(sourceRep.Video[0].FPS) {
		return fail("quality_final_source_timing_unavailable", fmt.Errorf("source FPS unavailable"))
	}
	if sourceRep.Video[0].BitDepth > 8 && runVMAF {
		return fail("quality_vmaf_native_main10_unverified", fmt.Errorf("native %d-bit source not eligible for VMAF", sourceRep.Video[0].BitDepth))
	}
	if sourceRep.Video[0].BitDepth > 8 && runCAMBI {
		return fail("quality_cambi_native_main10_unverified", fmt.Errorf("native %d-bit source not eligible for CAMBI", sourceRep.Video[0].BitDepth))
	}
	if sourceRep.Video[0].FPS >= 45 && runVMAF {
		return fail("quality_vmaf_hfr_model_unavailable", fmt.Errorf("non-HFR model requested for %.3f FPS source", sourceRep.Video[0].FPS))
	}
	if sourceRep.DurationSec <= 0 || candidateRep.DurationSec <= 0 {
		return fail("quality_final_duration_unavailable", fmt.Errorf("source or candidate duration missing"))
	}
	scratch, err := os.MkdirTemp(jobDir, "quality-samples-")
	if err != nil {
		return fail("quality_final_scratch_unavailable", err)
	}
	defer os.RemoveAll(scratch)
	var vmafSamples []quality.VMAFStats
	var cambiSamples []quality.CAMBIStats
	var ssimScores []float64
	for _, window := range q.Samples {
		if err := ctx.Err(); err != nil {
			return e, err
		}
		if window.StartSeconds < 0 || window.DurationSeconds <= 0 || window.StartSeconds+window.DurationSeconds > math.Min(sourceRep.DurationSec, candidateRep.DurationSec)+0.05 {
			return fail("quality_final_sample_misaligned", fmt.Errorf("window %d exceeds source/candidate timeline", window.Index))
		}
		refPath := filepath.Join(scratch, fmt.Sprintf("reference_%d.mkv", window.Index))
		candPath := filepath.Join(scratch, fmt.Sprintf("candidate_%d.mkv", window.Index))
		for _, item := range []struct {
			source, out string
			videoIndex  int
		}{{source, refPath, sourceRep.Video[0].Index}, {candidate, candPath, candidateRep.Video[0].Index}} {
			if _, err := runQualityCommand(ctx, w.ffmpegPath, BuildReferenceExtractionArgs(item.source, item.out, item.videoIndex, window.StartSeconds, window.DurationSeconds)); err != nil {
				return fail("quality_final_sample_extract_failed", err)
			}
		}
		refPTS, err := probeSamplePTS(ctx, w.ffprobePath, refPath)
		if err != nil {
			return fail("quality_final_sample_misaligned", err)
		}
		candPTS, err := probeSamplePTS(ctx, w.ffprobePath, candPath)
		if err != nil {
			return fail("quality_final_sample_misaligned", err)
		}
		if err := verifySampleAlignment(refPTS, candPTS, sourceRep.Video[0].FPS); err != nil {
			return fail("quality_final_sample_misaligned", err)
		}
		if runVMAF {
			logPath := filepath.Join(scratch, fmt.Sprintf("vmaf_%d.json", window.Index))
			args, err := BuildVMAFModelArgs(candPath, refPath, logPath, *model)
			if err != nil {
				return fail("quality_vmaf_measurement_path_unavailable", err)
			}
			if _, err := runQualityCommand(ctx, w.ffmpegPath, args); err != nil {
				return fail("quality_vmaf_measurement_path_unavailable", err)
			}
			data, err := readMetricLogFile(logPath)
			if err != nil {
				return fail("quality_vmaf_evidence_incomplete", err)
			}
			stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, *model, window.Index, window.StartSeconds, sourceRep.Video[0].FPS, q.Quality.VMAF.WorstWindowSeconds, q.Quality.VMAF.FrameThreshold)
			if err != nil || stats.FramesTotal != len(refPTS) {
				return fail("quality_vmaf_evidence_incomplete", fmt.Errorf("sample %d: %v", window.Index, err))
			}
			vmafSamples = append(vmafSamples, stats)
		}
		if runCAMBI {
			stats, err := runCAMBIMetricSample(ctx, w.ffmpegPath, scratch, candPath, refPath, 0, "final", window.Index, window.StartSeconds, sourceRep.Video[0].FPS, caps.Quality.CAMBIOutput)
			if err != nil || stats.FramesTotal != len(refPTS) {
				return fail("quality_cambi_evidence_incomplete", fmt.Errorf("sample %d: %v", window.Index, err))
			}
			cambiSamples = append(cambiSamples, stats)
		}
		if runSSIM {
			logPath := filepath.Join(scratch, fmt.Sprintf("ssim_%d.log", window.Index))
			output, err := runQualityCommand(ctx, w.ffmpegPath, BuildSSIMArgs(candPath, refPath, logPath))
			if err != nil {
				return fail("quality_ssim_evidence_incomplete", err)
			}
			data, err := readMetricLogFile(logPath)
			var score float64
			if err == nil {
				score, err = ParseSSIMStatsFile(data)
			} else {
				score, err = ParseSSIMStderr(string(output))
			}
			if err != nil {
				return fail("quality_ssim_evidence_incomplete", err)
			}
			ssimScores = append(ssimScores, score)
		}
	}
	if runVMAF {
		combined, err := quality.CombineVMAF(vmafSamples)
		if err != nil {
			return fail("quality_vmaf_evidence_incomplete", err)
		}
		e.VMAF = &combined
		v := q.Quality.VMAF
		if combined.Mean < v.Minimum {
			e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_mean_below_minimum")
		}
		if v.GuardrailEnforcement == "reject" {
			if v.P5Minimum != nil && combined.P5 < *v.P5Minimum {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_p5_below_minimum")
			}
			if v.WorstWindowMinimum != nil && combined.WorstWindowMean < *v.WorstWindowMinimum {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_worst_window_below_minimum")
			}
			if v.MaxFramesBelowThreshold != nil && combined.FramesBelowThreshold > *v.MaxFramesBelowThreshold {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_frames_below_limit_exceeded")
			}
		}
	}
	if runCAMBI {
		combined, err := quality.CombineCAMBI(cambiSamples)
		if err != nil {
			return fail("quality_cambi_evidence_incomplete", err)
		}
		e.CAMBI = &combined
		b := q.Quality.Banding
		if b.Enforcement == "reject" {
			if b.MaxMean != nil && combined.Mean > *b.MaxMean {
				e.ReasonCodes = append(e.ReasonCodes, "quality_cambi_mean_above_maximum")
			}
			if b.MaxPeak != nil && combined.Max > *b.MaxPeak {
				e.ReasonCodes = append(e.ReasonCodes, "quality_cambi_peak_above_maximum")
			}
		}
	}
	if runSSIM {
		sum := 0.0
		for _, score := range ssimScores {
			sum += score
		}
		mean := sum / float64(len(ssimScores))
		e.SSIMMean = &mean
		if q.Quality.SSIM != nil && mean < q.Quality.SSIM.Minimum {
			e.ReasonCodes = append(e.ReasonCodes, "quality_ssim_mean_below_minimum")
		}
	}
	if len(e.ReasonCodes) > 0 {
		return e, fmt.Errorf("final perceptual quality rejected: %v", e.ReasonCodes)
	}
	e.Verdict = "pass"
	return e, nil
}

func runQualityCommand(ctx context.Context, ffmpegPath string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	tail := newTailBuffer(64 * 1024)
	cmd.Stderr = tail
	if err := cmd.Run(); err != nil {
		return []byte(tail.String()), fmt.Errorf("metric/extraction ffmpeg failed: %v: %s", err, boundedErrorMessage(nil, []byte(tail.String()), 512))
	}
	output := []byte(tail.String())
	return output, nil
}

func probeSamplePTS(ctx context.Context, ffprobePath, samplePath string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", samplePath)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe sample frame timestamps: %w", err)
	}
	if len(output) > 4*MaxMetricLogSizeBytes {
		return nil, fmt.Errorf("sample frame timestamp evidence oversized")
	}
	var log struct {
		Frames []struct {
			PTS string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(output, &log); err != nil {
		return nil, err
	}
	if len(log.Frames) == 0 {
		return nil, fmt.Errorf("sample has no video frames")
	}
	pts := make([]float64, len(log.Frames))
	for i, frame := range log.Frames {
		v, err := strconv.ParseFloat(frame.PTS, 64)
		if err != nil || !isFiniteQuality(v) {
			return nil, fmt.Errorf("invalid sample frame timestamp at %d", i)
		}
		pts[i] = v
		if i > 0 && pts[i] <= pts[i-1] {
			return nil, fmt.Errorf("non-monotonic sample timestamps")
		}
	}
	return pts, nil
}

func verifySampleAlignment(ref, cand []float64, fps float64) error {
	if len(ref) != len(cand) || len(ref) == 0 {
		return fmt.Errorf("source/candidate frame count mismatch: %d versus %d", len(ref), len(cand))
	}
	tolerance := 0.25 / fps
	for i := range ref {
		if math.Abs((ref[i]-ref[0])-(cand[i]-cand[0])) > tolerance {
			return fmt.Errorf("source/candidate timestamp mismatch at frame %d", i)
		}
	}
	return nil
}

func isFiniteQuality(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
