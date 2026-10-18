package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

func automaticFallback(decision *transcode.BenchmarkDecision) string {
	if decision != nil {
		for _, candidate := range decision.Evaluations {
			if candidate.MinimumMet || candidate.EvaluationReason == "insufficient_savings" {
				return "fallback-savings-"
			}
		}
	}
	return "fallback-quality-"
}

// One admission, one deadline, fixed thresholds. At most one fallback family
// and two holdout checks run; rejected samples never start a full-file encode.
func (r *ProductionBenchmarkRunner) runAutomaticCandidates(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, dir string, vcs []validatedCandidate, rep mediainspect.DetailedReport, sourceSize int64, sourceMod time.Time, progress *benchmarkProgressReporter) error {
	if len(record.Samples) != 5 {
		return fmt.Errorf("automatic search has no independent verification windows")
	}
	automatic := &transcode.BenchmarkAutomaticEvidence{}
	evidence.Automatic = automatic
	discovery := *record
	discovery.Samples = record.Samples[:3]
	var tested []validatedCandidate
	for _, vc := range vcs {
		if !strings.HasPrefix(vc.candidate.ID, "fallback-") {
			tested = append(tested, vc)
		}
	}
	if len(tested) == 0 {
		return fmt.Errorf("automatic search has no initial candidates")
	}
	run := func(stage string, stageRecord *BenchmarkRecord, candidates []validatedCandidate) error {
		ids := []string{}
		for _, vc := range candidates {
			ids = append(ids, vc.candidate.ID)
		}
		automatic.Stages = append(automatic.Stages, transcode.BenchmarkAutomaticStage{Stage: stage, Candidates: ids, Outcome: "running"})
		progress.SetPhase(stage)
		var err error
		if r.automaticHook != nil {
			err = r.automaticHook(ctx, stageRecord, evidence, candidates)
		} else {
			err = r.runPipelinedEncodeMetrics(ctx, w, stageRecord, evidence, dir, candidates, sourceSize, sourceMod, progress, nil)
		}
		if err != nil {
			return err
		}
		automatic.Stages[len(automatic.Stages)-1].Outcome = "completed"
		return nil
	}
	var skipped []transcode.BenchmarkCandidateEvaluation
	// measure runs one stage. With quality pruning, each rate-control family's
	// highest-quality setting runs first; when it fails the quality policy the
	// family's remaining settings, which only spend fewer bits, are not measured.
	measure := func(stage string, stageRecord *BenchmarkRecord, candidates []validatedCandidate) ([]validatedCandidate, error) {
		leaders, followers := splitQualityFamilies(candidates)
		if !record.QualityPruning || len(followers) == 0 {
			return candidates, run(stage, stageRecord, candidates)
		}
		if err := run(stage, stageRecord, leaders); err != nil {
			return nil, err
		}
		if err := r.runSelection(ctx, stageRecord, evidence, leaders, rep, sourceSize); err != nil {
			return nil, err
		}
		failed := map[string]bool{}
		for _, e := range evidence.Decision.Evaluations {
			if qualityRejection(e.EvaluationReason) {
				failed[e.CandidateID] = true
			}
		}
		measured := leaders
		var rest []validatedCandidate
		var skippedIDs []string
		for _, f := range followers {
			if !failed[f.leader] {
				rest = append(rest, f.vc)
				continue
			}
			skippedIDs = append(skippedIDs, f.vc.candidate.ID)
			skipped = append(skipped, transcode.BenchmarkCandidateEvaluation{
				CandidateID: f.vc.candidate.ID, CandidateIndex: f.vc.index, VideoCodec: transcode.BenchmarkCandidateVideoCodec(f.vc.candidate),
				Quality: f.vc.candidate.Quality, Preset: f.vc.candidate.Preset, Tune: f.vc.candidate.Tune, VideoProfile: f.vc.profile, PixelFormat: f.vc.pixelFormat,
				ExpectedBitDepth: f.vc.bitDepth, EvaluationReason: "skipped_quality_dominated", Uncertainties: []string{"not measured: " + f.leader + " failed quality"},
			})
		}
		if len(rest) > 0 {
			if err := run(stage, stageRecord, rest); err != nil {
				return nil, err
			}
			measured = append(append([]validatedCandidate{}, leaders...), rest...)
		}
		automatic.Stages[len(automatic.Stages)-1].Skipped = skippedIDs
		return measured, nil
	}
	finish := func() {
		evidence.Decision.Evaluations = append(evidence.Decision.Evaluations, skipped...)
	}
	tested, err := measure("testing_initial", &discovery, tested)
	if err != nil {
		return err
	}
	if err := r.runSelection(ctx, &discovery, evidence, tested, rep, sourceSize); err != nil {
		return err
	}
	automatic.Discovery = evidence.Decision
	fallbackUsed := false
	runFallback := func() error {
		if fallbackUsed {
			return nil
		}
		fallbackUsed = true
		prefix := automaticFallback(evidence.Decision)
		var alternatives []validatedCandidate
		for _, vc := range vcs {
			if strings.HasPrefix(vc.candidate.ID, prefix) {
				alternatives = append(alternatives, vc)
			}
		}
		if len(alternatives) > 0 {
			measured, err := measure("testing_alternatives", &discovery, alternatives)
			if err != nil {
				return err
			}
			tested = append(tested, measured...)
			if err := r.runSelection(ctx, &discovery, evidence, tested, rep, sourceSize); err != nil {
				return err
			}
			automatic.Discovery = evidence.Decision
		}
		return nil
	}
	if evidence.Decision.Winner == nil {
		if err := runFallback(); err != nil {
			return err
		}
	}
	// A holdout failure can select the next measured contender, but never expands
	// the number of attempts or silently reduces the quality/size policy.
	blocked := map[string]bool{}
	verificationFailures := []transcode.BenchmarkCandidateEvaluation{}
	for attempt := 0; attempt < 2 && evidence.Decision.Winner != nil; attempt++ {
		id := evidence.Decision.Winner.CandidateID
		var winner validatedCandidate
		for _, vc := range tested {
			if vc.candidate.ID == id {
				winner = vc
				break
			}
		}
		holdout := *record
		holdout.Samples = record.Samples[3:]
		if err := run(phaseVerifyingSamples, &holdout, []validatedCandidate{winner}); err != nil {
			return err
		}
		if err := mergeAutomaticQuality(record, evidence, winner); err != nil {
			return err
		}
		if err := r.runSelection(ctx, record, evidence, []validatedCandidate{winner}, rep, sourceSize); err != nil {
			return err
		}
		if evidence.Decision.Winner != nil {
			automatic.VerifiedSamples = 5
			evidence.Decision.Evaluations = append(evidence.Decision.Evaluations, verificationFailures...)
			appendAutomaticRejections(evidence.Decision, automatic.Discovery)
			finish()
			return nil
		}
		verificationFailures = append(verificationFailures, evidence.Decision.Evaluations...)
		blocked[id] = true
		var remaining []validatedCandidate
		for _, vc := range tested {
			if !blocked[vc.candidate.ID] {
				remaining = append(remaining, vc)
			}
		}
		if err := r.runSelection(ctx, &discovery, evidence, remaining, rep, sourceSize); err != nil {
			return err
		}
		tested = remaining
		if !fallbackUsed && attempt == 0 {
			if err := runFallback(); err != nil {
				return err
			}
		}
	}
	// Discovery winners are not accepted without holdouts, even after the
	// confirmation allowance or search deadline has been exhausted.
	evidence.Decision = &transcode.BenchmarkDecision{DecisionReason: "no_verified_candidate", Evaluations: verificationFailures}
	appendAutomaticRejections(evidence.Decision, automatic.Discovery)
	finish()
	return nil
}

