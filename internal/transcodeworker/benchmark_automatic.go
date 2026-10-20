package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

const (
	fallbackHardwarePrefix = transcode.HardwareFallbackCandidatePrefix
	fallbackQualityPrefix  = "fallback-quality-"
	fallbackSavingsPrefix  = "fallback-savings-"
	// reasonNotVerified marks a candidate that passed discovery but was never
	// checked on the holdout windows (the attempts went elsewhere, or a
	// sibling's quality failure ruled it out). It is not a failed check.
	reasonNotVerified = "not_verified"
	// reasonSkippedQualityDominated marks a setting not measured because the
	// highest-quality setting of its family failed quality.
	reasonSkippedQualityDominated = "skipped_quality_dominated"
	// reasonSelectionMarginNotMet marks a VMAF result that passes its p5 and
	// worst-window minimums by less than the size search's selection margin.
	reasonSelectionMarginNotMet = "quality_vmaf_selection_margin_not_met"
)

// automaticFallback picks the one fallback family a search may try: savings
// when a setting met quality but not the size minimum (a more efficient
// preset can help), quality otherwise. Under automatic search rules a holdout
// that failed on quality always picks quality: the discovery windows
// overstated quality, so more compression cannot help.
func automaticFallback(decision *transcode.BenchmarkDecision, holdoutQualityFailed bool) string {
	if holdoutQualityFailed {
		return fallbackQualityPrefix
	}
	if decision != nil {
		for _, candidate := range decision.Evaluations {
			if candidate.MinimumMet || candidate.EvaluationReason == "insufficient_savings" {
				return fallbackSavingsPrefix
			}
		}
	}
	return fallbackQualityPrefix
}

// automaticFallbackStages splits the chosen fallback family into the stages
// measured in order. Without rules, or for the savings family, the family is
// one stage. Under rules the quality family follows the frozen preset order
// (medium before slow): a later stage runs only when the earlier ones found
// no winner.
//
// hardware are the higher-quality VideoToolbox candidates of a ladder that
// failed quality (see hardwareQualityFallback). They join the first stage of
// the quality family, beside the medium x265 rungs, so a fast hardware result
// can win within the size margin before the slow x265 stage is paid for.
func automaticFallbackStages(vcs []validatedCandidate, prefix string, rules *transcode.AutomaticSearchRules, hardware []validatedCandidate) [][]validatedCandidate {
	stages := automaticX265FallbackStages(vcs, prefix, rules)
	if prefix != fallbackQualityPrefix || len(hardware) == 0 {
		return stages
	}
	if len(stages) == 0 {
		return [][]validatedCandidate{hardware}
	}
	stages[0] = append(append([]validatedCandidate(nil), stages[0]...), hardware...)
	return stages
}

func automaticX265FallbackStages(vcs []validatedCandidate, prefix string, rules *transcode.AutomaticSearchRules) [][]validatedCandidate {
	var family []validatedCandidate
	for _, vc := range vcs {
		if strings.HasPrefix(vc.candidate.ID, prefix) {
			family = append(family, vc)
		}
	}
	if len(family) == 0 {
		return nil
	}
	if rules == nil || prefix != fallbackQualityPrefix || len(rules.QualityFallbackPresets) == 0 {
		return [][]validatedCandidate{family}
	}
	var stages [][]validatedCandidate
	used := map[string]bool{}
	for _, preset := range rules.QualityFallbackPresets {
		var stage []validatedCandidate
		for _, vc := range family {
			if norm(vc.candidate.Preset) == preset {
				stage = append(stage, vc)
				used[vc.candidate.ID] = true
			}
		}
		if len(stage) > 0 {
			stages = append(stages, stage)
		}
	}
	var rest []validatedCandidate
	for _, vc := range family {
		if !used[vc.candidate.ID] {
			rest = append(rest, vc)
		}
	}
	if len(rest) > 0 {
		stages = append(stages, rest)
	}
	return stages
}

