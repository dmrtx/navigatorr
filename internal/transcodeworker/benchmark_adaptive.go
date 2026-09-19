package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// isAdaptiveEnabled reports whether the record requests adaptive evaluation.
// Nil/empty/exhaustive Adaptive preserves the existing exhaustive path unchanged.
// Adaptive requires more than two candidates to be worthwhile and is disabled when
// test hooks bypass real encode/metric execution, preserving hook semantics.
func isAdaptiveEnabled(record *BenchmarkRecord, r *ProductionBenchmarkRunner) bool {
	if record == nil || record.Adaptive == nil {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(record.Adaptive.Mode))
	if mode != optimization.AdaptiveModeAdaptive {
		return false
	}
	if r != nil && (r.metricsHook != nil || r.selectionHook != nil) {
		return false
	}
	// Adaptive ordering assumes ascending rate-control value => non-decreasing
	// quality. That holds for VideoToolbox -q:v but is inverted for libx265 CRF
	// (lower CRF = higher quality), so non-VideoToolbox codecs always use the
	// exhaustive path to preserve selection semantics. VideoToolbox bitrate
	// sweeps are likewise excluded: the planner probes quality-ordered
	// candidates only.
	for _, c := range record.Candidates {
		if transcode.BenchmarkCandidateVideoCodec(c) != "hevc_videotoolbox" {
			return false
		}
		if c.AverageBitrateKbps != 0 {
			return false
		}
	}
	if len(record.Candidates) <= 2 {
		return false
	}
	return true
}

// resolveAdaptivePlanningPolicy resolves the target, tolerance, and metric used for
// adaptive floor planning. It mirrors runSelection policy resolution for single-metric
// jobs; for metric "both" it plans on the preferred metric. Size estimates are never
// used for planning.
func resolveAdaptivePlanningPolicy(record *BenchmarkRecord) (target, tolerance float64, metric optimization.MetricType, err error) {
	normMetric := strings.ToLower(strings.TrimSpace(record.Metric))
	if normMetric == "" {
		normMetric = "vmaf"
	}
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
	var policy optimization.QualityPolicy
	switch normMetric {
	case "vmaf":
		policy = vmafPolicy
	case "ssim":
		policy = ssimPolicy
	case "both", "vmaf+ssim":
		preferred := "vmaf"
		if record.Quality != nil && strings.ToLower(strings.TrimSpace(record.Quality.PreferredMetric)) != "" {
			preferred = strings.ToLower(strings.TrimSpace(record.Quality.PreferredMetric))
		}
		if preferred == "ssim" {
			policy = ssimPolicy
		} else {
			policy = vmafPolicy
		}
	default:
		return 0, 0, "", fmt.Errorf("unsupported benchmark metric %q (fail closed)", record.Metric)
	}
	if err := optimization.ValidatePolicy(policy); err != nil {
		return 0, 0, "", fmt.Errorf("validating adaptive planning policy: %w (fail closed)", err)
	}
	return policy.TargetScore(), policy.Tolerance(), policy.Metric(), nil
}

// adaptiveObservationForCandidate builds the planner observation for one evaluated
// candidate using the planning policy. Valid is false when the probe is
// failed/inconclusive (missing/invalid aggregate or encode failure), which forces
// exhaustive fallback. Eligibility preserves average-minimum and per-sample-minimum
// semantics via Policy.Evaluate.
func adaptiveObservationForCandidate(
	sortedPos int,
	vc validatedCandidate,
	policy optimization.QualityPolicy,
	evidence *BenchmarkExecutionEvidence,
	colorInfo optimization.ColorInfo,
) optimization.AdaptiveProbeObservation {
	obs := optimization.AdaptiveProbeObservation{
		SortedPos: sortedPos,
		Quality:   vc.candidate.Quality,
	}
	// Encode success: every planned sample must have a non-empty, error-free entry.
	encodeSuccess := true
	sampleCount := 0
	for _, cs := range evidence.CandidateSamples {
		if cs.CandidateID == vc.candidate.ID {
			if cs.Error != "" || cs.SizeBytes <= 0 {
				encodeSuccess = false
			} else {
				sampleCount++
			}
		}
	}
	// Find the planning-metric aggregate for this candidate.
	var agg optimization.MetricAggregate
	found := false
	for _, cm := range evidence.CandidateMetrics {
		if cm.CandidateID == vc.candidate.ID && cm.MetricType == policy.Metric() {
			agg = cm.Aggregate
			found = true
			break
		}
	}
	if !found {
		agg = optimization.MetricAggregate{
			MetricType:       policy.Metric(),
			Valid:            false,
			IneligibleReason: optimization.ReasonMetricMissing,
		}
	}
	eval := policy.Evaluate(agg, colorInfo)
	obs.Evaluation = eval
	// Conclusive only when encode succeeded and the aggregate is valid.
	obs.Valid = encodeSuccess && agg.Valid
	_ = sampleCount
	return obs
}