// qualityRejection reports measured quality-policy failures. Incomplete
// evidence, encode errors and size rejections are not quality verdicts.
func qualityRejection(reason string) bool {
	switch reason {
	case optimization.ReasonBelowMinimumQuality, optimization.ReasonSampleBelowMinimum,
		"quality_vmaf_p5_below_minimum", "quality_vmaf_worst_window_below_minimum", "quality_vmaf_frames_below_limit_exceeded",
		"quality_cambi_mean_above_maximum", "quality_cambi_peak_above_maximum":
		return true
	}
	return false
}

type qualityFollower struct {
	vc     validatedCandidate
	leader string
}

// qualityFamily is a set of constant-quality candidates that differ only in
// their quality value. Bitrate sweeps and other candidates form singletons.
type qualityFamily struct {
	members       []validatedCandidate
	lowerIsBetter bool // libx265 CRF; VideoToolbox -q:v is higher-is-better
}

// pick returns the index of the family's highest-quality member (best) or
// its most-compressed member (!best). Ties keep the first member.
func (f qualityFamily) pick(best bool) int {
	out := 0
	for i, vc := range f.members {
		q, oq := vc.candidate.Quality, f.members[out].candidate.Quality
		better := (f.lowerIsBetter && q < oq) || (!f.lowerIsBetter && q > oq)
		worse := (f.lowerIsBetter && q > oq) || (!f.lowerIsBetter && q < oq)
		if (best && better) || (!best && worse) {
			out = i
		}
	}
	return out
}

