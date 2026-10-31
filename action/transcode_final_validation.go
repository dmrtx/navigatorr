package action

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// A ladder's encodes are judged by the worker's final gate on the windows and
// quality limits the goal froze. They are kept as a transcode.BenchmarkRequest,
// the request type the worker's gate already names, under the state keys
// benchmark_request and benchmark_request_digest. The request is never
// submitted as a job; its digest binds every rung's plan to it.

// finalValidationJobID names the request. It is an identity only: nothing is
// submitted under it.
func finalValidationJobID(ec *ExecutionContext) (string, error) {
	id := fmt.Sprintf("bench-%s", ec.InstanceID)
	if len(id) > transcode.MaxBenchmarkIDLength {
		id = id[:transcode.MaxBenchmarkIDLength]
	}
	if err := transcode.ValidateBenchmarkJobID(id); err != nil {
		return "", fmt.Errorf("generated final validation id %q is invalid: %w", id, err)
	}
	return id, nil
}

// buildFinalSamples places the sample windows the source's video allows.
func buildFinalSamples(rep *mediainspect.DetailedReport, optSampling *recipe.SamplingPolicy) ([]transcode.BenchmarkSampleWindow, error) {
	if rep.WindowSeconds() <= 0 {
		return nil, fmt.Errorf("invalid source duration %.3f: must be positive", rep.WindowSeconds())
	}
	samplePlan, err := transcode.PlanSamples(transcode.SamplePlanConfig{
		Duration:          rep.WindowSeconds(),
		SampleSeconds:     optSampling.SampleSeconds,
		SampleCount:       optSampling.SampleCount,
		Positions:         optSampling.Positions,
		RelativePositions: true,
	})
	if err != nil {
		return nil, fmt.Errorf("planning final validation samples: %w", err)
	}
	samples := make([]transcode.BenchmarkSampleWindow, 0, len(samplePlan.Samples))
	for _, sw := range samplePlan.Samples {
		samples = append(samples, transcode.BenchmarkSampleWindow{
			Index:           sw.Index,
			StartSeconds:    sw.StartSeconds,
			DurationSeconds: sw.DurationSeconds,
			CenterSeconds:   sw.CenterSeconds,
		})
	}
	return samples, nil
}

// finalSampleWindows are the windows a source gets when the goal has no
// final validation grid of its own: its three planned windows shortened to at
// most 10 seconds, with two more at 35% and 65% of the source. A source under
// 50 seconds keeps the planned windows.
func finalSampleWindows(duration float64, samples []transcode.BenchmarkSampleWindow) ([]transcode.BenchmarkSampleWindow, error) {
	if duration < 50 {
		return samples, nil
	}
	if len(samples) != 3 {
		return nil, fmt.Errorf("final validation requires three planned windows")
	}
	seconds := min(10.0, duration/12)
	for i := range samples {
		samples[i].DurationSeconds = seconds
		samples[i].StartSeconds = samples[i].CenterSeconds - seconds/2
	}
	for _, center := range []float64{duration * 0.35, duration * 0.65} {
		samples = append(samples, transcode.BenchmarkSampleWindow{Index: len(samples), StartSeconds: center - seconds/2, DurationSeconds: seconds, CenterSeconds: center})
	}
	return samples, nil
}

func buildFinalQualityConfig(optQuality *recipe.QualityPolicy) *transcode.BenchmarkQualityConfig {
	if optQuality == nil {
		return nil
	}
	qc := &transcode.BenchmarkQualityConfig{
		PreferredMetric: optQuality.PreferredMetric,
	}
	if optQuality.VMAF != nil {
		qc.VMAF = &transcode.BenchmarkQualityThresholds{
			Target:                  optQuality.VMAF.Target,
			Minimum:                 optQuality.VMAF.Minimum,
			MarginalTolerance:       optQuality.VMAF.MarginalTolerance,
			Model:                   optQuality.VMAF.Model,
			GuardrailEnforcement:    optQuality.VMAF.GuardrailEnforcement,
			P5Minimum:               optQuality.VMAF.P5Minimum,
			WorstWindowMinimum:      optQuality.VMAF.WorstWindowMinimum,
			WorstWindowSeconds:      optQuality.VMAF.WorstWindowSeconds,
			FrameThreshold:          optQuality.VMAF.FrameThreshold,
			MaxFramesBelowThreshold: optQuality.VMAF.MaxFramesBelowThreshold,
		}
	}
	if optQuality.Banding != nil {
		b := optQuality.Banding
		qc.Banding = &transcode.BenchmarkBandingConfig{Enabled: b.Enabled, Metric: b.Metric, Mode: b.Mode, Enforcement: b.Enforcement, MaxMean: b.MaxMean, MaxPeak: b.MaxPeak}
	}
	if optQuality.FinalValidation != nil {
		qc.FinalValidation = &transcode.BenchmarkFinalValidationConfig{Mode: optQuality.FinalValidation.Mode}
	}
	if optQuality.SSIM != nil {
		qc.SSIM = &transcode.BenchmarkQualityThresholds{
			Target:            optQuality.SSIM.Target,
			Minimum:           optQuality.SSIM.Minimum,
			MarginalTolerance: optQuality.SSIM.MarginalTolerance,
		}
	}
	return qc
}

