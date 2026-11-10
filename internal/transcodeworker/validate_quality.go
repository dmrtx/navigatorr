package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
		err := fmt.Errorf("%s: %w", code, cause)
		if code == "quality_final_sample_misaligned" {
			// The same source and encode cut the same way misalign again, and
			// the encode that precedes this check runs for hours, so the job is
			// never replayed on its own; a manual retry re-encodes.
			err = candidateInvalid(err)
		}
		return e, err
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
		return fail("quality_final_source_probe_failed", probeFailure("source video probe", err, len(sourceVideoStreams)))
	}
	candidateRep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, candidate)
	candidateVideoStreams := candidateRep.MainVideo()
	if err != nil || len(candidateVideoStreams) != 1 {
		return fail("quality_final_candidate_probe_failed", probeFailure("candidate video probe", err, len(candidateVideoStreams)))
	}
	var compactChain, compactCandidateChain string
	if plan.Compact != nil {
		// Compact converts HDR/DV to SDR, so the source may be HDR, but only
		// exactly as frozen; the candidate must be clean SDR at the target size
		// and is measured against the source through the identical chain. A
		// keep-HDR candidate must carry exactly the frozen HDR signalling, and
		// both pictures are measured through the same tone map.
		sourceVideo, candidateVideo := sourceVideoStreams[0], candidateVideoStreams[0]
		dr, err := classifySourceDynamicRange(ctx, w.ffprobePath, source, &sourceRep)
		if err != nil {
			return fail("quality_source_class_unsupported", err)
		}
		if err := plan.Compact.MatchSource(dr, sourceVideo); err != nil {
			return fail("quality_source_class_unsupported", err)
		}
		if (sourceVideo.BitDepth != 8 && sourceVideo.BitDepth != 10) || (candidateVideo.BitDepth != 8 && candidateVideo.BitDepth != 10) || !sourceChromaAdmitted(plan, sourceVideo.PixelFormat, sourceVideo.BitDepth) || !isSupportedSourceChroma(candidateVideo.PixelFormat, candidateVideo.BitDepth) {
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
		if compactChain, compactCandidateChain, err = compactMeasurementChains(plan.Compact); err != nil {
			return fail("quality_compact_chain_unavailable", err)
		}
	} else if plan.Mode != "" {
		sourceVideo, candidateVideo := sourceVideoStreams[0], candidateVideoStreams[0]
		if mediainspect.IsHDRorDolbyVisionReport(&sourceRep) || mediainspect.IsHDRorDolbyVisionReport(&candidateRep) || (sourceVideo.BitDepth != 8 && sourceVideo.BitDepth != 10) || candidateVideo.BitDepth != modeCandidateBitDepth(plan, sourceVideo.BitDepth) || !sourceChromaAdmitted(plan, sourceVideo.PixelFormat, sourceVideo.BitDepth) || !isSupportedSourceChroma(candidateVideo.PixelFormat, candidateVideo.BitDepth) {
			return fail("quality_source_class_unsupported", fmt.Errorf("source/candidate measurement class is unsupported or changed"))
		}
	}
	e.SourceBitDepth = sourceVideoStreams[0].BitDepth
	// VMAF and CAMBI read luma only, so a source beyond 4:2:0, whose chroma
	// the encode subsamples, also has its colour planes checked.
	runChroma := !isSupportedSourceChroma(sourceVideoStreams[0].PixelFormat, sourceVideoStreams[0].BitDepth)
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
	// Windows are cut from each file's first picture, which the encode keeps
	// as its own first picture whatever the containers say their streams start at.
	sourceFirstFrame, err := probeFirstFrameTime(ctx, w.ffprobePath, source, sourceVideoStreams[0].Index)
	if err != nil {
		return fail("quality_final_source_timing_unavailable", err)
	}
	candidateFirstFrame, err := probeFirstFrameTime(ctx, w.ffprobePath, candidate, candidateVideoStreams[0].Index)
	if err != nil {
		return fail("quality_final_candidate_probe_failed", err)
	}
	// The grid windows decide the verdict. Fresh windows (rules from the
	// plan's own verification) were never part of the grid; they are measured
	// after it and reported separately. A plan that gates them
	// (GateFreshSamples) holds them to everything a grid window is held to;
	// otherwise they only report, and one that cannot be measured is listed
	// rather than failing (see measureFreshWindows).
	grid := len(q.Samples)
	windows := append(append([]transcode.BenchmarkSampleWindow(nil), q.Samples...), q.FreshSamples...)
	concurrency := w.finalValidationConcurrency(len(windows))
	threads := w.metricThreads(concurrency)
	measure := func(ctx context.Context, pos int) finalWindowResult {
		window := windows[pos]
		bad := func(code string, cause error) finalWindowResult {
			return finalWindowResult{code: code, err: cause}
		}
		// A fresh window the source has no pictures for (past the end of its
		// container, or an extract in which ffmpeg encodes nothing) has
		// nothing to hold the candidate to: it is unmeasured, never a refusal
		// (noSource). Only the source's own extract says so: the containers'
		// durations follow their longest stream, not their video.
		sourceless := func(code string, cause error) finalWindowResult {
			return finalWindowResult{code: code, err: cause, noSource: pos >= grid}
		}
		if end := window.StartSeconds + window.DurationSeconds; window.StartSeconds < 0 || window.DurationSeconds <= 0 || end > sourceRep.DurationSec+0.05 {
			return sourceless("quality_final_sample_misaligned", fmt.Errorf("window %d exceeds the source's timeline", window.Index))
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
		// One set of bounds, on the source's frame grid, for both files.
		from, to := sampleFrameBounds(window.StartSeconds, window.DurationSeconds, sourceVideoStreams[0].FPS)
		// Which window failed and where it sits tells a gap in the source's
		// timeline from a shifted encode.
		where := fmt.Sprintf("window %d (starts %.3fs after the first picture, %.0fs long)", window.Index, window.StartSeconds, window.DurationSeconds)
		// The source's extract comes first. One in which ffmpeg encodes no
		// picture is a window past the source's video. Any other failure
		// (a read error, a killed process, an extract ffprobe cannot read)
		// is retried once and then fails the window: it is not a window
		// without pictures, and a gate must not pass on windows it never
		// measured.
		refArgs := BuildReferenceExtractionArgs(source, refPath, sourceVideoStreams[0].Index, sourceFirstFrame, from, to)
		if compactChain != "" {
			refArgs = withReferenceChain(refArgs, compactChain)
		}
		var refPTS []float64
		for attempt := 1; ; attempt++ {
			output, err := runQualityCommand(ctx, w.ffmpegPath, refArgs)
			if err == nil && emptyExtract(output) {
				return sourceless("quality_final_sample_misaligned", fmt.Errorf("%s: the source has no pictures there", where))
			}
			if err == nil {
				if refPTS, err = probeSamplePTS(ctx, w.ffprobePath, refPath); err == nil {
					break
				}
			}
			if attempt == 2 || ctx.Err() != nil {
				return bad("quality_final_sample_extract_failed", fmt.Errorf("%s, source extract (%d attempts): %w", where, attempt, err))
			}
		}
		candArgs := BuildReferenceExtractionArgs(candidate, candPath, candidateVideoStreams[0].Index, candidateFirstFrame, from, to)
		if compactCandidateChain != "" {
			candArgs = withReferenceChain(candArgs, compactCandidateChain)
		}
		if _, err := runQualityCommand(ctx, w.ffmpegPath, candArgs); err != nil {
			return bad("quality_final_sample_extract_failed", err)
		}
		candPTS, err := probeSamplePTS(ctx, w.ffprobePath, candPath)
		if err != nil {
			return bad("quality_final_sample_misaligned", fmt.Errorf("%s, candidate extract: %w", where, err))
		}
		if err := verifySampleAlignment(refPTS, candPTS, sourceVideoStreams[0].FPS); err != nil {
			return bad("quality_final_sample_misaligned", fmt.Errorf("%s: %w", where, err))
		}
		if err := verifyContentAlignment(ctx, w.ffmpegPath, candPath, refPath, threads); err != nil {
			return bad("quality_final_sample_misaligned", fmt.Errorf("sample %d: %w", window.Index, err))
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
		if runChroma && (pos < grid || q.GateFreshSamples) {
			u, v, err := windowChromaPSNR(ctx, w.ffmpegPath, candPath, refPath, threads)
			if err != nil {
				return bad("quality_chroma_evidence_incomplete", fmt.Errorf("sample %d: %w", window.Index, err))
			}
			r.chroma = &[2]float64{u, v}
		}
		return r
	}
	results, failed, err := runFinalWindows(ctx, len(windows), concurrency, measureFreshWindows(q, measure))
	if err != nil {
		return e, err
	}
	// Only a verdict window fails here: a fresh window measureFreshWindows
	// marked unmeasured is reported, not failed.
	if failed >= 0 {
		return fail(results[failed].code, results[failed].err)
	}
	results, scores, freshVMAF, unmeasured := splitFinalWindowResults(q, results)
	e.Windows = scores
	e.FreshUnmeasured = unmeasured
	if len(freshVMAF) > 0 {
		if combined, err := quality.CombineVMAF(freshVMAF); err == nil {
			e.FreshVMAF = &combined
		}
	}
	return judgeFinalWindows(e, q, results, runVMAF, runCAMBI, runSSIM, runChroma)
}

// judgeFinalWindows pools the verdict windows' measurements (the grid, and the
// fresh windows of a plan that gates them) and checks them against the plan's
// limits, filling e. Every window in results was measured.
func judgeFinalWindows(e *transcode.FinalQualityEvidence, q *transcode.QualityValidationPlan, results []finalWindowResult, runVMAF, runCAMBI, runSSIM, runChroma bool) (*transcode.FinalQualityEvidence, error) {
	fail := func(code string, cause error) (*transcode.FinalQualityEvidence, error) {
		e.ReasonCodes = append(e.ReasonCodes, code)
		return e, fmt.Errorf("%s: %w", code, cause)
	}
	var vmafSamples []quality.VMAFStats
	var cambiSamples []quality.CAMBIStats
	var ssimScores []float64
	var chroma *transcode.ChromaPSNREvidence
	if runChroma {
		chroma = &transcode.ChromaPSNREvidence{MinimumDB: chromaPSNRMinimumDB, WorstUDB: math.Inf(1), WorstVDB: math.Inf(1)}
	}
	for _, r := range results {
		if r.chroma != nil {
			chroma.WorstUDB = math.Min(chroma.WorstUDB, r.chroma[0])
			chroma.WorstVDB = math.Min(chroma.WorstVDB, r.chroma[1])
		}
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
	if chroma != nil {
		if math.IsInf(chroma.WorstUDB, 1) || math.IsInf(chroma.WorstVDB, 1) {
			return fail("quality_chroma_evidence_incomplete", fmt.Errorf("no window measured its colour planes"))
		}
		e.ChromaPSNR = chroma
		if chroma.WorstUDB < chroma.MinimumDB || chroma.WorstVDB < chroma.MinimumDB {
			e.ReasonCodes = append(e.ReasonCodes, "quality_chroma_psnr_below_minimum")
		}
	}
	if len(e.ReasonCodes) > 0 {
		return e, fmt.Errorf("final perceptual quality rejected: %v", e.ReasonCodes)
	}
	e.Verdict = "pass"
	return e, nil
}

// measureFreshWindows wraps a window measurement for runFinalWindows. A grid
// window, or a fresh window of a plan that gates them, keeps its failure, which
// fails final validation as the lowest failing window. An ungated fresh window
// that cannot be measured is marked unmeasured instead: it does not stop the
// windows after it, and splitFinalWindowResults reports it with its reason.
// Its scores never decide the verdict, but the candidate's pictures there
// do: a window whose candidate pictures do not line up with the source's
// (freshWindowContentCodes) fails like a grid one. Fresh windows come from the
// plan's verification, not from the grid the source was checked against
// before the encode, so one the source has no pictures for (noSource) is
// unmeasured whether or not the plan gates them: it says nothing about the
// candidate, and refusing on it would throw the encode away. Any other
// failure of a gated window, the source's extract included, fails: a gate
// never passes on a window it could not measure.
func measureFreshWindows(q *transcode.QualityValidationPlan, measure func(context.Context, int) finalWindowResult) func(context.Context, int) finalWindowResult {
	grid := len(q.Samples)
	return func(ctx context.Context, pos int) finalWindowResult {
		r := measure(ctx, pos)
		if pos < grid || r.code == "" {
			return r
		}
		if r.noSource || (!q.GateFreshSamples && !freshWindowContentCodes[r.code]) {
			return finalWindowResult{unmeasured: r.code}
		}
		return r
	}
}

// freshWindowContentCodes are the window failures that say the candidate's
// pictures are out of place, not that a score could not be taken. A
// misalignment counts only once the source's extract is known good: a source
// with no pictures there is noSource, and a source extract that failed twice
// is quality_final_sample_extract_failed, which fails a gated plan and leaves
// an ungated window unmeasured.
var freshWindowContentCodes = map[string]bool{
	"quality_final_sample_misaligned": true,
}

// splitFinalWindowResults separates the results that decide the verdict (the
// grid's, and the measured fresh windows' when the plan gates them) from the
// report.
// Every window was measured or, for an ungated fresh window, marked
// unmeasured. It returns the verdict results, every measured window's scores
// in time order, the fresh windows' VMAF statistics and the ungated fresh
// windows that could not be measured.
func splitFinalWindowResults(q *transcode.QualityValidationPlan, results []finalWindowResult) ([]finalWindowResult, []transcode.FinalWindowScore, []quality.VMAFStats, []transcode.UnmeasuredWindow) {
	grid := len(q.Samples)
	var windows []transcode.FinalWindowScore
	var freshVMAF []quality.VMAFStats
	var unmeasured []transcode.UnmeasuredWindow
	for i, r := range results {
		role, window := transcode.WindowRoleFresh, transcode.BenchmarkSampleWindow{}
		if i < grid {
			role, window = "grid", q.Samples[i]
		} else {
			window = q.FreshSamples[i-grid]
		}
		if r.unmeasured != "" {
			unmeasured = append(unmeasured, transcode.UnmeasuredWindow{StartSeconds: window.StartSeconds, DurationSeconds: window.DurationSeconds, ReasonCode: r.unmeasured})
			continue
		}
		if r.vmaf == nil {
			continue
		}
		if i >= grid {
			freshVMAF = append(freshVMAF, *r.vmaf)
		}
		score := finalWindowScore(window, role, r.vmaf)
		if r.chroma != nil {
			score.ChromaUDB, score.ChromaVDB = &r.chroma[0], &r.chroma[1]
		}
		windows = append(windows, score)
	}
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].StartSeconds < windows[j].StartSeconds })
	verdict := results[:grid:grid]
	if q.GateFreshSamples {
		for _, r := range results[grid:] {
			if r.unmeasured == "" {
				verdict = append(verdict, r)
			}
		}
	}
	return verdict, windows, freshVMAF, unmeasured
}

