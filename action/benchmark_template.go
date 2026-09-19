package action

import (
	"context"
	"encoding/json"
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
		AutoReconcile:  true,
		Name:           "benchmark_transcode",
		Version:        1,
		Description:    "Coordinates safe candidate-only benchmark evaluation of encoder parameters over deterministic source samples, returning quality metrics, size estimates, and an explainable decision without modifying media or creating permanent candidates.",
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
	if getBool(ec.State, "benchmark_submitted") && !getBool(ec.State, "benchmark_reconcile") {
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

	var req *transcode.BenchmarkRequest
	var caps transcode.WorkerCapabilities
	if getBool(ec.State, "benchmark_reconcile") {
		id := getString(ec.State, "benchmark_job_id")
		e.recordWorkerPoll(ec)
		st, err := e.deps.Transcode.BenchmarkStatus(ctx, id)
		if err != nil {
			if isRetryableWorkerPollError(err) {
				return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: "Benchmark submit uncertain; reconciling the existing remote identity"}, nil
			}
			if he, ok := transcodeHTTPError(err); !ok || he.StatusCode != 404 {
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("benchmark reconciliation failed: %v", err)}, nil
			}
			if raw, ok := ec.State["benchmark_request"]; ok {
				b, _ := json.Marshal(raw)
				_ = json.Unmarshal(b, &req)
			}
		} else {
			switch st.Status {
			case transcode.StatusQueued, transcode.StatusRunning, transcode.StatusCompleted, transcode.StatusFailed, transcode.StatusCancelled:
				ec.State["benchmark_submitted"] = true
				ec.State["benchmark_job_id"] = id
				delete(ec.State, "benchmark_reconcile")
				if perr := e.persistExecutionState(ctx, ec); perr != nil {
					return StepResult{}, perr
				}
				return StepResult{Status: StepCompleted, Outputs: map[string]any{"benchmark_job_id": id, "benchmark_status": st.Status, "reconciled": true}}, nil
			default:
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("unexpected benchmark status %q", st.Status)}, nil
			}
		}
	}

	// Admission critical section: when this child belongs to a batch, hold the
	// parent lease across the policy re-read and the persisted submit outcome so
	// a confirmed batch cancel cannot interleave a new benchmark admission. A
	// benchmark that was already accepted/uncertain is reconciled above and is
	// never blocked from being tracked. Admission is evaluated before any worker
	// capability probe or request build.
	lease, blockedRes, handled := e.beginAdmission(ctx, ec, "benchmark")
	if handled {
		return blockedRes, nil
	}
	defer lease.Close()
	actx := lease.Context(ctx)

	if req == nil {
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
		var err error
		caps, err = e.deps.Transcode.Capabilities(actx)
		if err != nil {
			if isRetryableWorkerPollError(err) {
				return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Worker capabilities temporarily unavailable: %v", err)}, nil
			}
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("querying worker capabilities: %v (fail closed)", err)}, nil
		}
		req, err = buildBenchmarkRequest(ec, cleanPath, rep, opt, caps)
		if err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, nil
		}
	}
	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("computing benchmark request digest: %v", err)}, nil
	}

	ec.State["benchmark_job_id"] = req.ID
	ec.State["benchmark_request"] = req
	ec.State["benchmark_request_digest"] = digest
	ec.State["benchmark_reconcile"] = true
	if caps.CapabilityFingerprint != "" {
		ec.State["capability_fingerprint"] = caps.CapabilityFingerprint
	}
	if err := e.persistExecutionState(actx, ec); err != nil {
		return StepResult{}, err
	}
	// Ownership check after the identity checkpoint: a lost parent lease must
	// not create a new benchmark job; the persisted uncertainty stays for a
	// later safe reconciliation.
	if !e.parentLeaseHeld(lease) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "parent_busy", WaitingReason: fmt.Sprintf("Parent admission lease lost before benchmark submit for %s; deferring without resubmit", req.ID), Outputs: map[string]any{"benchmark_job_id": req.ID}}, nil
	}

	job, err := e.deps.Transcode.BenchmarkSubmit(actx, *req)
	if err != nil {
		if isRetryableWorkerPollError(err) {
			res := StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: "Benchmark submit uncertain; reconciling the existing remote identity", Outputs: map[string]any{"benchmark_job_id": req.ID}}
			if perr := e.persistExecutionState(actx, ec); perr != nil {
				return StepResult{}, perr
			}
			return res, nil
		}
		delete(ec.State, "benchmark_reconcile")
		plan := getPlan(ec.State["plan"])
		res, herr := e.handleBenchmarkTransientFailure(ec, plan, err, "benchmark_submit")
		if herr != nil {
			return StepResult{}, herr
		}
		// Persist the definitive outcome under the child owner before releasing
		// the parent admission lease.
		if perr := e.persistExecutionState(actx, ec); perr != nil {
			return StepResult{}, perr
		}
		return res, nil
	}

	delete(ec.State, "benchmark_reconcile")
	ec.State["benchmark_submitted"] = true
	ec.State["benchmark_job_id"] = job.ID
	ec.State["benchmark_request_digest"] = digest
	if caps.CapabilityFingerprint != "" {
		ec.State["capability_fingerprint"] = caps.CapabilityFingerprint
	}
	// Persist acceptance before releasing the parent admission lease.
	if perr := e.persistExecutionState(actx, ec); perr != nil {
		return StepResult{}, perr
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"benchmark_job_id":         job.ID,
			"benchmark_request_digest": digest,
			"capability_fingerprint":   ec.State["capability_fingerprint"],
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
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: "Coordinator request interrupted; benchmark continues on worker"}, nil
	}
	if ec.Decision == "cancel" {
		_ = e.deps.Transcode.BenchmarkCancel(ctx, benchJobID)
		return StepResult{Status: StepFailed, Error: "benchmark job was cancelled by decision"}, nil
	}

	e.recordWorkerPoll(ec)
	st, err := e.deps.Transcode.BenchmarkStatus(ctx, benchJobID)
	if err != nil {
		if isRetryableWorkerPollError(err) {
			ec.State["progress_is_stale"] = true
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: "Benchmark status temporarily unavailable; existing remote job will be reconciled"}, nil
		}
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("benchmark status failed; no resubmit: %v", err)}, nil
	}
	e.observeBenchmarkStatus(ec, st)

	switch st.Status {
	case transcode.StatusRunning, transcode.StatusQueued:
		reason := fmt.Sprintf("Benchmarking encoder configurations (%s, progress: %.1f%%)", st.Status, st.Progress)
		if st.Phase != "" {
			reason = fmt.Sprintf("Benchmarking encoder configurations (%s, phase: %s, progress: %.1f%%)", st.Status, st.Phase, st.Progress)
		}
		if st.ProgressIsStale {
			reason = fmt.Sprintf("Benchmarking encoder configurations (%s; phase: %s; progress temporarily stale; last known: %.1f%%)", st.Status, st.Phase, st.Progress)
		}
		outputs := map[string]any{
			"benchmark_job_id": benchJobID,
			"benchmark_status": st.Status,
			"progress":         st.Progress,
		}
		if st.Phase != "" {
			outputs["phase"] = st.Phase
		}
		if !st.HeartbeatAt.IsZero() {
			outputs["heartbeat"] = st.HeartbeatAt
			outputs["heartbeat_at"] = st.HeartbeatAt
		}
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "benchmark_complete",
			WaitingReason:    reason,
			Outputs:          outputs,
		}, nil
	case transcode.StatusFailed:
		msg := st.Error
		if msg == "" {
			msg = "benchmark executor reported job failure"
		}
		class := resilience.Classify(msg)
		ec.State["failure_classification"] = string(class)
		appendFailureHistory(ec, "worker_benchmark", string(class), msg)

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
				// The swept rate dimension is authoritative verbatim: quality
				// winners carry Quality with zero bitrate, bitrate winners
				// carry AverageBitrateKbps with zero quality.
				wp.Quality = winner.Quality
				wp.AverageBitrateKbps = winner.AverageBitrateKbps
				// All other typed knobs are carried when the winner states
				// them (coordinator-built candidates always echo the full
				// inherited set); a winner that omits them (foreign or
				// pre-upgrade in-flight job) retains the base plan intent
				// instead of silently clearing encoder configuration.
				if winner.MaxBitrateKbps != 0 {
					wp.MaxBitrateKbps = winner.MaxBitrateKbps
				}
				if winner.ConstantBitrate != nil {
					wp.ConstantBitrate = winner.ConstantBitrate
				}
				if winner.QMin != nil {
					wp.QMin = winner.QMin
				}
				if winner.QMax != nil {
					wp.QMax = winner.QMax
				}
				if winner.GOPSize != nil {
					wp.GOPSize = winner.GOPSize
				}
				if winner.BFrames != nil {
					wp.BFrames = winner.BFrames
				}
				if winner.ClosedGOP != nil {
					wp.ClosedGOP = winner.ClosedGOP
				}
				if winner.PowerEfficient != nil {
					wp.PowerEfficient = winner.PowerEfficient
				}
				if winner.MaxRefFrames != nil {
					wp.MaxRefFrames = winner.MaxRefFrames
				}
				if winner.PrioritizeSpeed != nil {
					wp.PrioritizeSpeed = winner.PrioritizeSpeed
				}
				if winner.SpatialAQ != nil {
					wp.SpatialAQ = winner.SpatialAQ
				}
				if winner.Realtime != nil {
					wp.Realtime = winner.Realtime
				}
				wp.VideoProfile = winner.VideoProfile
				wp.PixelFormat = winner.PixelFormat
				wp.ExpectedBitDepth = winner.ExpectedBitDepth
				// The winner is authoritative for the encoder and preset it was
				// benchmarked with; carry them into the full-encode plan so a
				// libx265 winner is not silently re-encoded with the base plan's
				// VideoToolbox encoder (and vice versa).
				if codec := strings.TrimSpace(winner.VideoCodec); codec != "" {
					wp.VideoCodec = codec
				}
				if preset := strings.TrimSpace(winner.Preset); preset != "" {
					wp.Preset = preset
				}
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
			outputs["winner_video_codec"] = winner.VideoCodec
			outputs["winner_preset"] = winner.Preset
			outputs["winner_quality"] = winner.Quality
			outputs["winner_average_bitrate_kbps"] = winner.AverageBitrateKbps
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

		// For transcode_media only, enforce the effective size guardrails
		// against the winner's predicted savings BEFORE any full encode is
		// launched. Standalone benchmark_transcode stays report-only and
		// must not block on a growing estimate.
		if ec.ActionName == "transcode_media" && hasWinner {
			minSavings, maxIncrease := e.effectiveSizeGuardrails(ec)
			outputs["effective_min_savings_percent"] = minSavings
			outputs["effective_max_size_increase_percent"] = maxIncrease
			outputs["predicted_savings_percent"] = st.Decision.Winner.SavingsPercent
			if err := checkBenchmarkSavingsGuardrail(st.Decision.Winner.SavingsPercent, minSavings, maxIncrease); err != nil {
				return StepResult{
					Status:  StepFailed,
					Error:   fmt.Sprintf("%v: full transcode not started; manual review required (fail closed)", err),
					Outputs: outputs,
				}, nil
			}
		}

		return StepResult{
			Status:  StepCompleted,
			Outputs: outputs,
		}, nil
	default:
		reason := fmt.Sprintf("Benchmark in progress (%s)", st.Status)
		if st.Phase != "" {
			reason = fmt.Sprintf("Benchmark in progress (%s, phase: %s)", st.Status, st.Phase)
		}
		outputs := map[string]any{
			"benchmark_job_id": benchJobID,
			"benchmark_status": st.Status,
			"progress":         st.Progress,
		}
		if st.Phase != "" {
			outputs["phase"] = st.Phase
		}
		if !st.HeartbeatAt.IsZero() {
			outputs["heartbeat"] = st.HeartbeatAt
			outputs["heartbeat_at"] = st.HeartbeatAt
		}
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "benchmark_complete",
			WaitingReason:    reason,
			Outputs:          outputs,
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