func ensureFinalQualityMetric(qc *transcode.BenchmarkQualityConfig, metric string) *transcode.BenchmarkQualityConfig {
	if qc == nil {
		qc = &transcode.BenchmarkQualityConfig{}
	}
	if metric == "vmaf" || metric == "ssim" {
		qc.PreferredMetric = metric
	}
	if (metric == "vmaf" || metric == "both") && qc.VMAF == nil {
		tolerance := recipe.DefaultVMAFMarginalTolerance
		qc.VMAF = &transcode.BenchmarkQualityThresholds{Target: recipe.DefaultVMAFTarget, Minimum: recipe.DefaultVMAFMinimum, MarginalTolerance: &tolerance}
	}
	if (metric == "ssim" || metric == "both") && qc.SSIM == nil {
		tolerance := recipe.DefaultSSIMMarginalTolerance
		qc.SSIM = &transcode.BenchmarkQualityThresholds{Target: recipe.DefaultSSIMTarget, Minimum: recipe.DefaultSSIMMinimum, MarginalTolerance: &tolerance}
	}
	return qc
}

// finalValidationMetric is the goal's quality metric: its policy's preferred
// one, VMAF when it names none.
func finalValidationMetric(optQuality *recipe.QualityPolicy) (string, error) {
	if optQuality != nil && optQuality.PreferredMetric != "" {
		norm := strings.ToLower(strings.TrimSpace(optQuality.PreferredMetric))
		if norm != "vmaf" && norm != "ssim" {
			return "", fmt.Errorf("invalid preferred_metric %q: must be 'vmaf' or 'ssim' (fail closed)", optQuality.PreferredMetric)
		}
		return norm, nil
	}
	return "vmaf", nil
}

func validateWorkerCapabilitiesForFinalValidation(caps transcode.WorkerCapabilities, metric string, sourceBitDepth int, codec string) error {
	if caps.ProtocolVersion != transcode.WorkerProtocolVersion {
		return fmt.Errorf("worker protocol version %d does not match expected %d (fail closed)",
			caps.ProtocolVersion, transcode.WorkerProtocolVersion)
	}
	normCodec := transcode.NormalizeVideoCodec(codec)
	if normCodec == "" {
		normCodec = transcode.VideoCodecHEVCVideoToolbox
	}
	if !caps.Encoders[normCodec] {
		return fmt.Errorf("required encoder %q is not available on worker (fail closed)", normCodec)
	}
	needVMAF := metric == "vmaf" || metric == "both"
	needSSIM := metric == "ssim" || metric == "both"
	if needVMAF && !caps.Filters["libvmaf"] {
		return errors.New("required filter 'libvmaf' is not available on worker (fail closed)")
	}
	if needSSIM && !caps.Filters["ssim"] {
		return errors.New("required filter 'ssim' is not available on worker (fail closed)")
	}
	if sourceBitDepth > 8 && needVMAF && !caps.SupportsNative10BitQuality() {
		return fmt.Errorf("worker capability unsupported: source media has bit depth %d (> 8-bit) but worker does not have verified 10-bit VMAF capability; silent 8-bit downconversion is prohibited (fail closed)", sourceBitDepth)
	}
	return nil
}