// finalWindowScore is one measured window's VMAF scores for the report.
func finalWindowScore(window transcode.BenchmarkSampleWindow, role string, v *quality.VMAFStats) transcode.FinalWindowScore {
	return transcode.FinalWindowScore{StartSeconds: window.StartSeconds, DurationSeconds: window.DurationSeconds, Role: role, VMAFMean: v.Mean, VMAFP5: v.P5, VMAFMin: v.Min}
}

// maxFinalValidationConcurrency bounds how many final validation windows are
// extracted and measured at once.
var maxFinalValidationConcurrency = 3

// finalWindowResult is one window's measurement, or the reason it failed.
type finalWindowResult struct {
	vmaf  *quality.VMAFStats
	cambi *quality.CAMBIStats
	ssim  *float64
	// chroma is the window's U and V PSNR (runChroma).
	chroma *[2]float64
	code   string
	err    error
	// unmeasured is the reason code of a fresh window that could not be
	// measured (measureFreshWindows); such a result carries nothing else.
	unmeasured string
	// noSource marks a fresh window's failure on the source's side: no
	// source pictures there to compare the candidate with.
	noSource bool
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
	return max(1, min(maxFinalValidationConcurrency, windows, w.cpuBudget()/jobs/perPass))
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

// emptyExtract reports whether an extract that ffmpeg finished without an
// error holds no picture: a window placed past the file's video, where the
// trim filter lets nothing through. ffmpeg says so in these words.
func emptyExtract(output []byte) bool {
	return strings.Contains(string(output), "Output file is empty, nothing was encoded")
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

// firstFrameProbeIntervals are the stretches of the file probeFirstFrameTime
// decodes, the cheapest first: 32 packets return the first picture of any
// file that starts on a keyframe, even past a decoder that holds pictures
// back for reordering. A capture cut mid-GOP decodes nothing until its first
// keyframe, which can be many packets in, so the probe then reads 30 s.
var firstFrameProbeIntervals = []string{"%+#32", "%+30"}

// probeFirstFrameTime is the timestamp of the first picture a video stream
// decodes to. It is not the stream's start time: packed MPEG-4 B-frames in AVI
// hold every picture back one packet, so the first picture is stamped a frame
// after the stream starts, while its encode starts on that picture.
func probeFirstFrameTime(ctx context.Context, ffprobePath, path string, videoIndex int) (float64, error) {
	for _, interval := range firstFrameProbeIntervals {
		cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(videoIndex), "-read_intervals", interval, "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", path)
		output, err := cmd.Output()
		chargeCPU(ctx, cmd)
		if err != nil {
			return 0, fmt.Errorf("ffprobe first frame timestamp: %w", err)
		}
		var log struct {
			Frames []struct {
				PTS string `json:"best_effort_timestamp_time"`
			} `json:"frames"`
		}
		if err := json.Unmarshal(output, &log); err != nil {
			return 0, err
		}
		// The decoder flush at the end of the interval may return a picture
		// out of order or without a timestamp; the first picture is the
		// earliest.
		first := math.Inf(1)
		for _, frame := range log.Frames {
			if v, err := strconv.ParseFloat(frame.PTS, 64); err == nil && isFiniteQuality(v) {
				first = math.Min(first, v)
			}
		}
		if !math.IsInf(first, 1) {
			return first, nil
		}
	}
	return 0, fmt.Errorf("no timestamped video frame at the start of the file")
}

// leadingTimestampCount is how many timestamps of each extract an alignment
// failure reports: enough to see an offset, a gap or a duplicated picture at
// the start without printing a whole window.
const leadingTimestampCount = 5

// leadingTimestamps formats the first timestamps of an extract, in seconds from
// its own first picture, and its last one, so a failure shows where each side's
// pictures start and how long the extract runs.
func leadingTimestamps(pts []float64) string {
	if len(pts) == 0 {
		return "[]"
	}
	n := min(len(pts), leadingTimestampCount)
	parts := make([]string, 0, n+1)
	for _, v := range pts[:n] {
		parts = append(parts, strconv.FormatFloat(v, 'f', 3, 64))
	}
	text := "[" + strings.Join(parts, " ")
	if len(pts) > n {
		text += fmt.Sprintf(" … %s", strconv.FormatFloat(pts[len(pts)-1], 'f', 3, 64))
	}
	return text + "]"
}

func verifySampleAlignment(ref, cand []float64, fps float64) error {
	if len(ref) != len(cand) || len(ref) == 0 {
		return fmt.Errorf("source/candidate frame count mismatch: %d versus %d (source timestamps %s, candidate timestamps %s)", len(ref), len(cand), leadingTimestamps(ref), leadingTimestamps(cand))
	}
	tolerance := 0.25 / fps
	for i := range ref {
		if math.Abs((ref[i]-ref[0])-(cand[i]-cand[0])) > tolerance {
			return fmt.Errorf("source/candidate timestamp mismatch at frame %d: source %.3f, candidate %.3f (source timestamps %s, candidate timestamps %s)", i, ref[i]-ref[0], cand[i]-cand[0], leadingTimestamps(ref), leadingTimestamps(cand))
		}
	}
	return nil
}

// contentAlignmentFrames is how many leading frames verifyContentAlignment
// compares, and contentAlignmentMarginDB how much closer a neighbouring frame
// must match than the aligned one before the window counts as shifted.
const (
	contentAlignmentFrames   = 12
	contentAlignmentMarginDB = 3.0
)

var psnrAverage = regexp.MustCompile(`PSNR .*average:(inf|[0-9]+(?:\.[0-9]+)?)`)

// verifyContentAlignment checks that the two extracts start on the same
// picture. Timestamps cannot show this: both extracts start at zero, so an
// encode that lost one frame before the window pairs picture N+1 with N at
// perfectly regular spacing, and every metric then reads it as damage. The
// first frames are compared at offsets -1, 0 and +1; a neighbour that matches
// clearly better than the aligned pair means the window is shifted.
func verifyContentAlignment(ctx context.Context, ffmpegPath, candPath, refPath string, threads int) error {
	aligned, err := leadingFramesPSNR(ctx, ffmpegPath, candPath, refPath, 0, threads)
	if err != nil {
		return err
	}
	for _, offset := range []int{-1, 1} {
		shifted, err := leadingFramesPSNR(ctx, ffmpegPath, candPath, refPath, offset, threads)
		if err != nil {
			return err
		}
		if shifted > aligned+contentAlignmentMarginDB {
			return fmt.Errorf("candidate is shifted %+d frame against the source (PSNR %.1f dB shifted, %.1f dB aligned)", offset, shifted, aligned)
		}
	}
	return nil
}

// leadingFramesPSNR is the mean PSNR of candidate frame i+offset against
// reference frame i over the first contentAlignmentFrames pairs. Frames are
// paired by index, not timestamp, so irregular spacing cannot re-pair them.
// threads is the window's share of the metric threads, as for VMAF.
func leadingFramesPSNR(ctx context.Context, ffmpegPath, candPath, refPath string, offset, threads int) (float64, error) {
	pick := func(skip int) string {
		return fmt.Sprintf("trim=start_frame=%d:end_frame=%d,setpts=N/(10*TB)", skip, skip+contentAlignmentFrames)
	}
	candSkip, refSkip := max(offset, 0), max(-offset, 0)
	graph := fmt.Sprintf("[0:v:0]%s[c];[1:v:0]%s[r];[c][r]psnr", pick(candSkip), pick(refSkip))
	t := strconv.Itoa(max(threads, 1))
	output, err := runQualityCommand(ctx, ffmpegPath, []string{"-nostdin", "-nostats", "-threads", t, "-i", candPath, "-threads", t, "-i", refPath, "-filter_complex_threads", t, "-filter_complex", graph, "-f", "null", "-"})
	if err != nil {
		return 0, err
	}
	m := psnrAverage.FindStringSubmatch(string(output))
	if m == nil {
		return 0, fmt.Errorf("alignment PSNR missing from ffmpeg output")
	}
	if m[1] == "inf" {
		return 100, nil
	}
	return strconv.ParseFloat(m[1], 64)
}

// chromaPSNRMinimumDB is the lowest window U or V PSNR a candidate from a
// source beyond 4:2:0 may have. On 4:4:4 10-bit film, sound x265 encodes at
// CRF 22 to 28 scored 45 dB or more per window and no frame fell below 43 dB,
// while colour planes moved by two pixels scored 31 to 33 dB and swapped
// planes about 21 dB. A synthetic test pattern, whose colour edges are far
// sharper than film's, still scored 38 dB once encoded soundly, so the
// minimum sits below that. A wrong matrix or range barely moves chroma PSNR
// (and moves luma, which VMAF reads): this catches misplaced, swapped or
// smeared colour, which nothing else measures.
const chromaPSNRMinimumDB = 35.0

// chromaPSNRSummary is the psnr filter's closing summary line, which ffmpeg
// logs under the filter's own instance name.
var chromaPSNRSummary = regexp.MustCompile(`(?m)^\[Parsed_psnr_[0-9]+ @ [^\]]+\] PSNR y:\S+ u:(inf|[0-9]+(?:\.[0-9]+)?) v:(inf|[0-9]+(?:\.[0-9]+)?) `)

// windowChromaPSNR is the U and V PSNR of a window's candidate against its
// reference, both converted to 4:2:0 at measurement precision and paired by
// frame index as for VMAF, so the source's extra chroma resolution is
// subsampled the same way the encode's is and only a real difference counts.
// Every frame is measured, and threads is the window's share as for VMAF.
func windowChromaPSNR(ctx context.Context, ffmpegPath, candPath, refPath string, threads int) (u, v float64, err error) {
	inputs, err := quality.MetricPairInputs(1)
	if err != nil {
		return 0, 0, err
	}
	t := strconv.Itoa(max(threads, 1))
	output, err := runQualityCommand(ctx, ffmpegPath, []string{"-nostdin", "-nostats", "-threads", t, "-i", candPath, "-threads", t, "-i", refPath, "-filter_complex_threads", t, "-filter_complex", inputs + "psnr", "-f", "null", "-"})
	if err != nil {
		return 0, 0, err
	}
	return parseChromaPSNR(string(output))
}

func parseChromaPSNR(output string) (u, v float64, err error) {
	all := chromaPSNRSummary.FindAllStringSubmatch(output, -1)
	if len(all) == 0 {
		return 0, 0, fmt.Errorf("chroma PSNR missing from ffmpeg output")
	}
	// The summary closes the run; anything that looks like one earlier is not it.
	m := all[len(all)-1]
	planes := [2]float64{}
	for i, s := range m[1:] {
		if s == "inf" {
			planes[i] = 100
			continue
		}
		if planes[i], err = strconv.ParseFloat(s, 64); err != nil {
			return 0, 0, err
		}
	}
	return planes[0], planes[1], nil
}

// probeFailure describes a probe that gave no usable video. The probe's own
// error is wrapped, not formatted, so its typed class survives for the
// classifier: the message carries file paths and ffprobe's stderr, and a
// classifier that reads it can match a word in a path.
func probeFailure(what string, err error, videoStreams int) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %d main video streams, want exactly 1", what, videoStreams)
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
		return fmt.Errorf("quality_source_evidence_missing: %w", probeFailure("source class unavailable", err, len(video)))
	}
	v := video[0]
	if plan.Mode == "x265_preserve" && (norm(v.Codec) == "hevc" || norm(v.Codec) == "h265") {
		return &transcode.PolicyRejectionError{ReasonCode: "already_hevc", Message: "source is already HEVC; no conversion required"}
	}
	if plan.Compact != nil {
		dr, err := classifySourceDynamicRange(ctx, w.ffprobePath, source, &rep)
		if err != nil {
			return fmt.Errorf("quality_source_evidence_missing: %w", err)
		}
		if err := plan.Compact.MatchSource(dr, v); err != nil {
			return &transcode.PolicyRejectionError{ReasonCode: "compact_source_changed", Message: err.Error()}
		}
		if _, err := ensureCompactLUT(plan.Compact); err != nil {
			return err
		}
	}
	// An audio-only plan copies the video untouched, so there is no frame to
	// measure; the source must still be the class size mode admitted. The
	// audio-only goal (v7) admits HDR and Dolby Vision, whose signalling the
	// copy proof then compares; the fallback of the other size versions only
	// ever planned SDR.
	if transcode.IsSizeAudioOnlyPlan(plan) {
		if plan.ModePolicyVersion != transcode.ModePolicyV7 && mediainspect.IsHDRorDolbyVisionReport(&rep) || v.BitDepth != plan.ExpectedBitDepth {
			return &transcode.PolicyRejectionError{ReasonCode: "preservation_not_met", Message: "audio-only source no longer matches its frozen plan"}
		}
		return nil
	}
	if _, err := quality.TemporalDecimationForStream(v.FPS, v.FieldOrder); err != nil || (plan.Compact == nil && mediainspect.IsHDRorDolbyVisionReport(&rep)) || (v.BitDepth != 8 && v.BitDepth != 10) || !sourceChromaAdmitted(plan, v.PixelFormat, v.BitDepth) {
		return fmt.Errorf("quality_source_class_unsupported: native SDR 8/10-bit 4:2:0, 4:2:2 or 4:4:4 evidence at a measurable frame rate (up to %.0f FPS) required", quality.HFRMaxFPS)
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
	if sourceBitDepth == 8 && plan.Mode == "size" && transcode.IsSizePolicyVersion(plan.ModePolicyVersion) && plan.ExpectedBitDepth == 10 {
		return 10
	}
	return sourceBitDepth
}
