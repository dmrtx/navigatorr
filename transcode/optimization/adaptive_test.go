package optimization

import (
	"testing"
)

func adaptiveEvalForTest(t *testing.T, policy QualityPolicy, meanScore float64) PolicyEvaluation {
	t.Helper()
	agg := AggregateSampleScores(policy.Metric(), []SampleScore{
		{SampleIndex: 0, Score: meanScore, Valid: true},
	})
	if !agg.Valid {
		t.Fatalf("aggregate invalid for score %v", meanScore)
	}
	color := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}
	return policy.Evaluate(agg, color)
}

func TestAdaptivePlanner_TypicalQ65WithLowerProbe(t *testing.T) {
	qualities := []int{55, 60, 65, 67, 68, 70, 75}
	policy := DefaultVMAFPolicy() // target 96.0, min 95.0, tol 0.5
	planner := NewAdaptivePlanner(qualities, 65, policy.TargetScore(), policy.Tolerance())

	// Deterministic start: initial 65 then high 75.
	if got := planner.ProbeOrder(); len(got) != 2 || got[0] != 65 || got[1] != 75 {
		t.Fatalf("ProbeOrder = %v, want [65 75]", got)
	}
	scores := map[int]float64{
		65: 96.5,
		75: 96.8,
		67: 96.6,
		68: 96.7,
		70: 96.75,
		60: 95.0,
	}
	// Sorted positions: 55:0, 60:1, 65:2, 67:3, 68:4, 70:5, 75:6.
	posByQuality := map[int]int{55: 0, 60: 1, 65: 2, 67: 3, 68: 4, 70: 5, 75: 6}
	var probed []int
	for {
		pos, ok := planner.NextProbe()
		if !ok {
			break
		}
		q := qualities[pos]
		probed = append(probed, q)
		score, known := scores[q]
		if !known {
			t.Fatalf("planner probed unexpected quality %d (probed so far %v)", q, probed)
		}
		eval := adaptiveEvalForTest(t, policy, score)
		planner.Observe(AdaptiveProbeObservation{SortedPos: posByQuality[q], Quality: q, Evaluation: eval, Valid: true})
		if planner.NeedsFallback() {
			t.Fatalf("unexpected fallback after probing %d: %s", q, planner.FallbackReason())
		}
		if len(probed) > len(qualities) {
			t.Fatalf("planner did not terminate (probed %v)", probed)
		}
	}
	// Expect 6 probes, skipping only 55 (provably below floor).
	if len(probed) != 6 {
		t.Fatalf("probed %v, want 6 probes skipping 55", probed)
	}
	for _, q := range probed {
		if q == 55 {
			t.Fatalf("quality 55 should have been skipped, probed %v", probed)
		}
	}
	floor, ok := planner.Floor()
	if !ok {
		t.Fatalf("expected floor to be established")
	}
	// floor = max(96.0, 96.8-0.5) = 96.3.
	if floor < 96.29 || floor > 96.31 {
		t.Fatalf("floor = %v, want 96.3", floor)
	}
}

func TestAdaptivePlanner_InitialBelowTargetProbesUpward(t *testing.T) {
	qualities := []int{55, 60, 65, 70, 75}
	policy := DefaultVMAFPolicy()
	planner := NewAdaptivePlanner(qualities, 65, policy.TargetScore(), policy.Tolerance())
	posByQuality := map[int]int{55: 0, 60: 1, 65: 2, 70: 3, 75: 4}
	scores := map[int]float64{
		65: 95.5, // below target 96.0
		75: 97.0, // floor = max(96.0, 96.5) = 96.5
		70: 96.6, // meets floor
	}
	var probed []int
	for {
		pos, ok := planner.NextProbe()
		if !ok {
			break
		}
		q := qualities[pos]
		probed = append(probed, q)
		score, known := scores[q]
		if !known {
			// Lower qualities should be skipped (initial fails materially: gap 1.0 > tol).
			t.Fatalf("planner probed unexpected quality %d (probed %v); lower should be skipped", q, probed)
		}
		eval := adaptiveEvalForTest(t, policy, score)
		planner.Observe(AdaptiveProbeObservation{SortedPos: posByQuality[q], Quality: q, Evaluation: eval, Valid: true})
		if planner.NeedsFallback() {
			t.Fatalf("unexpected fallback: %s", planner.FallbackReason())
		}
		if len(probed) > len(qualities) {
			t.Fatalf("planner did not terminate")
		}
	}
	// Must have probed upward (70) after initial+high.
	found70 := false
	for _, q := range probed {
		if q == 70 {
			found70 = true
		}
	}
	if !found70 {
		t.Fatalf("expected upward probe to 70, probed %v", probed)
	}
	if len(probed) != 3 {
		t.Fatalf("probed %v, want [65 75 70] with lower skipped", probed)
	}
}

