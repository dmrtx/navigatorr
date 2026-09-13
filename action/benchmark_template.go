package action

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func (e *Engine) registerBenchmarkTemplate() {
	e.RegisterTemplate(ActionTemplate{
		Name:        "benchmark_transcode",
		Version:     1,
		Description: "Coordinates safe candidate-only benchmark evaluation of encoder parameters over deterministic source samples, returning quality metrics, size estimates, and an explainable decision without modifying media or creating permanent candidates.",
		RequiredInputs: []string{"path"},
		OptionalInputs: []string{"profile", "metric", "replace_original", "surface_worker_busy"},
		Destructive:    false,
		Steps: []StepDefinition{
			{Name: "preflight", Description: "Inspect source, hash original, resolve profile and optimization policy", Run: e.stepTranscodePreflight},
			{Name: "submit_benchmark", Description: "Submit deterministic sample benchmark request to remote worker", Run: e.stepBenchmarkSubmit},
			{Name: "wait_benchmark", Description: "Monitor remote benchmark progress and collect evaluation decision", Run: e.stepBenchmarkWait},
		},
	})
}

func (e *Engine) stepBenchmarkSubmit(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if !getBool(ec.State, "optimization_enabled") {
		return StepResult{Status: StepSkipped}, nil
	}
	if getBool(ec.State, "skip_transcode") {
		return StepResult{Status: StepSkipped}, nil
	}
	if getBool(ec.State, "benchmark_submitted") || getString(ec.State, "benchmark_job_id") != "" {
		benchID := getString(ec.State, "benchmark_job_id")
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"benchmark_job_id": benchID,
				"reused":           true,
			},
		}, nil
	}
	if e.deps.Transcode == nil {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, nil
	}
	if wait := getString(ec.State, "benchmark_retry_not_before"); wait != "" {
		if at, err := time.Parse(time.RFC3339Nano, wait); err == nil && time.Now().Before(at) {
			return StepResult{
				Status:           StepWaitingExternal,
				WaitingCondition: "benchmark_retry",
				WaitingReason:    fmt.Sprintf("Benchmark retry is backed off until %s", at.UTC().Format(time.RFC3339)),
			}, nil
		}
		delete(ec.State, "benchmark_retry_not_before")
	}

	cleanPath := getString(ec.State, "resolved_path")
	if cleanPath == "" {
		cleanPath = getString(ec.Inputs, "path")
	}

	rep := getSourceReport(ec.State["source_report"])
	if rep == nil {
		rep = getSourceReport(ec.State["original"])
	}
	if rep == nil {
		return StepResult{Status: StepFailed, Error: "media inspection report is missing from state (fail closed)"}, nil
	}

	opt := getOptimizationPolicy(ec.State["optimization_policy"])
	if opt == nil || !opt.Enabled {
		return StepResult{Status: StepFailed, Error: "optimization policy is missing or disabled (fail closed)"}, nil
	}

	caps, err := e.deps.Transcode.Capabilities(ctx)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("querying worker capabilities: %v (fail closed)", err)}, nil
	}

	req, err := buildBenchmarkRequest(ec, cleanPath, rep, opt, caps)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}

	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("computing benchmark request digest: %v", err)}, nil
	}

	job, err := e.deps.Transcode.BenchmarkSubmit(ctx, *req)
	if err != nil {
		plan := getPlan(ec.State["plan"])
		return e.handleBenchmarkTransientFailure(ec, plan, err, "benchmark_submit")
	}

	ec.State["benchmark_submitted"] = true
	ec.State["benchmark_job_id"] = job.ID
	ec.State["benchmark_request_digest"] = digest
	ec.State["capability_fingerprint"] = caps.CapabilityFingerprint

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"benchmark_job_id":         job.ID,
			"benchmark_request_digest": digest,
			"capability_fingerprint":   caps.CapabilityFingerprint,
		},
	}, nil
}