func buildBenchmarkCandidates(basePlan *transcode.Plan, bitDepth int, srch *recipe.SearchPolicy) ([]transcode.BenchmarkCandidate, error) {
	if basePlan == nil {
		return nil, fmt.Errorf("base transcode plan is required to derive benchmark encoder and video knobs (fail closed)")
	}
	if bitDepth != 8 && bitDepth != 10 {
		return nil, fmt.Errorf("unsupported source bit depth %d: only 8-bit and 10-bit SDR content supported (fail closed)", bitDepth)
	}
	if srch == nil {
		return nil, fmt.Errorf("search policy is required to build benchmark candidates (fail closed)")
	}
	targetProf := "main"
	targetPix := "yuv420p"
	if bitDepth == 10 {
		targetProf = "main10"
		targetPix = "p010le"
	}

	normCodec := transcode.NormalizeVideoCodec(basePlan.VideoCodec)
	if normCodec == "" {
		normCodec = transcode.VideoCodecHEVCVideoToolbox
	}
	normPreset := strings.ToLower(strings.TrimSpace(basePlan.Preset))
	maxCandidates := srch.MaxCandidates
	hasQuality := len(srch.QualityValues) > 0
	hasBitrate := len(srch.BitrateValues) > 0
	if hasQuality && hasBitrate {
		return nil, fmt.Errorf("search quality_values and bitrate_values are mutually exclusive: sweep exactly one rate-control dimension (fail closed)")
	}
	if !hasQuality && !hasBitrate {
		return nil, fmt.Errorf("search must specify quality_values or bitrate_values (fail closed)")
	}

	// All typed VideoToolbox knobs except the swept rate dimension are
	// inherited verbatim from the profile's resolved base plan, so benchmark
	// encodes exercise the exact configuration the full transcode would run.
	inheritVideoToolboxKnobs := func(c *transcode.BenchmarkCandidate) {
		c.VideoProfile = targetProf
		c.PixelFormat = targetPix
		c.MaxBitrateKbps = basePlan.MaxBitrateKbps
		c.ConstantBitrate = cloneBenchmarkBool(basePlan.ConstantBitrate)
		c.QMin = cloneBenchmarkInt(basePlan.QMin)
		c.QMax = cloneBenchmarkInt(basePlan.QMax)
		c.GOPSize = cloneBenchmarkInt(basePlan.GOPSize)
		c.BFrames = cloneBenchmarkInt(basePlan.BFrames)
		c.ClosedGOP = cloneBenchmarkBool(basePlan.ClosedGOP)
		c.PowerEfficient = cloneBenchmarkBool(basePlan.PowerEfficient)
		c.MaxRefFrames = cloneBenchmarkInt(basePlan.MaxRefFrames)
		c.PrioritizeSpeed = cloneBenchmarkBool(basePlan.PrioritizeSpeed)
		c.SpatialAQ = cloneBenchmarkBool(basePlan.SpatialAQ)
		c.Realtime = cloneBenchmarkBool(basePlan.Realtime)
	}

	switch normCodec {
	case transcode.VideoCodecHEVCVideoToolbox:
		if normPreset != "" {
			return nil, fmt.Errorf("preset %q is only supported for libx265 (fail closed)", basePlan.Preset)
		}
		if hasBitrate {
			brVals := srch.BitrateValues
			if maxCandidates > 0 && len(brVals) > maxCandidates {
				brVals = brVals[:maxCandidates]
			}
			candidates := make([]transcode.BenchmarkCandidate, 0, len(brVals))
			for _, br := range brVals {
				if br < 1 || br > transcode.MaxVideoBitrateKbps {
					return nil, fmt.Errorf("invalid bitrate value %d: must be in 1..%d kbps", br, transcode.MaxVideoBitrateKbps)
				}
				c := transcode.BenchmarkCandidate{
					ID:                 fmt.Sprintf("cand_br%dk", br),
					VideoCodec:         normCodec,
					AverageBitrateKbps: br,
				}
				inheritVideoToolboxKnobs(&c)
				if c.MaxBitrateKbps != 0 && c.MaxBitrateKbps < c.AverageBitrateKbps {
					return nil, fmt.Errorf("bitrate candidate %q (%d kbps) exceeds inherited max_bitrate_kbps (%d) (fail closed)", c.ID, c.AverageBitrateKbps, c.MaxBitrateKbps)
				}
				candidates = append(candidates, c)
			}
			return candidates, nil
		}
		qVals := srch.QualityValues
		if maxCandidates > 0 && len(qVals) > maxCandidates {
			qVals = qVals[:maxCandidates]
		}
		candidates := make([]transcode.BenchmarkCandidate, 0, len(qVals))
		for _, q := range qVals {
			if q < 1 || q > 100 {
				return nil, fmt.Errorf("invalid quality %d: must be in 1..100", q)
			}
			c := transcode.BenchmarkCandidate{
				ID:         fmt.Sprintf("cand_q%d", q),
				VideoCodec: normCodec,
				Quality:    q,
			}
			inheritVideoToolboxKnobs(&c)
			candidates = append(candidates, c)
		}
		return candidates, nil
	case transcode.VideoCodecLibX265:
		if hasBitrate {
			return nil, fmt.Errorf("search bitrate_values are only supported for hevc_videotoolbox, not libx265: libx265 is CRF-only (fail closed)")
		}
		if normPreset == "" {
			normPreset = "medium"
		}
		if !transcode.IsValidLibX265Preset(normPreset) {
			return nil, fmt.Errorf("unsupported libx265 preset %q", basePlan.Preset)
		}
		qVals := srch.QualityValues
		if maxCandidates > 0 && len(qVals) > maxCandidates {
			qVals = qVals[:maxCandidates]
		}
		candidates := make([]transcode.BenchmarkCandidate, 0, len(qVals))
		for _, q := range qVals {
			if q < transcode.LibX265CRFMin || q > transcode.LibX265CRFMax {
				return nil, fmt.Errorf("invalid libx265 crf %d: must be in %d..%d", q, transcode.LibX265CRFMin, transcode.LibX265CRFMax)
			}
			candidates = append(candidates, transcode.BenchmarkCandidate{
				ID:           fmt.Sprintf("cand_crf%d", q),
				VideoCodec:   normCodec,
				Quality:      q,
				Preset:       normPreset,
				VideoProfile: targetProf,
				PixelFormat:  targetPix,
			})
		}
		return candidates, nil
	default:
		return nil, fmt.Errorf("unsupported video codec %q for benchmark (fail closed)", basePlan.VideoCodec)
	}
}

func cloneBenchmarkBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneBenchmarkInt(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
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

func buildBenchmarkAdaptiveConfig(srch *recipe.SearchPolicy, codec string) *transcode.BenchmarkAdaptiveConfig {
	if srch == nil {
		return nil
	}
	// Adaptive ordering assumes ascending rate-control value => non-decreasing
	// quality, which does not hold for libx265 CRF (lower = higher quality).
	// libx265 always uses exhaustive candidate evaluation. The same applies
	// to VideoToolbox bitrate sweeps: the adaptive planner probes
	// quality-ordered candidates only.
	if transcode.NormalizeVideoCodec(codec) == transcode.VideoCodecLibX265 {
		return nil
	}
	if len(srch.BitrateValues) > 0 {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(srch.AdaptiveMode))
	if mode == "" || mode == optimization.AdaptiveModeExhaustive {
		return nil
	}
	if mode != optimization.AdaptiveModeAdaptive {
		return nil
	}
	initial := srch.AdaptiveInitialQuality
	if initial == 0 {
		initial = optimization.DefaultAdaptiveInitialQuality
	}
	return &transcode.BenchmarkAdaptiveConfig{
		Mode:           optimization.AdaptiveModeAdaptive,
		InitialQuality: initial,
	}
}

func buildBenchmarkConcurrencyConfig(srch *recipe.SearchPolicy) *transcode.BenchmarkConcurrencyConfig {
	if srch == nil {
		return nil
	}
	if srch.EncodeConcurrency == 0 && srch.MetricConcurrency == 0 {
		return nil
	}
	enc := srch.EncodeConcurrency
	if enc == 0 {
		enc = transcode.DefaultEncodeConcurrency
	}
	met := srch.MetricConcurrency
	if met == 0 {
		met = transcode.DefaultMetricConcurrency
	}
	return &transcode.BenchmarkConcurrencyConfig{
		EncodeConcurrency: enc,
		MetricConcurrency: met,
	}
}

func validateWorkerCapabilitiesForBenchmark(caps transcode.WorkerCapabilities, metric string, sourceBitDepth int, codec string) error {
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
	if normCodec == transcode.VideoCodecLibX265 && sourceBitDepth > 8 {
		return fmt.Errorf("worker capability unsupported: libx265 benchmark is 8-bit only, got source bit depth %d (fail closed)", sourceBitDepth)
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

	// The encoder and preset come from the resolved plan so profile=live-action-hevc
	// benchmarks produce libx265 candidates rather than VideoToolbox candidates.
	// A missing or unreadable plan must fail closed: falling back to an empty
	// codec would silently benchmark VideoToolbox candidates instead of the
	// encoder the profile actually selected.
	plan := getPlan(ec.State["plan"])
	if plan == nil {
		return nil, errors.New("resolved plan is missing or unreadable from state; refusing to default benchmark encoder to VideoToolbox (fail closed)")
	}
	planCodec := plan.VideoCodec

	candidates, err := buildBenchmarkCandidates(plan, bitDepth, opt.Search)
	if err != nil {
		return nil, err
	}

	metric, err := resolveBenchmarkMetric(ec, opt.Quality)
	if err != nil {
		return nil, err
	}

	if err := validateWorkerCapabilitiesForBenchmark(caps, metric, bitDepth, planCodec); err != nil {
		return nil, err
	}

	qualityCfg := buildBenchmarkQualityConfig(opt.Quality)
	adaptiveCfg := buildBenchmarkAdaptiveConfig(opt.Search, planCodec)
	concurrencyCfg := buildBenchmarkConcurrencyConfig(opt.Search)

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
		ProtocolVersion:         transcode.WorkerProtocolVersion,
		ID:                      benchJobID,
		SourcePath:              cleanPath,
		SourceDuration:          rep.DurationSec,
		Metric:                  metric,
		Samples:                 samples,
		Candidates:              candidates,
		Quality:                 qualityCfg,
		Adaptive:                adaptiveCfg,
		Concurrency:             concurrencyCfg,
		FallbackAudioBitrateBps: 384000,
		DeclaredVideoBitrateBps: declaredVideoBitrate,
		AttachmentBytes:         attachmentBytes,
	}

	if err := transcode.ValidateBenchmarkRequest(req); err != nil {
		return nil, fmt.Errorf("validating benchmark request: %w", err)
	}

	return req, nil
}