func TestAdaptivePlanner_NonMonotonicFallback(t *testing.T) {
	qualities := []int{55, 60, 65, 70, 75}
	policy := DefaultVMAFPolicy()
	planner := NewAdaptivePlanner(qualities, 65, policy.TargetScore(), policy.Tolerance())
	posByQuality := map[int]int{55: 0, 60: 1, 65: 2, 70: 3, 75: 4}

	pos, ok := planner.NextProbe()
	if !ok || qualities[pos] != 65 {
		t.Fatalf("first probe = %v,%v, want 65,true", pos, ok)
	}
	planner.Observe(AdaptiveProbeObservation{
		SortedPos: posByQuality[65], Quality: 65,
		Evaluation: adaptiveEvalForTest(t, policy, 97.0), Valid: true,
	})
	pos, ok = planner.NextProbe()
	if !ok || qualities[pos] != 75 {
		t.Fatalf("second probe = %v,%v, want 75,true", pos, ok)
	}
	// High scores materially below initial: monotonicity violated.
	planner.Observe(AdaptiveProbeObservation{
		SortedPos: posByQuality[75], Quality: 75,
		Evaluation: adaptiveEvalForTest(t, policy, 95.0), Valid: true,
	})
	if !planner.NeedsFallback() {
		t.Fatalf("expected fallback on non-monotonic high<initial")
	}
	// Fallback drains remaining exhaustively in deterministic ascending order.
	var rest []int
	for {
		p, ok := planner.NextProbe()
		if !ok {
			break
		}
		rest = append(rest, qualities[p])
		// Observe with passing scores to keep draining.
		planner.Observe(AdaptiveProbeObservation{
			SortedPos: p, Quality: qualities[p],
			Evaluation: adaptiveEvalForTest(t, policy, 96.5), Valid: true,
		})
	}
	if len(rest) == 0 {
		t.Fatalf("expected fallback to evaluate remaining candidates")
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] < rest[i-1] {
			t.Fatalf("fallback order not ascending: %v", rest)
		}
	}
}

func TestAdaptivePlanner_CandidateFailureFallback(t *testing.T) {
	qualities := []int{60, 65, 70}
	policy := DefaultVMAFPolicy()
	planner := NewAdaptivePlanner(qualities, 65, policy.TargetScore(), policy.Tolerance())
	pos, ok := planner.NextProbe()
	if !ok {
		t.Fatalf("expected first probe")
	}
	// Inconclusive probe forces fallback.
	planner.Observe(AdaptiveProbeObservation{SortedPos: pos, Quality: qualities[pos], Valid: false})
	if !planner.NeedsFallback() {
		t.Fatalf("expected fallback on failed/inconclusive probe")
	}
}

func TestAdaptivePlanner_DeterministicProbeOrder(t *testing.T) {
	qualities := []int{55, 60, 65, 67, 68, 70, 75}
	mk := func() *AdaptivePlanner {
		return NewAdaptivePlanner(qualities, 65, DefaultVMAFPolicy().TargetScore(), DefaultVMAFPolicy().Tolerance())
	}
	a, b := mk(), mk()
	for i := 0; i < 2; i++ {
		pa, oka := a.NextProbe()
		pb, okb := b.NextProbe()
		if pa != pb || oka != okb {
			t.Fatalf("non-deterministic probe %d: (%d,%v) vs (%d,%v)", i, pa, oka, pb, okb)
		}
		// Observe identical passing scores to advance both identically.
		policy := DefaultVMAFPolicy()
		eval := adaptiveEvalForTest(t, policy, 96.5+float64(i)*0.1)
		a.Observe(AdaptiveProbeObservation{SortedPos: pa, Quality: qualities[pa], Evaluation: eval, Valid: true})
		b.Observe(AdaptiveProbeObservation{SortedPos: pb, Quality: qualities[pb], Evaluation: eval, Valid: true})
	}
	// Equidistant tie-break: [60,70] with initial 65 must start at lower (60).
	tie := NewAdaptivePlanner([]int{60, 70}, 65, 96.0, 0.5)
	if got := tie.ProbeOrder(); len(got) == 0 || got[0] != 60 {
		t.Fatalf("tie-break ProbeOrder = %v, want start at 60", got)
	}
	// Default initial quality applies when requested is out of range.
	def := NewAdaptivePlanner([]int{55, 60, 65, 70, 75}, 0, 96.0, 0.5)
	if got := def.ProbeOrder(); len(got) == 0 || got[0] != 65 {
		t.Fatalf("default initial ProbeOrder = %v, want start at 65", got)
	}
}