func (e *Engine) stepBenchmarkWait(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if !getBool(ec.State, "optimization_enabled") {
		return StepResult{Status: StepSkipped}, nil
	}
	if getBool(ec.State, "skip_transcode") {
		return StepResult{Status: StepSkipped}, nil
	}

	benchJobID := getString(ec.State, "benchmark_job_id")
	if benchJobID == "" {
		return StepResult{Status: StepFailed, Error: "no active benchmark job ID to monitor"}, nil
	}
	if e.deps.Transcode == nil {
		return StepResult{Status: StepFailed, Error: "transcode executor is not available to monitor benchmark"}, nil
	}

	if wait := getString(ec.State, "benchmark_retry_not_before"); wait != "" {
		if at, err := time.Parse(time.RFC3339Nano, wait); err == nil && time.Now().Before(at) {
			return StepResult{
				Status:           StepWaitingExternal,
				WaitingCondition: "benchmark_retry",
				WaitingReason:    fmt.Sprintf("Benchmark retry is backed off until %s", at.UTC().Format(time.RFC3339)),
			}, nil
		}
		delete(ec.State, "benchmark_retry_not_before")
	}

	if ctx.Err() != nil {
		_ = e.deps.Transcode.BenchmarkCancel(context.Background(), benchJobID)
		return StepResult{Status: StepFailed, Error: ctx.Err().Error()}, nil
	}
	if ec.Decision == "cancel" {
		_ = e.deps.Transcode.BenchmarkCancel(ctx, benchJobID)
		return StepResult{Status: StepFailed, Error: "benchmark job was cancelled by decision"}, nil
	}

	plan := getPlan(ec.State["plan"])
	st, err := e.deps.Transcode.BenchmarkStatus(ctx, benchJobID)
	if err != nil {
		return e.handleBenchmarkTransientFailure(ec, plan, err, "benchmark_status")
	}

	switch st.Status {
	case transcode.StatusRunning, transcode.StatusQueued:
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "benchmark_complete",
			WaitingReason:    fmt.Sprintf("Benchmarking encoder configurations (%s, progress: %.1f%%)", st.Status, st.Progress),
			Outputs: map[string]any{
				"benchmark_job_id": benchJobID,
				"benchmark_status": st.Status,
				"progress":         st.Progress,
			},
		}, nil
	case transcode.StatusFailed:
		msg := st.Error
		if msg == "" {
			msg = "benchmark executor reported job failure"
		}
		class := resilience.Classify(msg)
		ec.State["failure_classification"] = string(class)
		appendFailureHistory(ec, "worker_benchmark", string(class), msg)
		if class == resilience.WorkerBusy && (getBool(ec.Inputs, "surface_worker_busy") || getBool(ec.State, "surface_worker_busy")) {
			return StepResult{
				Status:           StepWaitingExternal,
				WaitingCondition: "worker_busy",
				WaitingReason:    "Worker is busy; waiting for benchmark slot",
				Outputs: map[string]any{
					"failure_classification": string(class),
					"worker_busy":            true,
				},
			}, nil
		}
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("Benchmark failed (%s): %s", class, msg),
			Outputs: map[string]any{
				"benchmark_job_id":       benchJobID,
				"benchmark_status":       "failed",
				"failure_classification": string(class),
			},
		}, nil
	case transcode.StatusCancelled:
		return StepResult{
			Status: StepFailed,
			Error:  "benchmark job was cancelled",
			Outputs: map[string]any{
				"benchmark_job_id": benchJobID,
				"benchmark_status": "cancelled",
			},
		}, nil
	case transcode.StatusCompleted:
		if st.Decision == nil {
			return StepResult{Status: StepFailed, Error: "worker completed benchmark without decision report (fail closed)"}, nil
		}

		hasWinner := st.Decision.Winner != nil
		ec.State["benchmark_done"] = true
		ec.State["benchmark_decision"] = st.Decision
		ec.State["decision_reason"] = st.Decision.DecisionReason
		ec.State["has_winner"] = hasWinner

		outputs := map[string]any{
			"benchmark_done":     true,
			"benchmark_job_id":   benchJobID,
			"benchmark_status":   "completed",
			"benchmark_decision": st.Decision,
			"decision_reason":    st.Decision.DecisionReason,
			"has_winner":         hasWinner,
		}

		var winningPlan *transcode.Plan
		if hasWinner {
			winner := st.Decision.Winner
			ec.State["benchmark_winner"] = winner

			basePlan := getPlan(ec.State["plan"])
			if basePlan != nil {
				wp := *basePlan
				wp.Quality = winner.Quality
				wp.VideoProfile = winner.VideoProfile
				wp.PixelFormat = winner.PixelFormat
				wp.ExpectedBitDepth = winner.ExpectedBitDepth
				digest, err := transcode.DigestPlan(&wp)
				if err != nil {
					return StepResult{Status: StepFailed, Error: fmt.Sprintf("computing winning plan digest: %v", err)}, nil
				}
				wp.PlanDigest = digest
				winningPlan = &wp

				ec.State["plan"] = winningPlan
				ec.State["plan_digest"] = digest
				ec.State["recipe_version"] = winningPlan.RecipeVersion
				ec.State["recipe_digest"] = winningPlan.RecipeDigest

				outputs["plan"] = winningPlan
				outputs["plan_digest"] = digest
				outputs["recipe_version"] = winningPlan.RecipeVersion
				outputs["recipe_digest"] = winningPlan.RecipeDigest
			}

			outputs["winner"] = winner
			outputs["winner_candidate_id"] = winner.CandidateID
			outputs["winner_quality"] = winner.Quality
			outputs["winner_video_profile"] = winner.VideoProfile
			outputs["winner_pixel_format"] = winner.PixelFormat
			outputs["winner_bit_depth"] = winner.ExpectedBitDepth
			outputs["winner_score"] = winner.Score
			outputs["winner_metric"] = winner.MetricType
			outputs["estimated_total_mb"] = winner.EstimatedTotalMB
			outputs["estimated_savings_percent"] = winner.SavingsPercent
		}

		// If this is transcode_media, require a valid winner before proceeding to full transcode.
		if ec.ActionName == "transcode_media" && !hasWinner {
			reason := "no winner selected"
			if st.Decision.DecisionReason != "" {
				reason = st.Decision.DecisionReason
			}
			return StepResult{
				Status:  StepFailed,
				Error:   fmt.Sprintf("benchmark completed with no winning candidate (%s): manual review required (fail closed)", reason),
				Outputs: outputs,
			}, nil
		}

		return StepResult{
			Status:  StepCompleted,
			Outputs: outputs,
		}, nil
	default:
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "benchmark_complete",
			WaitingReason:    fmt.Sprintf("Benchmark in progress (%s)", st.Status),
		}, nil
	}
}