// groupQualityFamilies groups constant-quality libx265 and VideoToolbox
// candidates whose settings match except for ID and quality, in first-seen
// order. The resolved output depth is part of the key: a Main candidate
// failing banding says nothing about a Main10 one, so it never prunes it.
func groupQualityFamilies(vcs []validatedCandidate) []qualityFamily {
	var order []string
	families := map[string]*qualityFamily{}
	for _, vc := range vcs {
		c := vc.candidate
		codec := transcode.BenchmarkCandidateVideoCodec(c)
		if c.Quality <= 0 || c.AverageBitrateKbps != 0 || (codec != "libx265" && codec != "hevc_videotoolbox") {
			order = append(order, "\x00"+c.ID)
			families["\x00"+c.ID] = &qualityFamily{members: []validatedCandidate{vc}}
			continue
		}
		key := c
		key.ID, key.Quality = "", 0
		b, _ := json.Marshal(key)
		k := fmt.Sprintf("%s|%d|%s|%s|%s", codec, vc.bitDepth, vc.profile, vc.pixelFormat, b)
		if families[k] == nil {
			order = append(order, k)
			families[k] = &qualityFamily{lowerIsBetter: codec == "libx265"}
		}
		families[k].members = append(families[k].members, vc)
	}
	out := make([]qualityFamily, 0, len(order))
	for _, k := range order {
		out = append(out, *families[k])
	}
	return out
}

// splitQualityFamilies groups constant-quality candidates that differ only in
// their quality value. Each family's best setting (lowest x265 CRF, highest
// VideoToolbox -q:v) leads; bitrate sweeps and singletons are always leaders.
func splitQualityFamilies(vcs []validatedCandidate) (leaders []validatedCandidate, followers []qualityFollower) {
	for _, f := range groupQualityFamilies(vcs) {
		best := f.pick(true)
		leaders = append(leaders, f.members[best])
		for i, vc := range f.members {
			if i != best {
				followers = append(followers, qualityFollower{vc: vc, leader: f.members[best].candidate.ID})
			}
		}
	}
	return leaders, followers
}

func appendAutomaticRejections(final, discovery *transcode.BenchmarkDecision) {
	if discovery == nil {
		return
	}
	present := map[string]bool{}
	for _, c := range final.Evaluations {
		present[c.CandidateID] = true
	}
	for _, c := range discovery.Evaluations {
		if present[c.CandidateID] {
			continue
		}
		if c.Eligible {
			c.Eligible = false
			c.EvaluationReason = "verification_not_passed"
		}
		final.Evaluations = append(final.Evaluations, c)
	}
}

// Recombine the original per-window measurements; no encode or metric is
// repeated. Native frame scores remain in memory throughout this invocation.
func mergeAutomaticQuality(record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, vc validatedCandidate) error {
	model, err := quality.Model(record.Quality.VMAF.Model)
	if err != nil {
		return err
	}
	sh := &pipelineShared{record: record, evidence: evidence, vcs: []validatedCandidate{vc}, runVMAF: true, runCAMBI: record.Quality.Banding != nil && record.Quality.Banding.Enabled, vmafModel: &model, metricOut: make([]pipelineMetricOutcome, len(record.Samples))}
	scores := []optimization.SampleScore{}
	for i, window := range record.Samples {
		found := false
		for _, ms := range evidence.MetricSamples {
			if ms.CandidateID != vc.candidate.ID || ms.SampleIndex != window.Index {
				continue
			}
			if found {
				return fmt.Errorf("duplicate automatic sample evidence")
			}
			found = true
			sh.metricOut[i] = pipelineMetricOutcome{executed: true, vmafStats: ms.VMAFStats, cambi: ms.CAMBI}
			if ms.VMAF == nil || ms.Error != "" {
				return fmt.Errorf("incomplete automatic metric evidence")
			}
			scores = append(scores, optimization.SampleScore{SampleIndex: window.Index, Score: *ms.VMAF, Valid: true})
		}
		if !found {
			return fmt.Errorf("missing automatic verification sample")
		}
	}
	for _, qe := range evidence.Quality {
		if qe.CandidateID == vc.candidate.ID {
			sh.capFingerprint = qe.CapabilityFingerprint
			break
		}
	}
	qe, reject := sh.evaluateCandidateQuality(0, vc.candidate.ID)
	if qe == nil {
		return fmt.Errorf("missing automatic quality evidence")
	}
	qualityRows := evidence.Quality[:0]
	for _, old := range evidence.Quality {
		if old.CandidateID != vc.candidate.ID {
			qualityRows = append(qualityRows, old)
		}
	}
	evidence.Quality = append(qualityRows, *qe)
	metrics := evidence.CandidateMetrics[:0]
	for _, old := range evidence.CandidateMetrics {
		if old.CandidateID != vc.candidate.ID {
			metrics = append(metrics, old)
		}
	}
	aggregate := optimization.AggregateSampleScores(optimization.MetricTypeVMAF, scores)
	if reject != "" {
		aggregate.Valid = false
		aggregate.IneligibleReason = reject
	}
	evidence.CandidateMetrics = append(metrics, BenchmarkCandidateMetricAggregate{CandidateID: vc.candidate.ID, CandidateIndex: vc.index, MetricType: optimization.MetricTypeVMAF, Aggregate: aggregate})
	return nil
}