// hardwareQualityFallback returns the higher-quality VideoToolbox candidates
// when the hardware ladder has nothing left to offer on quality: every initial
// VideoToolbox rung either failed the quality policy at discovery (measured, or
// skipped as dominated by the rung that failed) or is ruled out by a failed
// holdout. The ladder's best rung is then too low for this content, and the
// rungs above it are the step toward quality. A ladder with a rung that failed
// for another reason (too large, an encode error, a non-quality holdout
// failure) or that was never measured gets none: more quality would not help,
// or the failure is not a quality verdict.
func hardwareQualityFallback(vcs, initial []validatedCandidate, discoveryFailed map[string]bool, qualityFailures []validatedCandidate) []validatedCandidate {
	ladder := 0
	for _, vc := range initial {
		if !isVideoToolbox(vc.candidate.VideoCodec) {
			continue
		}
		ladder++
		if !discoveryFailed[vc.candidate.ID] && !ruledOutByQualityFailure(vc, qualityFailures) {
			return nil
		}
	}
	if ladder == 0 {
		return nil
	}
	var fallback []validatedCandidate
	for _, vc := range vcs {
		if strings.HasPrefix(vc.candidate.ID, fallbackHardwarePrefix) {
			fallback = append(fallback, vc)
		}
	}
	return fallback
}

// ruledOutByQualityFailure reports a candidate a failed holdout rules out
// under automatic search rules: the failed setting itself, every setting of
// its family that spends no more bits (higher x265 CRF, lower VideoToolbox
// quality), and any other x265 CRF setting at or above the failed CRF. What
// remains is a step toward quality.
func ruledOutByQualityFailure(vc validatedCandidate, failed []validatedCandidate) bool {
	key, lowerIsBetter, ok := qualityFamilyKey(vc)
	for _, f := range failed {
		if vc.candidate.ID == f.candidate.ID {
			return true
		}
		fkey, flower, fok := qualityFamilyKey(f)
		if !ok || !fok {
			continue
		}
		q, fq := vc.candidate.Quality, f.candidate.Quality
		if key == fkey {
			if lowerIsBetter && q >= fq || !lowerIsBetter && q <= fq {
				return true
			}
			continue
		}
		if lowerIsBetter && flower && q >= fq {
			return true
		}
	}
	return false
}

// selectionMarginNotMet reports a candidate whose VMAF p5 or worst-window mean
// clears its minimum by less than the frozen selection margin (a hardware
// candidate's margin is larger by the frozen extra). It reads the
// candidate's latest quality evidence, so it applies to discovery and to the
// merged discovery-plus-holdout result alike.
func selectionMarginNotMet(record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, candidateID, videoCodec string) bool {
	rules := record.AutomaticSearch
	if rules == nil || rules.SelectionMarginVMAF <= 0 || record.Quality == nil || record.Quality.VMAF == nil || record.Quality.VMAF.GuardrailEnforcement != "reject" {
		return false
	}
	v := record.Quality.VMAF
	margin := rules.SelectionMarginVMAF
	if record.HardwareSearch != nil && isVideoToolbox(videoCodec) {
		margin += record.HardwareSearch.ExtraSelectionMarginVMAF
	}
	for _, q := range evidence.Quality {
		if q.CandidateID != candidateID || q.VMAF == nil {
			continue
		}
		if v.P5Minimum != nil && q.VMAF.P5 < *v.P5Minimum+margin {
			return true
		}
		return v.WorstWindowMinimum != nil && q.VMAF.WorstWindowMean < *v.WorstWindowMinimum+margin
	}
	return false
}