func (e *Engine) handleBenchmarkTransientFailure(ec *ExecutionContext, plan *transcode.Plan, err error, phase string) (StepResult, error) {
	class := resilience.Classify(err.Error())
	ec.State["failure_classification"] = string(class)
	appendFailureHistory(ec, phase, string(class), err.Error())

	retries := getInt(ec.State, "benchmark_retry_count")
	attempt := getInt(ec.State, "benchmark_attempt")
	if attempt <= 0 {
		attempt = 1
	}

	if class == resilience.WorkerBusy && (getBool(ec.Inputs, "surface_worker_busy") || getBool(ec.State, "surface_worker_busy")) {
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "worker_busy",
			WaitingReason:    "Worker is busy; waiting for benchmark slot",
			Outputs: map[string]any{
				"benchmark_attempt":      attempt,
				"benchmark_retry_count":  retries,
				"failure_classification": string(class),
				"worker_busy":            true,
			},
		}, nil
	}
	var res transcode.ResiliencePlan
	if plan != nil {
		res = plan.Resilience
	}
	if resilience.Retryable(class, res.RetryOn) && retries < res.TransientRetries && attempt < res.MaxAttempts {
		retries++
		attempt++
		ec.State["benchmark_retry_count"] = retries
		ec.State["benchmark_attempt"] = attempt
		backoff := 0
		if retries-1 < len(res.RetryBackoffSeconds) {
			backoff = res.RetryBackoffSeconds[retries-1]
		}
		if backoff > 0 {
			ec.State["benchmark_retry_not_before"] = time.Now().Add(time.Duration(backoff) * time.Second).UTC().Format(time.RFC3339Nano)
		}
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "benchmark_retry",
			WaitingReason:    fmt.Sprintf("%s classified as %s; bounded retry %d/%d after %ds", phase, class, retries, res.TransientRetries, backoff),
			Outputs: map[string]any{
				"benchmark_attempt":      attempt,
				"benchmark_retry_count":  retries,
				"failure_classification": string(class),
			},
		}, nil
	}
	return StepResult{
		Status: StepFailed,
		Error:  fmt.Sprintf("%s failed (%s): %v", phase, class, err),
		Outputs: map[string]any{
			"benchmark_attempt":      attempt,
			"benchmark_retry_count":  retries,
			"failure_classification": string(class),
		},
	}, nil
}

