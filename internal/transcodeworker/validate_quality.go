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
	"sync"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
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
	sourceVideoStreams := sourceRep.MainVideo()
	if err != nil || len(sourceVideoStreams) != 1 {
		return fail("quality_final_source_probe_failed", fmt.Errorf("source video probe: %v", err))
	}
	candidateRep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, candidate)
	candidateVideoStreams := candidateRep.MainVideo()
	if err != nil || len(candidateVideoStreams) != 1 {
		return fail("quality_final_candidate_probe_failed", fmt.Errorf("candidate video probe: %v", err))
	}
	var compactChain string
	if plan.Compact != nil {
		// Compact converts HDR/DV to SDR, so the source may be HDR, but only
		// exactly as frozen; the candidate must be clean SDR at the target size
		// and is measured against the source through the identical chain.
		sourceVideo, candidateVideo := sourceVideoStreams[0], candidateVideoStreams[0]
		dr, err := classifySourceDynamicRange(ctx, w.ffprobePath, source, &sourceRep)
		if err != nil {
			return fail("quality_source_class_unsupported", err)
		}
		if err := plan.Compact.MatchSource(dr, sourceVideo); err != nil {
			return fail("quality_source_class_unsupported", err)
		}
		if (sourceVideo.BitDepth != 8 && sourceVideo.BitDepth != 10) || (candidateVideo.BitDepth != 8 && candidateVideo.BitDepth != 10) || !isSupportedSourceChroma(sourceVideo.PixelFormat, sourceVideo.BitDepth) || !isSupportedSourceChroma(candidateVideo.PixelFormat, candidateVideo.BitDepth) {
			return fail("quality_source_class_unsupported", fmt.Errorf("source/candidate measurement class is unsupported"))
		}
		e.CompactOutput = validateCompactOutput(ctx, w.ffprobePath, candidate, &candidateRep, plan.Compact)
		if e.CompactOutput.Verdict != "pass" {
			e.ReasonCodes = append(e.ReasonCodes, e.CompactOutput.ReasonCodes...)
			return e, compactOutputError(e.CompactOutput)
		}
		digest, err := ensureCompactLUT(plan.Compact)
		if err != nil {
			return fail("quality_compact_chain_unavailable", err)
		}
		if e.CompactChain, err = compactChainEvidence(plan.Compact, digest); err != nil {
			return fail("quality_compact_chain_unavailable", err)
		}
		if compactChain, err = compactFilterForRun(plan.Compact); err != nil {
			return fail("quality_compact_chain_unavailable", err)
		}
	} else if plan.Mode != "" {
		sourceVideo, candidateVideo := sourceVideoStreams[0], candidateVideoStreams[0]
		if mediainspect.IsHDRorDolbyVisionReport(&sourceRep) || mediainspect.IsHDRorDolbyVisionReport(&candidateRep) || (sourceVideo.BitDepth != 8 && sourceVideo.BitDepth != 10) || candidateVideo.BitDepth != modeCandidateBitDepth(plan, sourceVideo.BitDepth) || !isSupportedSourceChroma(sourceVideo.PixelFormat, sourceVideo.BitDepth) || !isSupportedSourceChroma(candidateVideo.PixelFormat, candidateVideo.BitDepth) {
			return fail("quality_source_class_unsupported", fmt.Errorf("source/candidate measurement class is unsupported or changed"))
		}
	}
	e.SourceBitDepth = sourceVideoStreams[0].BitDepth
	e.CandidateBitDepth = candidateVideoStreams[0].BitDepth
	if sourceVideoStreams[0].FPS <= 0 || !isFiniteQuality(sourceVideoStreams[0].FPS) {
		return fail("quality_final_source_timing_unavailable", fmt.Errorf("source FPS unavailable"))
	}
	if candidateVideoStreams[0].BitDepth > 8 && sourceVideoStreams[0].BitDepth == 8 && (runVMAF || runCAMBI) && !caps.SupportsNative10BitQuality() {
		return fail("quality_native_main10_unverified", fmt.Errorf("8-bit source encoded as Main10 needs the proven native 10-bit path"))
	}
	if sourceVideoStreams[0].BitDepth > 8 && runVMAF && !caps.SupportsNative10BitQuality() {
		return fail("quality_vmaf_native_main10_unverified", fmt.Errorf("native %d-bit source not eligible for VMAF", sourceVideoStreams[0].BitDepth))
	}
	if sourceVideoStreams[0].BitDepth > 8 && runCAMBI && !caps.SupportsNative10BitQuality() {
		return fail("quality_cambi_native_main10_unverified", fmt.Errorf("native %d-bit source not eligible for CAMBI", sourceVideoStreams[0].BitDepth))
	}
	vmafDecimation := 1
	if runVMAF {
		// The non-HFR model only sees HFR motion on a frame-exact decimation;
		// rates outside the supported class still fail closed.
		if vmafDecimation, err = quality.TemporalDecimationForStream(sourceVideoStreams[0].FPS, sourceVideoStreams[0].FieldOrder); err != nil {
			return fail("quality_vmaf_hfr_model_unavailable", err)
		}
		if vmafDecimation > 1 && !caps.SupportsHFRQuality() {
			return fail("quality_vmaf_hfr_model_unavailable", fmt.Errorf("no proven decimated VMAF path for %.3f FPS source", sourceVideoStreams[0].FPS))
		}
	}
	if sourceRep.DurationSec <= 0 || candidateRep.DurationSec <= 0 {
		return fail("quality_final_duration_unavailable", fmt.Errorf("source or candidate duration missing"))
	}
	scratch, err := os.MkdirTemp(jobDir, "quality-samples-")
	if err != nil {
		return fail("quality_final_scratch_unavailable", err)
	}
	defer os.RemoveAll(scratch)
	timeline := math.Min(sourceRep.DurationSec, candidateRep.DurationSec)
	concurrency := w.finalValidationConcurrency(len(q.Samples))
	threads := w.metricThreads(concurrency)
	measure := func(ctx context.Context, pos int) finalWindowResult {
		window := q.Samples[pos]
		bad := func(code string, cause error) finalWindowResult {
			return finalWindowResult{code: code, err: cause}
		}
		if window.StartSeconds < 0 || window.DurationSeconds <= 0 || window.StartSeconds+window.DurationSeconds > timeline+0.05 {
			return bad("quality_final_sample_misaligned", fmt.Errorf("window %d exceeds source/candidate timeline", window.Index))
		}
		// Each window owns a directory, removed as soon as it is measured,
		// so at most `concurrency` windows of lossless extracts exist at once.
		dir := filepath.Join(scratch, fmt.Sprintf("window_%d", pos))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return bad("quality_final_scratch_unavailable", err)
		}
		defer os.RemoveAll(dir)
		refPath := filepath.Join(dir, fmt.Sprintf("reference_%d.mkv", window.Index))
		candPath := filepath.Join(dir, fmt.Sprintf("candidate_%d.mkv", window.Index))
		for _, item := range []struct {
			source, out string
			videoIndex  int
		}{{source, refPath, sourceVideoStreams[0].Index}, {candidate, candPath, candidateVideoStreams[0].Index}} {
			args := BuildReferenceExtractionArgs(item.source, item.out, item.videoIndex, window.StartSeconds, window.DurationSeconds)
			if compactChain != "" && item.out == refPath {
				args = withReferenceChain(args, compactChain)
			}
			if _, err := runQualityCommand(ctx, w.ffmpegPath, args); err != nil {
				return bad("quality_final_sample_extract_failed", err)
			}
		}
		refPTS, err := probeSamplePTS(ctx, w.ffprobePath, refPath)
		if err != nil {
			return bad("quality_final_sample_misaligned", err)
		}
		candPTS, err := probeSamplePTS(ctx, w.ffprobePath, candPath)
		if err != nil {
			return bad("quality_final_sample_misaligned", err)
		}
		if err := verifySampleAlignment(refPTS, candPTS, sourceVideoStreams[0].FPS); err != nil {
			return bad("quality_final_sample_misaligned", err)
		}
		var r finalWindowResult
		if runVMAF {
			logPath := filepath.Join(dir, fmt.Sprintf("vmaf_%d.json", window.Index))
			args, err := BuildVMAFModelArgs(candPath, refPath, logPath, *model, candidateVideoStreams[0].BitDepth, vmafDecimation, threads)
			if err != nil {
				return bad("quality_vmaf_measurement_path_unavailable", err)
			}
			if _, err := runQualityCommand(ctx, w.ffmpegPath, args); err != nil {
				return bad("quality_vmaf_measurement_path_unavailable", err)
			}
			data, err := readMetricLogFile(logPath)
			if err != nil {
				return bad("quality_vmaf_evidence_incomplete", err)
			}
			stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, *model, window.Index, window.StartSeconds, sourceVideoStreams[0].FPS, vmafDecimation, q.Quality.VMAF.WorstWindowSeconds, q.Quality.VMAF.FrameThreshold)
			if err != nil || stats.FramesTotal != quality.DecimatedFrameCount(len(refPTS), vmafDecimation) {
				return bad("quality_vmaf_evidence_incomplete", fmt.Errorf("sample %d: %v", window.Index, err))
			}
			r.vmaf = &stats
		}
		if runCAMBI {
			stats, err := runCAMBIMetricSample(ctx, w.ffmpegPath, dir, candPath, refPath, 0, "final", window.Index, window.StartSeconds, sourceVideoStreams[0].FPS, candidateVideoStreams[0].BitDepth, caps.Quality.CAMBIOutput, threads)
			if err != nil || stats.FramesTotal != len(refPTS) {
				return bad("quality_cambi_evidence_incomplete", fmt.Errorf("sample %d: %v", window.Index, err))
			}
			r.cambi = &stats
		}
		if runSSIM {
			logPath := filepath.Join(dir, fmt.Sprintf("ssim_%d.log", window.Index))
			output, err := runQualityCommand(ctx, w.ffmpegPath, BuildSSIMArgs(candPath, refPath, logPath))
			if err != nil {
				return bad("quality_ssim_evidence_incomplete", err)
			}
			data, err := readMetricLogFile(logPath)
			var score float64
			if err == nil {
				score, err = ParseSSIMStatsFile(data)
			} else {
				score, err = ParseSSIMStderr(string(output))
			}
			if err != nil {
				return bad("quality_ssim_evidence_incomplete", err)
			}
			r.ssim = &score
		}
		return r
	}
	results, failed, err := runFinalWindows(ctx, len(q.Samples), concurrency, measure)
	if err != nil {
		return e, err
	}
	if failed >= 0 {
		return fail(results[failed].code, results[failed].err)
	}
	var vmafSamples []quality.VMAFStats
	var cambiSamples []quality.CAMBIStats
	var ssimScores []float64
	for _, r := range results {
		if r.vmaf != nil {
			vmafSamples = append(vmafSamples, *r.vmaf)
		}
		if r.cambi != nil {
			cambiSamples = append(cambiSamples, *r.cambi)
		}
		if r.ssim != nil {
			ssimScores = append(ssimScores, *r.ssim)
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
		for _, sample := range vmafSamples {
			if sample.Mean < v.Minimum {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_sample_below_minimum")
				break
			}
		}
		if v.GuardrailEnforcement == "reject" {
			if v.P5Minimum != nil && combined.P5 < *v.P5Minimum {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_p5_below_minimum")
			}
			if v.WorstWindowMinimum != nil && combined.WorstWindowMean < *v.WorstWindowMinimum {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_worst_window_below_minimum")
			}
			if v.MaxFramesBelowThreshold != nil && combined.SourceEquivalentFramesBelowThreshold() > *v.MaxFramesBelowThreshold {
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

// maxFinalValidationConcurrency bounds how many final validation windows are
// extracted and measured at once.
var maxFinalValidationConcurrency = 3

// finalWindowResult is one window's measurement, or the reason it failed.
type finalWindowResult struct {
	vmaf  *quality.VMAFStats
	cambi *quality.CAMBIStats
	ssim  *float64
	code  string
	err   error
}

// minMetricThreadsPerPass is the fewest libvmaf threads a concurrent metric
// pass is given. One thread per pass measured slower than fewer, wider
// passes: libvmaf feature extraction, not extraction or decode, dominates.
const minMetricThreadsPerPass = 2

// finalValidationConcurrency runs up to maxFinalValidationConcurrency windows
// at once, but only as many as this job's share of the host CPUs (CPUs ÷
// video jobs running together) can give minMetricThreadsPerPass libvmaf threads each.
// An explicit metric_threads fixes the threads of every pass, so windows are
// limited to what that many threads each fit in the job's share (and, like
// sizeModeMetricConcurrency, a count below minMetricThreadsPerPass runs the
// windows one at a time).
func (w *Worker) finalValidationConcurrency(windows int) int {
	jobs := w.videoJobsSharingCPU()
	perPass := minMetricThreadsPerPass
	if w != nil && w.cfg != nil && w.cfg.MetricThreads > 0 {
		if w.cfg.MetricThreads < minMetricThreadsPerPass {
			return 1
		}
		perPass = w.cfg.MetricThreads
	}
	return max(1, min(maxFinalValidationConcurrency, windows, hostCPUCount()/jobs/perPass))
}

// runFinalWindows measures windows 0..n-1 with at most concurrency in flight
// and returns their results in window order. It reproduces the sequential
// verdict exactly: failed is the lowest failing window (-1 when none), every
// window before it has completed and passed, and windows after a failure are
// cancelled or never started. A cancelled ctx returns its error once every
// started window has stopped.
func runFinalWindows(ctx context.Context, n, concurrency int, measure func(context.Context, int) finalWindowResult) ([]finalWindowResult, int, error) {
	results := make([]finalWindowResult, n)
	var (
		mu      sync.Mutex
		next    int
		failed  = -1
		cancels = map[int]context.CancelFunc{}
		wg      sync.WaitGroup
	)
	claim := func() (int, context.Context, bool) {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil || next >= n || (failed >= 0 && next > failed) {
			return 0, nil, false
		}
		pos := next
		next++
		windowCtx, cancel := context.WithCancel(ctx)
		cancels[pos] = cancel
		return pos, windowCtx, true
	}
	for range max(1, min(concurrency, n)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				pos, windowCtx, ok := claim()
				if !ok {
					return
				}
				r := measure(windowCtx, pos)
				mu.Lock()
				cancels[pos]()
				delete(cancels, pos)
				results[pos] = r
				if r.code != "" && (failed < 0 || pos < failed) {
					failed = pos
					// Later windows can no longer change the verdict.
					for other, cancel := range cancels {
						if other > pos {
							cancel()
						}
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return results, -1, err
	}
	return results, failed, nil
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
	if err := runCharged(ctx, cmd); err != nil {
		return []byte(tail.String()), fmt.Errorf("metric/extraction ffmpeg failed: %v: %s", err, boundedErrorMessage(nil, []byte(tail.String()), 512))
	}
	output := []byte(tail.String())
	return output, nil
}

func probeSamplePTS(ctx context.Context, ffprobePath, samplePath string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", samplePath)
	output, err := cmd.Output()
	chargeCPU(ctx, cmd)
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

// Mode preflight rejects unsupported measurement classes on the staged local
// source before committing to a full encode. It never relaxes depth/chroma.
func (w *Worker) validateModeSourceQuality(ctx context.Context, source string, plan *transcode.Plan) error {
	if plan == nil || plan.Mode == "" {
		return nil
	}
	rep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, source)
	video := rep.MainVideo()
	if err != nil || len(video) != 1 {
		return fmt.Errorf("quality_source_evidence_missing: source class unavailable: %v", err)
	}
	v := video[0]
	if plan.Mode == "x265_preserve" && (norm(v.Codec) == "hevc" || norm(v.Codec) == "h265") {
		return &transcode.PolicyRejectionError{ReasonCode: "already_hevc", Message: "source is already HEVC; no conversion required"}
	}
	if plan.Compact != nil {
		dr, err := classifySourceDynamicRange(ctx, w.ffprobePath, source, &rep)
		if err != nil {
			return fmt.Errorf("quality_source_evidence_missing: %v", err)
		}
		if err := plan.Compact.MatchSource(dr, v); err != nil {
			return &transcode.PolicyRejectionError{ReasonCode: "compact_source_changed", Message: err.Error()}
		}
		if _, err := ensureCompactLUT(plan.Compact); err != nil {
			return err
		}
	}
	// An audio-only plan copies the video untouched, so there is no frame to
	// measure; the source must still be the SDR class size mode admitted.
	if transcode.IsSizeAudioOnlyPlan(plan) {
		if mediainspect.IsHDRorDolbyVisionReport(&rep) || v.BitDepth != plan.ExpectedBitDepth {
			return &transcode.PolicyRejectionError{ReasonCode: "preservation_not_met", Message: "audio-only source no longer matches its frozen SDR plan"}
		}
		return nil
	}
	if _, err := quality.TemporalDecimationForStream(v.FPS, v.FieldOrder); err != nil || (plan.Compact == nil && mediainspect.IsHDRorDolbyVisionReport(&rep)) || (v.BitDepth != 8 && v.BitDepth != 10) || !isSupportedSourceChroma(v.PixelFormat, v.BitDepth) {
		return fmt.Errorf("quality_source_class_unsupported: native SDR 8/10-bit 4:2:0 evidence at a measurable frame rate (up to %.0f FPS) required", quality.HFRMaxFPS)
	}
	q := plan.QualityValidation
	if q == nil || q.Quality.VMAF == nil || q.Quality.VMAF.Model == "" || (q.Metric != "vmaf" && q.Metric != "both" && q.Metric != "vmaf+ssim") {
		return fmt.Errorf("quality_model_unavailable: mode requires its frozen VMAF policy")
	}
	caps, err := w.Capabilities(ctx)
	if err != nil {
		return err
	}
	if caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.Models[q.Quality.VMAF.Model].Available {
		return fmt.Errorf("quality_model_unavailable: requested executable model is unavailable")
	}
	if (v.BitDepth == 10 || plan.ExpectedBitDepth == 10) && !caps.SupportsNative10BitQuality() {
		return fmt.Errorf("quality_native_main10_unverified: executable native depth path is unavailable")
	}
	if !caps.SupportsQualityFPS(v.FPS, v.FieldOrder) {
		return fmt.Errorf("quality_source_class_unsupported: no proven decimated VMAF path for %.3f FPS source", v.FPS)
	}
	return nil
}

// modeCandidateBitDepth is the output depth a mode plan may produce: the
// source depth, except that size v3 encodes an 8-bit source as Main10 when
// its frozen plan says so.
func modeCandidateBitDepth(plan *transcode.Plan, sourceBitDepth int) int {
	if sourceBitDepth == 8 && plan.Mode == "size" && plan.ModePolicyVersion == optimization.ModePolicyV3 && plan.ExpectedBitDepth == 10 {
		return 10
	}
	return sourceBitDepth
}