// buildFinalValidationRequest is the metric, quality limits and sample windows
// the goal's encodes are judged on, after proving the worker can encode with
// codec, measure them for this source and run the plan's streams.
func buildFinalValidationRequest(ec *ExecutionContext, cleanPath string, rep *mediainspect.DetailedReport, opt *recipe.OptimizationPolicy, caps transcode.WorkerCapabilities, codec string) (*transcode.BenchmarkRequest, error) {
	video := rep.MainVideo()
	if len(video) == 0 {
		return nil, errors.New("media inspection report has no video streams (fail closed)")
	}
	v0 := video[0]
	bitDepth := v0.BitDepth
	if bitDepth != 8 && bitDepth != 10 {
		return nil, fmt.Errorf("unsupported bit depth %d: only 8-bit and 10-bit SDR content supported (fail closed)", bitDepth)
	}
	id, err := finalValidationJobID(ec)
	if err != nil {
		return nil, err
	}
	// A missing or unreadable plan fails closed: the worker's encoder must be
	// the one the goal's plan selected.
	plan := getPlan(ec.State["plan"])
	if plan == nil {
		return nil, errors.New("resolved plan is missing or unreadable from state (fail closed)")
	}
	samples, err := buildFinalSamples(rep, opt.Sampling)
	if err != nil {
		return nil, err
	}
	if samples, err = finalSampleWindows(rep.WindowSeconds(), samples); err != nil {
		return nil, err
	}
	metric, err := finalValidationMetric(opt.Quality)
	if err != nil {
		return nil, err
	}
	if opt.Quality != nil && opt.Quality.VMAF != nil && opt.Quality.VMAF.Model != "" {
		modelID := opt.Quality.VMAF.Model
		if caps.Quality != nil && caps.Quality.ProbeError != "" {
			return nil, fmt.Errorf("quality VMAF capability probe failed: %s", caps.Quality.ProbeError)
		}
		modelAvailable := caps.Quality != nil && caps.Quality.Models[modelID].Available
		// HFR is measurable only on a worker that proved decimated VMAF; this
		// mirrors the worker gate so it is never dispatched work it rejects.
		// Missing timing still fails later at measurement, as before.
		factor, fpsErr := quality.TemporalDecimationForStream(v0.FPS, v0.FieldOrder)
		fpsUnmeasurable := v0.FPS > 0 && fpsErr != nil || factor > 1 && !caps.SupportsHFRQuality()
		if (metric == "vmaf" || metric == "both") && (!modelAvailable || fpsUnmeasurable) {
			return nil, fmt.Errorf("quality_vmaf_model_unavailable: %s cannot execute for %.3f FPS source", modelID, v0.FPS)
		}
	}
	if opt.Quality != nil && opt.Quality.Banding != nil && opt.Quality.Banding.Enabled {
		if bitDepth > 8 && !caps.SupportsNative10BitQuality() {
			return nil, fmt.Errorf("quality_cambi_native_main10_unverified: native %d-bit source is not eligible for CAMBI", bitDepth)
		}
		if caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.CAMBIFullRef {
			return nil, fmt.Errorf("quality_cambi_full_ref_unavailable: worker cannot prove requested CAMBI mode")
		}
	}
	qualityBitDepth := max(bitDepth, plan.ExpectedBitDepth)
	if err := validateWorkerCapabilitiesForFinalValidation(caps, metric, qualityBitDepth, codec); err != nil {
		return nil, err
	}

	req := &transcode.BenchmarkRequest{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              id,
		SourcePath:      cleanPath,
		// The windows are planned on the video, which can end before a
		// container that a subtitle or attachment keeps running.
		SourceDuration: rep.WindowSeconds(),
		Metric:         metric,
		Samples:        samples,
		Quality:        ensureFinalQualityMetric(buildFinalQualityConfig(opt.Quality), metric),
		SourceSHA256:   getString(ec.State, "original_sha256"),
	}
	if mode := getString(ec.Inputs, "mode"); mode != "" {
		policy, err := freezeModePolicy(ec)
		if err != nil {
			return nil, err
		}
		req.Mode = mode
		req.ModePolicyVersion = policy.workerVersion()
	}
	if plan.X265 != nil && !caps.X265TwoPass {
		return nil, fmt.Errorf("worker does not support two-pass x265; upgrade before encoding")
	}
	if len(rep.CoverArt()) > 0 && !caps.CoverArt {
		return nil, fmt.Errorf("%s before encoding", errCoverArtWorker)
	}
	switch plan.AudioMode {
	case "he_aac":
		if !caps.HEAACAudio {
			return nil, fmt.Errorf("worker does not support HE-AAC audio; upgrade before encoding")
		}
	case "compact":
		if !caps.CompactAudio {
			return nil, fmt.Errorf("worker does not support compact audio; upgrade the worker before encoding")
		}
	case transcode.AudioModeCompactLossless:
		if !caps.SizeModeV3 {
			return nil, fmt.Errorf("worker does not support lossless audio compaction; upgrade the worker before encoding")
		}
	case transcode.AudioModeSizeAAC, transcode.AudioModeSizeHEAAC, transcode.AudioModeSizeAACBed, transcode.AudioModeSizeHEAACBed:
		if !caps.SizeAudioLevels {
			return nil, fmt.Errorf("worker does not support the size goal's audio levels; upgrade the worker before encoding")
		}
		if transcode.IsObjectBedAudioLevelMode(plan.AudioMode) && !caps.SizeAudioObjectBed {
			return nil, fmt.Errorf("%s before encoding", errAudioObjectBedWorker)
		}
	}
	if plan.Compact != nil && !caps.SupportsCompactTranscode() {
		return nil, fmt.Errorf("worker does not support compact SDR conversion; upgrade the worker before encoding")
	}
	if plan.Tracks != nil && !caps.TrackSelection {
		return nil, fmt.Errorf("%s before encoding", errTrackSelectionWorker)
	}
	return req, nil
}