func getOrCreateBenchmarkJobID(ec *ExecutionContext) (string, error) {
	if id := getString(ec.State, "benchmark_job_id"); id != "" {
		return id, nil
	}
	id := fmt.Sprintf("bench-%s", ec.InstanceID)
	if len(id) > transcode.MaxBenchmarkIDLength {
		id = id[:transcode.MaxBenchmarkIDLength]
	}
	if err := transcode.ValidateBenchmarkJobID(id); err != nil {
		return "", fmt.Errorf("generated benchmark job id %q is invalid: %w", id, err)
	}
	return id, nil
}

func buildBenchmarkCandidates(bitDepth int, qVals []int, maxCandidates int) ([]transcode.BenchmarkCandidate, error) {
	if bitDepth != 8 && bitDepth != 10 {
		return nil, fmt.Errorf("unsupported source bit depth %d: only 8-bit and 10-bit SDR content supported (fail closed)", bitDepth)
	}
	targetProf := "main"
	targetPix := "yuv420p"
	if bitDepth == 10 {
		targetProf = "main10"
		targetPix = "p010le"
	}

	if maxCandidates > 0 && len(qVals) > maxCandidates {
		qVals = qVals[:maxCandidates]
	}

	candidates := make([]transcode.BenchmarkCandidate, 0, len(qVals))
	for _, q := range qVals {
		if q < 1 || q > 100 {
			return nil, fmt.Errorf("invalid quality %d: must be in 1..100", q)
		}
		cID := fmt.Sprintf("cand_q%d", q)
		candidates = append(candidates, transcode.BenchmarkCandidate{
			ID:           cID,
			Quality:      q,
			VideoProfile: targetProf,
			PixelFormat:  targetPix,
		})
	}
	return candidates, nil
}

func buildBenchmarkSamples(rep *mediainspect.DetailedReport, optSampling *recipe.SamplingPolicy) ([]transcode.BenchmarkSampleWindow, error) {
	if rep.DurationSec <= 0 {
		return nil, fmt.Errorf("invalid source duration %.3f: must be positive", rep.DurationSec)
	}
	sampleCfg := optimization.SamplePlanConfig{
		Duration:          rep.DurationSec,
		SampleSeconds:     optSampling.SampleSeconds,
		SampleCount:       optSampling.SampleCount,
		Positions:         optSampling.Positions,
		RelativePositions: true,
	}
	samplePlan, err := optimization.PlanSamples(sampleCfg)
	if err != nil {
		return nil, fmt.Errorf("planning benchmark samples: %w", err)
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

func resolveBenchmarkMetric(ec *ExecutionContext, optQuality *recipe.QualityPolicy) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(getString(ec.Inputs, "metric")))
	if raw != "" {
		if raw != "vmaf" && raw != "ssim" && raw != "both" && raw != "vmaf+ssim" {
			return "", fmt.Errorf("invalid metric %q: must be 'vmaf', 'ssim', or 'both' (fail closed)", raw)
		}
		if raw == "vmaf+ssim" {
			raw = "both"
		}
		return raw, nil
	}
	if optQuality != nil && optQuality.PreferredMetric != "" {
		norm := strings.ToLower(strings.TrimSpace(optQuality.PreferredMetric))
		if norm != "vmaf" && norm != "ssim" {
			return "", fmt.Errorf("invalid preferred_metric %q: must be 'vmaf' or 'ssim' (fail closed)", optQuality.PreferredMetric)
		}
		return norm, nil
	}
	return "vmaf", nil
}