// One admission, one deadline, fixed thresholds. At most one fallback family
// and two holdout checks run; rejected samples never start a full-file encode.
//
// Under automatic search rules (record.AutomaticSearch) a holdout that fails
// on quality rules out that setting and every more-compressed one, the next
// verification goes to the safest remaining eligible setting of the same
// family (not the next-smallest, which tends to fail for the same reason), and
// the quality fallback family is measured only when no such setting remains,
// preset by preset. A holdout failing on anything else keeps the earlier behaviour.
func (r *ProductionBenchmarkRunner) runAutomaticCandidates(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, dir string, vcs []validatedCandidate, rep mediainspect.DetailedReport, sourceSize int64, sourceMod time.Time, progress *benchmarkProgressReporter) error {
	rules := record.AutomaticSearch
	// Verification on the final grid takes the grid's window count; every other
	// search has exactly two verification windows after its three discovery ones.
	grid := rules != nil && rules.HoldoutWindows == transcode.HoldoutWindowsFinalGrid
	if n := len(record.Samples); n < 5 || n > 5 && (!grid || n > transcode.MaxAutomaticGridWindows) {
		return fmt.Errorf("automatic search has no independent verification windows")
	}
	untilExhausted := rules != nil && rules.VerifyUntilExhausted
	automatic := &transcode.BenchmarkAutomaticEvidence{VerificationWindows: verificationWindowsKind(rules)}
	verification := &transcode.BenchmarkVerificationEvidence{Attempts: []transcode.BenchmarkVerificationAttempt{}}
	automatic.Verification = verification
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
	// A continued search starts from the earlier search's x265 measurements.
	reused := r.seedContinuation(ctx, w, record, evidence, vcs)
	run := func(stage string, stageRecord *BenchmarkRecord, candidates []validatedCandidate) error {
		ids := []string{}
		var reusedIDs []string
		var fresh []validatedCandidate
		for _, vc := range candidates {
			ids = append(ids, vc.candidate.ID)
			// Only the discovery windows are reused, never a holdout check.
			if reused[vc.candidate.ID] && len(stageRecord.Samples) == 3 && stageRecord.Samples[0].Index == record.Samples[0].Index {
				reusedIDs = append(reusedIDs, vc.candidate.ID)
				continue
			}
			fresh = append(fresh, vc)
		}
		automatic.Stages = append(automatic.Stages, transcode.BenchmarkAutomaticStage{Stage: stage, Candidates: ids, Reused: reusedIDs, Outcome: "running"})
		if len(fresh) == 0 {
			automatic.Stages[len(automatic.Stages)-1].Outcome = "completed"
			return nil
		}
		progress.SetPhase(stage)
		var err error
		if r.automaticHook != nil {
			err = r.automaticHook(ctx, stageRecord, evidence, fresh)
		} else {
			err = r.runPipelinedEncodeMetrics(ctx, w, stageRecord, evidence, dir, fresh, sourceSize, sourceMod, progress, nil)
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
				ExpectedBitDepth: f.vc.bitDepth, EvaluationReason: reasonSkippedQualityDominated, Uncertainties: []string{"not measured: " + f.leader + " failed quality"},
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
	// The initial hardware rungs that failed quality at discovery, whether
	// measured or skipped as dominated by the rung that did.
	initial := append([]validatedCandidate(nil), tested...)
	discoveryFailed := map[string]bool{}
	for _, e := range append(append([]transcode.BenchmarkCandidateEvaluation(nil), evidence.Decision.Evaluations...), skipped...) {
		if qualityRejection(e.EvaluationReason) || e.EvaluationReason == reasonSkippedQualityDominated {
			discoveryFailed[e.CandidateID] = true
		}
	}
	// Settings a failed holdout ruled out (rules only), and why.
	var qualityFailures []validatedCandidate
	var ruledOut []transcode.BenchmarkCandidateEvaluation
	fallbackPrefix := ""
	var fallbackStages [][]validatedCandidate
	// runFallback measures the next stage of the one fallback family (chosen
	// on first use) until a stage yields a discovery winner or none remain.
	runFallback := func() error {
		if fallbackPrefix == "" {
			fallbackPrefix = automaticFallback(evidence.Decision, rules != nil && len(qualityFailures) > 0)
			var hardware []validatedCandidate
			if record.HardwareSearch != nil && len(record.HardwareSearch.FallbackQualityValues) > 0 && fallbackPrefix == fallbackQualityPrefix {
				hardware = hardwareQualityFallback(vcs, initial, discoveryFailed, qualityFailures)
			}
			fallbackStages = automaticFallbackStages(vcs, fallbackPrefix, rules, hardware)
		}
		for len(fallbackStages) > 0 {
			var alternatives []validatedCandidate
			for _, vc := range fallbackStages[0] {
				if !ruledOutByQualityFailure(vc, qualityFailures) {
					alternatives = append(alternatives, vc)
				}
			}
			fallbackStages = fallbackStages[1:]
			if len(alternatives) == 0 {
				continue
			}
			measured, err := measure("testing_alternatives", &discovery, alternatives)
			if err != nil {
				return err
			}
			tested = append(tested, measured...)
			if err := r.runSelection(ctx, &discovery, evidence, tested, rep, sourceSize); err != nil {
				return err
			}
			automatic.Discovery = evidence.Decision
			// Without rules the family is a single stage. With rules a later
			// (slower) preset runs only when the earlier ones found nothing.
			if evidence.Decision.Winner != nil {
				break
			}
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
	// preferSafest sends the retry to the highest-quality eligible setting of the
	// winner's family instead of the next-smallest one.
	preferSafest := false
	verificationFailures := []transcode.BenchmarkCandidateEvaluation{}
	// passed is the smallest setting verified so far (only under
	// verify_until_exhausted, which keeps looking for a smaller one between it
	// and the failures); longest is the longest attempt, the time the next one
	// is expected to need.
	var passed *verifiedWinner
	var longest time.Duration
	// succeed ends the search with a verified winner.
	succeed := func(p *verifiedWinner) {
		evidence.Decision = p.decision
		automatic.VerifiedSamples = len(record.Samples)
		if p.role == transcode.VerificationRoleBisect {
			carryBisectedHardwareNote(evidence.Decision, p.chosen, record.HardwareSearch)
		} else {
			carryHardwareNote(evidence.Decision, p.chosen, record.HardwareSearch)
		}
		evidence.Decision.Evaluations = append(append(evidence.Decision.Evaluations, verificationFailures...), ruledOut...)
		appendAutomaticRejections(evidence.Decision, automatic.Discovery)
		automatic.WinnerWindows = winnerWindowScores(record, evidence, p.vc.candidate.ID)
		finish()
	}
	for attempt := 0; untilExhausted || attempt < 2; attempt++ {
		var id, role string
		// The decision that chose this attempt's candidate: its hardware note
		// describes that choice, whichever stage (discovery, fallback or retry) made it.
		chosen := evidence.Decision
		if passed != nil {
			// A setting passed: look for a smaller one between it and the failures.
			if id = bisectCandidateID(tested, evidence.Decision, passed.vc, qualityFailures, record.HardwareSearch); id == "" {
				break
			}
			role = transcode.VerificationRoleBisect
		} else {
			if evidence.Decision.Winner == nil {
				break
			}
			id, role = evidence.Decision.Winner.CandidateID, transcode.VerificationRoleSmallest
			if attempt > 0 {
				role = transcode.VerificationRoleNext
			}
			if preferSafest {
				id, role = safestEligibleIDWithin(tested, evidence.Decision, id, record.HardwareSearch), transcode.VerificationRoleSafest
			}
		}
		if untilExhausted && attempt > 0 && !verificationHasTime(record.searchBudget, longest) {
			verification.StoppedBy = transcode.VerificationStoppedTimeCap
			break
		}
		var winner validatedCandidate
		for _, vc := range tested {
			if vc.candidate.ID == id {
				winner = vc
				break
			}
		}
		attemptStart := time.Now()
		holdout := *record
		holdout.Samples = record.Samples[3:]
		if err := run(phaseVerifyingSamples, &holdout, []validatedCandidate{winner}); err != nil {
			if untilExhausted && searchTimeLimitReached(ctx, err) {
				// The limit fell inside this attempt: it measured part of the
				// windows, so the setting stays unverified.
				automatic.Stages[len(automatic.Stages)-1].Outcome = "stopped"
				verification.StoppedBy = transcode.VerificationStoppedTimeCap
				break
			}
			return err
		}
		if d := time.Since(attemptStart); d > longest {
			longest = d
		}
		if err := mergeAutomaticQuality(record, evidence, winner); err != nil {
			return err
		}
		if err := r.runSelection(ctx, record, evidence, []validatedCandidate{winner}, rep, sourceSize); err != nil {
			return err
		}
		if evidence.Decision.Winner != nil {
			verification.Attempts = append(verification.Attempts, verificationAttempt(record, evidence, id, role, transcode.VerificationPassed, ""))
			p := &verifiedWinner{vc: winner, decision: evidence.Decision, chosen: chosen, role: role}
			if !untilExhausted {
				succeed(p)
				return nil
			}
			passed, preferSafest = p, false
			blocked[id] = true
		} else {
			holdoutQuality := false
			reason := ""
			for _, e := range evidence.Decision.Evaluations {
				if e.CandidateID == id {
					reason = e.EvaluationReason
					if qualityRejection(e.EvaluationReason) {
						holdoutQuality = true
					}
				}
			}
			outcome := transcode.VerificationRejected
			if holdoutQuality {
				outcome = transcode.VerificationQualityFailed
			}
			verification.Attempts = append(verification.Attempts, verificationAttempt(record, evidence, id, role, outcome, reason))
			verificationFailures = append(verificationFailures, evidence.Decision.Evaluations...)
			blocked[id] = true
			if rules != nil && holdoutQuality {
				qualityFailures = append(qualityFailures, winner)
			}
			// The one retry after a quality failure goes to the safest eligible
			// setting, fallback stage included: the smallest one usually fails for
			// the same reason.
			preferSafest = rules != nil && holdoutQuality
		}
		var remaining []validatedCandidate
		for _, vc := range tested {
			if blocked[vc.candidate.ID] {
				continue
			}
			if rules != nil && ruledOutByQualityFailure(vc, qualityFailures) {
				ruledOut = append(ruledOut, notVerifiedEvaluation(automatic.Discovery, vc, id))
				continue
			}
			remaining = append(remaining, vc)
		}
		if err := r.runSelection(ctx, &discovery, evidence, remaining, rep, sourceSize); err != nil {
			return err
		}
		tested = remaining
		// Only the first failure can still lead to a verified fallback, unless
		// verification goes on: then whenever nothing eligible remains. Under
		// rules a quality failure goes to a safer measured setting when one
		// remains, and measures the quality fallback only when none does. A
		// search that already verified a setting has no use for a fallback.
		if passed == nil && (attempt == 0 || untilExhausted) && (evidence.Decision.Winner == nil || attempt == 0 && (rules == nil || !preferSafest)) {
			if err := runFallback(); err != nil {
				return err
			}
		}
	}
	if passed != nil {
		succeed(passed)
		return nil
	}
	// Discovery winners are not accepted without holdouts, even after the
	// confirmation allowance or search deadline has been exhausted.
	if verification.StoppedBy != "" {
		// Name the settings the time limit left unchecked: they passed
		// discovery, and are not failures.
		for _, vc := range tested {
			if blocked[vc.candidate.ID] || (rules != nil && ruledOutByQualityFailure(vc, qualityFailures)) {
				continue
			}
			ev := notVerifiedEvaluation(automatic.Discovery, vc, "")
			if ev.EvaluationReason == reasonNotVerified {
				ev.Uncertainties = append(append([]string(nil), ev.Uncertainties...), transcode.NotVerifiedTimeLimit)
				ev.Uncertainties = slices.DeleteFunc(ev.Uncertainties, func(u string) bool { return strings.HasSuffix(u, "failed verification on quality") })
				verification.Unverified = append(verification.Unverified, vc.candidate.ID)
				ruledOut = append(ruledOut, ev)
			}
		}
	}
	evidence.Decision = &transcode.BenchmarkDecision{DecisionReason: "no_verified_candidate", Evaluations: append(verificationFailures, ruledOut...)}
	appendAutomaticRejections(evidence.Decision, automatic.Discovery)
	finish()
	return nil
}

// verifiedWinner is a setting that passed verification: its decision, and the
// decision that chose it.
type verifiedWinner struct {
	vc       validatedCandidate
	decision *transcode.BenchmarkDecision
	chosen   *transcode.BenchmarkDecision
	role     string
}

// verificationWindowsKind names where the rules put the verification windows.
func verificationWindowsKind(rules *transcode.AutomaticSearchRules) string {
	if rules == nil {
		return ""
	}
	return rules.HoldoutWindows
}

// verificationHasTime reports whether another verification attempt, expected
// to take as long as the longest so far, fits before the search's hard
// deadline. Without a budget (an unbounded run) it always does.
func verificationHasTime(b *searchBudget, longest time.Duration) bool {
	if b == nil {
		return true
	}
	now := b.now()
	return b.continues(now) && now.Add(longest).Before(b.hardDeadline())
}

// searchTimeLimitReached reports an attempt cut short by the search's budget:
// the elastic watcher stopped it, or the hard deadline passed. A cancellation
// for any other reason (the user's, a shutdown) is not.
func searchTimeLimitReached(ctx context.Context, err error) bool {
	if errors.Is(context.Cause(ctx), errSearchBudgetStopped) {
		// A real failure that was under way when the watcher fired stays one.
		return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	return errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// verificationAttempt summarises a verification attempt for the result,
// from the candidate's merged quality evidence.
func verificationAttempt(record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, id, role, outcome, reason string) transcode.BenchmarkVerificationAttempt {
	a := transcode.BenchmarkVerificationAttempt{CandidateID: id, Role: role, Windows: len(record.Samples) - 3, Outcome: outcome, Reason: reason}
	for _, q := range evidence.Quality {
		if q.CandidateID == id && q.VMAF != nil {
			mean, p5, worst := q.VMAF.Mean, q.VMAF.P5, q.VMAF.WorstWindowMean
			a.VMAFMean, a.VMAFP5, a.WorstWindowMean = &mean, &p5, &worst
			break
		}
	}
	return a
}

// winnerWindowScores lists the winner's per-window sample VMAF scores in time
// order: the discovery windows it was selected on and the verification windows.
func winnerWindowScores(record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, id string) []transcode.BenchmarkWindowScore {
	var out []transcode.BenchmarkWindowScore
	for _, window := range record.Samples {
		for _, ms := range evidence.MetricSamples {
			if ms.CandidateID != id || ms.SampleIndex != window.Index || ms.VMAFStats == nil {
				continue
			}
			role := "verification"
			if window.Index < 3 {
				role = "discovery"
			}
			out = append(out, transcode.BenchmarkWindowScore{StartSeconds: window.StartSeconds, DurationSeconds: window.DurationSeconds, Role: role, VMAFMean: ms.VMAFStats.Mean, VMAFP5: ms.VMAFStats.P5, VMAFMin: ms.VMAFStats.Min})
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartSeconds < out[j].StartSeconds })
	return out
}

// bisectCandidateID returns the next setting to verify once one has passed
// after others failed on quality: of the eligible settings of the passed one's
// family strictly between the safest failure and the passed setting, the
// middle one, leaning to the more compressed. A pass verifies it as the new
// upper bound (a smaller file), a failure as the new lower one. It returns ""
// when nothing lies between, or when nothing failed (the first, smallest
// attempt passed, so nothing smaller is eligible).
func bisectCandidateID(tested []validatedCandidate, decision *transcode.BenchmarkDecision, passed validatedCandidate, failures []validatedCandidate, hardware *transcode.HardwareSearchRules) string {
	key, lowerIsBetter, ok := qualityFamilyKey(passed)
	if !ok {
		return ""
	}
	bound, found := 0, false
	for _, f := range failures {
		if fk, _, ok := qualityFamilyKey(f); !ok || fk != key {
			continue
		}
		if q := f.candidate.Quality; !found || lowerIsBetter && q < bound || !lowerIsBetter && q > bound {
			bound, found = q, true
		}
	}
	if !found {
		return ""
	}
	eligible := eligibleVerificationIDs(decision, passed.candidate.ID, hardware)
	var between []validatedCandidate
	for _, vc := range tested {
		if k, _, ok := qualityFamilyKey(vc); !ok || k != key || !eligible[vc.candidate.ID] {
			continue
		}
		q, pq := vc.candidate.Quality, passed.candidate.Quality
		if lowerIsBetter && q > pq && q < bound || !lowerIsBetter && q < pq && q > bound {
			between = append(between, vc)
		}
	}
	if len(between) == 0 {
		return ""
	}
	// Safest first: from the passed setting outward to the failure.
	sort.SliceStable(between, func(i, j int) bool {
		qi, qj := between[i].candidate.Quality, between[j].candidate.Quality
		if lowerIsBetter {
			return qi < qj
		}
		return qi > qj
	})
	return between[len(between)/2].candidate.ID
}

// safestEligibleID returns the candidate the second holdout verification
// should check: the highest-quality (lowest x265 CRF, highest VideoToolbox
// -q:v) setting that is still eligible in the selected candidate's quality
// family. Selection picks the smallest eligible setting, which after a quality
// failure tends to fail like the one before it. Eligibility is the decision's,
// so the quality limits and selection margin are not relaxed. A candidate
// outside a constant-quality family keeps the selected one.
func safestEligibleID(tested []validatedCandidate, decision *transcode.BenchmarkDecision, selectedID string) string {
	return safestEligibleIDWithin(tested, decision, selectedID, nil)
}

// safestEligibleIDWithin is safestEligibleID for a search that allows
// hardware: a VideoToolbox winner is only bumped to a safer VideoToolbox
// setting that is still within the hardware size margin of the smallest
// eligible x265 setting. The winner was chosen because it was within that
// margin, and a verified safer setting must not break the same promise.
func safestEligibleIDWithin(tested []validatedCandidate, decision *transcode.BenchmarkDecision, selectedID string, hardware *transcode.HardwareSearchRules) string {
	if decision == nil {
		return selectedID
	}
	eligible := eligibleVerificationIDs(decision, selectedID, hardware)
	var selected *validatedCandidate
	for i := range tested {
		if tested[i].candidate.ID == selectedID {
			selected = &tested[i]
		}
	}
	if selected == nil {
		return selectedID
	}
	key, lowerIsBetter, ok := qualityFamilyKey(*selected)
	if !ok {
		return selectedID
	}
	best := *selected
	for _, vc := range tested {
		if !eligible[vc.candidate.ID] {
			continue
		}
		k, _, ok := qualityFamilyKey(vc)
		if !ok || k != key {
			continue
		}
		q, bq := vc.candidate.Quality, best.candidate.Quality
		if lowerIsBetter && q < bq || !lowerIsBetter && q > bq {
			best = vc
		}
	}
	return best.candidate.ID
}

// eligibleVerificationIDs are the candidates a verification may still choose:
// those the decision found eligible, except a VideoToolbox setting (other than
// selectedID) beyond the hardware size margin of the smallest eligible x265 one.
func eligibleVerificationIDs(decision *transcode.BenchmarkDecision, selectedID string, hardware *transcode.HardwareSearchRules) map[string]bool {
	eligible := map[string]bool{}
	if decision == nil {
		return eligible
	}
	for _, e := range decision.Evaluations {
		if e.Eligible {
			eligible[e.CandidateID] = true
		}
	}
	if smallestSoftware := smallestEligibleSoftware(decision); hardware != nil && smallestSoftware > 0 {
		limit := float64(smallestSoftware) * (1 + hardware.MarginPercent/100)
		for _, e := range decision.Evaluations {
			if e.CandidateID != selectedID && e.Eligible && isVideoToolbox(e.VideoCodec) && float64(e.EstimatedBytes) > limit {
				eligible[e.CandidateID] = false
			}
		}
	}
	return eligible
}

// smallestEligibleSoftware is the size of the smallest eligible x265 candidate
// in a decision, zero when none qualified: the figure a media engine result is
// compared with.
func smallestEligibleSoftware(decision *transcode.BenchmarkDecision) int64 {
	var smallest int64
	if decision == nil {
		return 0
	}
	for _, e := range decision.Evaluations {
		if e.Eligible && !isVideoToolbox(e.VideoCodec) && (smallest == 0 || e.EstimatedBytes < smallest) {
			smallest = e.EstimatedBytes
		}
	}
	return smallest
}

// isVideoToolbox reports the media-engine codec; an empty codec means it too.
func isVideoToolbox(codec string) bool {
	return transcode.BenchmarkCandidateVideoCodec(transcode.BenchmarkCandidate{VideoCodec: codec}) == transcode.VideoCodecHEVCVideoToolbox
}

// notVerifiedEvaluation is a candidate's latest discovery evaluation after a
// failed holdout ruled it out. A setting that had passed discovery becomes
// not_verified, naming the setting whose quality failure ruled it out.
func notVerifiedEvaluation(discovery *transcode.BenchmarkDecision, vc validatedCandidate, failedID string) transcode.BenchmarkCandidateEvaluation {
	ev := transcode.BenchmarkCandidateEvaluation{CandidateID: vc.candidate.ID, CandidateIndex: vc.index, VideoCodec: transcode.BenchmarkCandidateVideoCodec(vc.candidate),
		Quality: vc.candidate.Quality, Preset: norm(vc.candidate.Preset), Tune: norm(vc.candidate.Tune), VideoProfile: vc.profile, PixelFormat: vc.pixelFormat, ExpectedBitDepth: vc.bitDepth, EvaluationReason: reasonNotVerified}
	if discovery != nil {
		for _, d := range discovery.Evaluations {
			if d.CandidateID == vc.candidate.ID {
				ev = d
				break
			}
		}
	}
	if ev.Eligible || ev.EvaluationReason == reasonNotVerified {
		ev.Eligible = false
		ev.EvaluationReason = reasonNotVerified
		ev.Uncertainties = append(append([]string(nil), ev.Uncertainties...), "not verified: "+failedID+" failed verification on quality")
	}
	return ev
}

// qualityRejection reports measured quality-policy failures. Incomplete
// evidence, encode errors and size rejections are not quality verdicts.
func qualityRejection(reason string) bool {
	switch reason {
	case optimization.ReasonBelowMinimumQuality, optimization.ReasonSampleBelowMinimum,
		"quality_vmaf_p5_below_minimum", "quality_vmaf_worst_window_below_minimum", "quality_vmaf_frames_below_limit_exceeded",
		"quality_cambi_mean_above_maximum", "quality_cambi_peak_above_maximum", reasonSelectionMarginNotMet:
		return true
	}
	return false
}

// qualityFamilyKey identifies a constant-quality libx265 or VideoToolbox
// candidate's family: every setting but its ID and quality value, plus the
// resolved output depth. ok is false for bitrate sweeps and other candidates.
func qualityFamilyKey(vc validatedCandidate) (key string, lowerIsBetter, ok bool) {
	c := vc.candidate
	codec := transcode.BenchmarkCandidateVideoCodec(c)
	if c.Quality <= 0 || c.AverageBitrateKbps != 0 || (codec != "libx265" && codec != "hevc_videotoolbox") {
		return "", false, false
	}
	c.ID, c.Quality = "", 0
	b, _ := json.Marshal(c)
	return fmt.Sprintf("%s|%d|%s|%s|%s", codec, vc.bitDepth, vc.profile, vc.pixelFormat, b), codec == "libx265", true
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
		k, lowerIsBetter, ok := qualityFamilyKey(vc)
		if !ok {
			order = append(order, "\x00"+vc.candidate.ID)
			families["\x00"+vc.candidate.ID] = &qualityFamily{members: []validatedCandidate{vc}}
			continue
		}
		if families[k] == nil {
			order = append(order, k)
			families[k] = &qualityFamily{lowerIsBetter: lowerIsBetter}
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
			c.EvaluationReason = reasonNotVerified
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

// carryHardwareNote keeps the reason a media-engine candidate was chosen on the
// verified decision. The holdout selects among a single candidate, so it cannot
// repeat the comparison; the note stays while the verified winner is still the
// candidate chosen. When the retry after a failed holdout verified a different,
// safer media-engine setting, that setting was never compared by the selector,
// so it gets its own accurate note instead of the chosen candidate's.
func carryHardwareNote(final, chosen *transcode.BenchmarkDecision, hardware *transcode.HardwareSearchRules) {
	if final == nil || final.Winner == nil || chosen == nil || chosen.Winner == nil {
		return
	}
	chosenNote := ""
	for _, u := range chosen.Winner.Uncertainties {
		if strings.HasPrefix(u, optimization.HardwareSelectedNote) {
			chosenNote = u
			break
		}
	}
	if chosenNote == "" {
		return
	}
	note := ""
	switch {
	case chosen.Winner.CandidateID == final.Winner.CandidateID:
		note = chosenNote
	case hardware != nil && isVideoToolbox(final.Winner.VideoCodec):
		note = optimization.HardwareSelectedNote + ": safer setting after a failed holdout; no software candidate qualified"
		if smallestEligibleSoftware(chosen) > 0 {
			note = fmt.Sprintf("%s: safer setting within the %.4g%% margin after a failed holdout", optimization.HardwareSelectedNote, hardware.MarginPercent)
		}
	default:
		return
	}
	final.Winner.Uncertainties = append(append([]string(nil), final.Winner.Uncertainties...), note)
}

// carryBisectedHardwareNote explains a media-engine setting that won by
// bisection: it sits between a setting that passed verification and one that
// failed, so the selector never compared it and the chosen decision's note
// (or its absence, when a software candidate led) does not describe it.
func carryBisectedHardwareNote(final, chosen *transcode.BenchmarkDecision, hardware *transcode.HardwareSearchRules) {
	if final == nil || final.Winner == nil || hardware == nil || !isVideoToolbox(final.Winner.VideoCodec) {
		return
	}
	note := optimization.HardwareSelectedNote + ": smaller setting found between a verified and a failed one; no software candidate qualified"
	if smallestEligibleSoftware(chosen) > 0 {
		note = fmt.Sprintf("%s: smaller setting found between a verified and a failed one, within the %.4g%% margin", optimization.HardwareSelectedNote, hardware.MarginPercent)
	}
	final.Winner.Uncertainties = append(append([]string(nil), final.Winner.Uncertainties...), note)
}