// finalValidationInputs reads the request a ladder planned from state and
// lays out the windows its final gate measures. Size goals measure the
// frozen policy's grid and its fresh windows in place of the request's
// windows; other goals, and sources too short for a grid, keep the request's.
func finalValidationInputs(ec *ExecutionContext) (req *transcode.BenchmarkRequest, digest string, samples, fresh []transcode.BenchmarkSampleWindow, err error) {
	var request transcode.BenchmarkRequest
	if !reencode(ec.State["benchmark_request"], &request) || request.Quality == nil || request.Quality.FinalValidation == nil || request.Quality.FinalValidation.Mode != "sampled" {
		return nil, "", nil, nil, fmt.Errorf("the goal's final validation plan is missing")
	}
	digest, err = transcode.DigestBenchmarkRequest(&request)
	if err != nil || digest != getString(ec.State, "benchmark_request_digest") {
		return nil, "", nil, nil, fmt.Errorf("the final validation request changed")
	}
	samples = append([]transcode.BenchmarkSampleWindow(nil), request.Samples...)
	grid, fresh := sizeFinalWindowsWithFresh(ec, request.SourceDuration, frozenFinalFreshWindows(ec))
	if grid != nil {
		samples = grid
	}
	return &request, digest, samples, fresh, nil
}

// sizeFinalWindowsWithFresh plans size v3's final validation windows from the
// frozen policy: one per FinalWindowEverySeconds of source, within its bounds.
// It also places up to fresh windows no other window covers (see
// transcode.FreshValidationWindows). Other policies, and sources too short for
// a grid, get nil.
func sizeFinalWindowsWithFresh(ec *ExecutionContext, duration float64, fresh int) (grid, extra []transcode.BenchmarkSampleWindow) {
	if getString(ec.Inputs, "mode") != "size" {
		return nil, nil
	}
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil || p.Size == nil {
		return nil, nil
	}
	s := p.Size
	grid = transcode.PlanFinalValidationWindows(duration, float64(s.FinalWindowSeconds), float64(s.FinalWindowEverySeconds), s.FinalWindowsMinimum, s.FinalWindowsMaximum)
	if grid != nil && fresh > 0 {
		extra = transcode.FreshValidationWindows(grid, duration, float64(s.FinalWindowSeconds), fresh)
	}
	return grid, extra
}

// frozenFinalFreshWindows is how many fresh windows the frozen size policy asks
// its final gate to measure beside the grid, outside its verdict.
func frozenFinalFreshWindows(ec *ExecutionContext) int {
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil || p.Size == nil {
		return 0
	}
	if r := p.Size.AutomaticSearch; r != nil && r.HoldoutWindows == transcode.HoldoutWindowsFinalGrid {
		return r.FinalFreshWindows
	}
	return 0
}