func buildBenchmarkQualityConfig(optQuality *recipe.QualityPolicy) *transcode.BenchmarkQualityConfig {
	if optQuality == nil {
		return nil
	}
	qc := &transcode.BenchmarkQualityConfig{
		PreferredMetric: optQuality.PreferredMetric,
	}
	if optQuality.VMAF != nil {
		qc.VMAF = &transcode.BenchmarkQualityThresholds{
			Target:            optQuality.VMAF.Target,
			Minimum:           optQuality.VMAF.Minimum,
			MarginalTolerance: optQuality.VMAF.MarginalTolerance,
		}
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

func validateWorkerCapabilitiesForBenchmark(caps transcode.WorkerCapabilities, metric string, sourceBitDepth int) error {
	if caps.ProtocolVersion != transcode.WorkerProtocolVersion {
		return fmt.Errorf("worker protocol version %d does not match expected %d (fail closed)",
			caps.ProtocolVersion, transcode.WorkerProtocolVersion)
	}
	if !caps.Encoders["hevc_videotoolbox"] {
		return errors.New("required encoder 'hevc_videotoolbox' is not available on worker (fail closed)")
	}
	needVMAF := metric == "vmaf" || metric == "both"
	needSSIM := metric == "ssim" || metric == "both"
	if needVMAF && !caps.Filters["libvmaf"] {
		return errors.New("required filter 'libvmaf' is not available on worker (fail closed)")
	}
	if needSSIM && !caps.Filters["ssim"] {
		return errors.New("required filter 'ssim' is not available on worker (fail closed)")
	}
	if sourceBitDepth > 8 && needVMAF {
		return fmt.Errorf("worker capability unsupported: source media has bit depth %d (> 8-bit) but worker does not have verified 10-bit VMAF capability; silent 8-bit downconversion is prohibited (fail closed)", sourceBitDepth)
	}
	return nil
}

func buildBenchmarkRequest(ec *ExecutionContext, cleanPath string, rep *mediainspect.DetailedReport, opt *recipe.OptimizationPolicy, caps transcode.WorkerCapabilities) (*transcode.BenchmarkRequest, error) {
	if rep == nil || len(rep.Video) == 0 {
		return nil, errors.New("media inspection report has no video streams (fail closed)")
	}
	v0 := rep.Video[0]
	bitDepth := v0.BitDepth
	if bitDepth != 8 && bitDepth != 10 {
		return nil, fmt.Errorf("unsupported bit depth %d: only 8-bit and 10-bit SDR content supported (fail closed)", bitDepth)
	}

	benchJobID, err := getOrCreateBenchmarkJobID(ec)
	if err != nil {
		return nil, err
	}

	samples, err := buildBenchmarkSamples(rep, opt.Sampling)
	if err != nil {
		return nil, err
	}

	candidates, err := buildBenchmarkCandidates(bitDepth, opt.Search.QualityValues, opt.Search.MaxCandidates)
	if err != nil {
		return nil, err
	}

	metric, err := resolveBenchmarkMetric(ec, opt.Quality)
	if err != nil {
		return nil, err
	}

	if err := validateWorkerCapabilitiesForBenchmark(caps, metric, bitDepth); err != nil {
		return nil, err
	}

	qualityCfg := buildBenchmarkQualityConfig(opt.Quality)

	declaredVideoBitrate := int64(0)
	if v0.BitRate > 0 {
		declaredVideoBitrate = v0.BitRate
	} else if rep.BitRate > 0 {
		declaredVideoBitrate = rep.BitRate
	}

	var attachmentBytes int64
	for _, att := range rep.Attachments {
		if att.Tags != nil {
			if sBytes, ok := att.Tags["NUMBER_OF_BYTES"]; ok {
				if b, err := strconv.ParseInt(sBytes, 10, 64); err == nil && b > 0 {
					attachmentBytes += b
				}
			}
		}
	}

	req := &transcode.BenchmarkRequest{
		ProtocolVersion:           transcode.WorkerProtocolVersion,
		ID:                        benchJobID,
		SourcePath:                cleanPath,
		SourceDuration:            rep.DurationSec,
		Metric:                    metric,
		Samples:                   samples,
		Candidates:                candidates,
		Quality:                   qualityCfg,
		FallbackAudioBitrateBps:   384000,
		DeclaredVideoBitrateBps:   declaredVideoBitrate,
		AttachmentBytes:           attachmentBytes,
	}

	if err := transcode.ValidateBenchmarkRequest(req); err != nil {
		return nil, fmt.Errorf("validating benchmark request: %w", err)
	}

	return req, nil
}