func TestAdaptivePlanner_ExhaustiveCompatibility(t *testing.T) {
	// Full 7-candidate set where adaptive skips 55; selector over the adaptive
	// evaluated subset must equal selector over all (skipped 55 is below floor and
	// cannot win among target-reaching candidates regardless of estimated size).
	policy := DefaultVMAFPolicy()
	sdrColor := ColorInfo{ColorPrimaries: "bt709", ColorTransfer: "bt709"}
	scores := map[string]float64{
		"q55": 94.0, "q60": 95.0, "q65": 96.5, "q67": 96.6, "q68": 96.7, "q70": 96.75, "q75": 96.8,
	}
	sizes := map[string]int64{
		"q55": 500000000, "q60": 600000000, "q65": 700000000, "q67": 750000000,
		"q68": 800000000, "q70": 900000000, "q75": 1000000000,
	}
	ids := []string{"q55", "q60", "q65", "q67", "q68", "q70", "q75"}
	mkInput := func(id string) CandidateInput {
		return CandidateInput{
			CandidateID:   id,
			EncodeSuccess: true,
			ColorInfo:     sdrColor,
			AggregateResult: AggregateSampleScores(MetricTypeVMAF, []SampleScore{
				{SampleIndex: 0, Score: scores[id], Valid: true},
			}),
			EstimatedOutput: EstimationResult{
				SuitableForSelection: true,
				EstimatedVideoBytes:  sizes[id] - 100000000,
				EstimatedTotalBytes:  sizes[id],
			},
		}
	}
	var full []CandidateInput
	for _, id := range ids {
		full = append(full, mkInput(id))
	}
	fullRes := SelectCandidate(SelectorInput{Policy: policy, Candidates: full})
	if fullRes.Winner == nil {
		t.Fatalf("expected full-set winner")
	}
	// Adaptive evaluated subset skips q55 (below floor 96.3 and below minimum 95.0
	// on mean; per-sample minimum also preserved via policy evaluation).
	var subset []CandidateInput
	for _, id := range []string{"q60", "q65", "q67", "q68", "q70", "q75"} {
		subset = append(subset, mkInput(id))
	}
	subRes := SelectCandidate(SelectorInput{Policy: policy, Candidates: subset})
	if subRes.Winner == nil {
		t.Fatalf("expected subset winner")
	}
	if subRes.Winner.CandidateID != fullRes.Winner.CandidateID {
		t.Fatalf("subset winner %q != full winner %q; early stop not compatible",
			subRes.Winner.CandidateID, fullRes.Winner.CandidateID)
	}
	if subRes.DecisionReason != fullRes.DecisionReason {
		t.Fatalf("subset reason %q != full reason %q", subRes.DecisionReason, fullRes.DecisionReason)
	}
	if fullRes.Winner.CandidateID != "q65" {
		t.Fatalf("full winner = %q, want q65 (lowest meeting floor 96.3)", fullRes.Winner.CandidateID)
	}
}

func TestAdaptivePlanner_HighEndpointCannotEstablishFloor(t *testing.T) {
	// Even the highest quality cannot reach target: adaptive must fall back so the
	// minimum-meeting branch is preserved exhaustively.
	qualities := []int{60, 65, 70}
	policy := DefaultVMAFPolicy()
	planner := NewAdaptivePlanner(qualities, 65, policy.TargetScore(), policy.Tolerance())
	posByQuality := map[int]int{60: 0, 65: 1, 70: 2}
	// Initial 65 below target, high 70 also below target.
	seq := []int{65, 70}
	scores := map[int]float64{65: 95.2, 70: 95.8}
	for _, q := range seq {
		pos, ok := planner.NextProbe()
		if !ok {
			t.Fatalf("expected probe for %d", q)
		}
		if qualities[pos] != q {
			t.Fatalf("probe = %d, want %d", qualities[pos], q)
		}
		planner.Observe(AdaptiveProbeObservation{
			SortedPos: posByQuality[q], Quality: q,
			Evaluation: adaptiveEvalForTest(t, policy, scores[q]), Valid: true,
		})
	}
	if !planner.NeedsFallback() {
		t.Fatalf("expected fallback when high endpoint cannot reach target")
	}
}