// runAdaptiveCandidates executes encode+metric probes in planner order, falling back
// to exhaustive remainder whenever the planner requires it. Progress uses the
// full-plan (worst-case) denominator so it stays monotonic; skipped candidates'
// planned units are explicitly resolved after early stop so progress reaches the
// same terminal value as exhaustive without ever decreasing or exceeding 100.
func (r *ProductionBenchmarkRunner) runAdaptiveCandidates(
	ctx context.Context,
	w *Worker,
	record *BenchmarkRecord,
	evidence *BenchmarkExecutionEvidence,
	samplesDir string,
	validatedCandidates []validatedCandidate,
	rep mediainspect.DetailedReport,
	sourceInitialSize int64,
	sourceInitialModTime time.Time,
	progressReporter *benchmarkProgressReporter,
	passesPerSample int,
) error {
	n := len(validatedCandidates)
	if n == 0 {
		return errors.New("no validated candidates for adaptive evaluation (fail closed)")
	}
	target, tolerance, planningMetric, err := resolveAdaptivePlanningPolicy(record)
	if err != nil {
		return err
	}
	// Build deterministic ascending-quality order. validatedCandidates arrive in
	// record order (ascending per request validation); sort defensively by quality
	// with original index tie-break for full determinism.
	type ordered struct {
		vc  validatedCandidate
		pos int // index into validatedCandidates
	}
	order := make([]ordered, 0, n)
	for i, vc := range validatedCandidates {
		order = append(order, ordered{vc: vc, pos: i})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].vc.candidate.Quality != order[j].vc.candidate.Quality {
			return order[i].vc.candidate.Quality < order[j].vc.candidate.Quality
		}
		return order[i].vc.index < order[j].vc.index
	})
	qualities := make([]int, 0, n)
	for _, o := range order {
		qualities = append(qualities, o.vc.candidate.Quality)
	}
	initialQuality := optimization.DefaultAdaptiveInitialQuality
	if record.Adaptive != nil && record.Adaptive.InitialQuality != 0 {
		initialQuality = record.Adaptive.InitialQuality
	}
	planner := optimization.NewAdaptivePlanner(qualities, initialQuality, target, tolerance)

	// Color info identical to runSelection so policy evaluation matches selection.
	colorInfo := optimization.ColorInfo{BitDepth: evidence.SourceBitDepth}
	if len(rep.Video) > 0 {
		colorInfo.ColorPrimaries = rep.Video[0].ColorPrimaries
		colorInfo.ColorTransfer = rep.Video[0].ColorTransfer
		colorInfo.ColorSpace = rep.Video[0].ColorSpace
		colorInfo.PixelFormat = rep.Video[0].PixelFormat
		if colorInfo.BitDepth == 0 {
			colorInfo.BitDepth = rep.Video[0].BitDepth
		}
	}
	var planningPolicy optimization.QualityPolicy
	switch planningMetric {
	case optimization.MetricTypeSSIM:
		planningPolicy = resolveSSIMPolicyForRecord(record)
	default:
		planningPolicy = resolveVMAFPolicyForRecord(record)
	}

	// Map sorted position -> validatedCandidates slice position.
	evaluatedCount := 0
	for {
		sortedPos, ok := planner.NextProbe()
		if !ok {
			break
		}
		if sortedPos < 0 || sortedPos >= n {
			return fmt.Errorf("adaptive planner returned out-of-range position %d (fail closed)", sortedPos)
		}
		vcPos := order[sortedPos].pos
		vc := validatedCandidates[vcPos]
		// One probe candidate at a time: samples pipeline within the probe while
		// planner decisions stay strictly sequential, so adaptive search still
		// avoids unnecessary candidates.
		if err := r.runPipelinedEncodeMetrics(ctx, w, record, evidence, samplesDir, []validatedCandidate{vc}, sourceInitialSize, sourceInitialModTime, progressReporter); err != nil {
			return err
		}
		evaluatedCount++
		obs := adaptiveObservationForCandidate(sortedPos, vc, planningPolicy, evidence, colorInfo)
		planner.Observe(obs)
	}

	// Explicitly resolve skipped units so progress with early stop matches the
	// full-plan denominator monotonically (never decreases, never exceeds 100).
	skipped := n - evaluatedCount
	if skipped > 0 {
		skippedUnits := skipped * len(record.Samples) * (1 + passesPerSample)
		progressReporter.SkipUnits(skippedUnits)
	}
	return nil
}

func resolveVMAFPolicyForRecord(record *BenchmarkRecord) optimization.QualityPolicy {
	p := optimization.DefaultVMAFPolicy()
	if record != nil && record.Quality != nil && record.Quality.VMAF != nil {
		tol := p.Tolerance()
		if record.Quality.VMAF.MarginalTolerance != nil {
			tol = *record.Quality.VMAF.MarginalTolerance
		}
		p = optimization.NewVMAFPolicy(record.Quality.VMAF.Target, record.Quality.VMAF.Minimum, tol)
	}
	return p
}

func resolveSSIMPolicyForRecord(record *BenchmarkRecord) optimization.QualityPolicy {
	p := optimization.DefaultSSIMPolicy()
	if record != nil && record.Quality != nil && record.Quality.SSIM != nil {
		tol := p.Tolerance()
		if record.Quality.SSIM.MarginalTolerance != nil {
			tol = *record.Quality.SSIM.MarginalTolerance
		}
		p = optimization.NewSSIMPolicy(record.Quality.SSIM.Target, record.Quality.SSIM.Minimum, tol)
	}
	return p
}
