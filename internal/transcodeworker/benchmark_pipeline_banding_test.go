package transcodeworker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// Deferred banding: CAMBI is measured only after a candidate's VMAF results are
// in and no VMAF limit rejects it.

// bandingWindow is one sample window of frames at 25 fps; a window of 100
// frames is 4 s, so its mean is the worst window of the sample.
func bandingWindow(t *testing.T, index int, scores ...float64) (*float64, *quality.VMAFStats) {
	t.Helper()
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	frames := make([]map[string]any, len(scores))
	sum := 0.0
	for i, s := range scores {
		frames[i] = map[string]any{"frameNum": i, "metrics": map[string]any{"vmaf": s}}
		sum += s
	}
	data, _ := json.Marshal(map[string]any{"frames": frames})
	stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, index, float64(index)*10, 25, 1, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &stats.Mean, &stats
}

func level(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// bandingShared is a pipeline over one candidate per entry of windows, each a
// list of per-window frame scores, with every CAMBI unit deferred.
func bandingShared(t *testing.T, gate bandingGate, enforcement string, candidates ...[][]float64) *pipelineShared {
	t.Helper()
	p5, worst := 84.0, 85.0
	record := &BenchmarkRecord{Mode: "size", Metric: "vmaf", bandingGate: gate,
		Quality: &transcode.BenchmarkQualityConfig{
			VMAF:    &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 92, Minimum: 88, P5Minimum: &p5, WorstWindowMinimum: &worst, WorstWindowSeconds: 4, GuardrailEnforcement: enforcement},
			Banding: &transcode.BenchmarkBandingConfig{Enabled: true, Enforcement: "reject"}}}
	n := len(candidates[0])
	for i := 0; i < n; i++ {
		record.Samples = append(record.Samples, transcode.BenchmarkSampleWindow{Index: i, StartSeconds: float64(i) * 10, DurationSeconds: 10, CenterSeconds: float64(i)*10 + 5})
	}
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	sh := &pipelineShared{record: record, evidence: &BenchmarkExecutionEvidence{}, runVMAF: true, runCAMBI: true, deferCAMBI: true, vmafModel: &model}
	for ord, windows := range candidates {
		sh.vcs = append(sh.vcs, validatedCandidate{index: ord, candidate: transcode.BenchmarkCandidate{ID: "cand" + itoa(ord), VideoCodec: "libx265", Quality: 20 + ord}})
		for i, frames := range windows {
			mean, stats := bandingWindow(t, i, frames...)
			sh.metricOut = append(sh.metricOut, pipelineMetricOutcome{executed: true, vmaf: mean, vmafStats: stats, cambiDeferred: true, entry: &BenchmarkMetricSampleResult{}})
		}
	}
	return sh
}

func passingWindows(n int) [][]float64 {
	out := make([][]float64, n)
	for i := range out {
		out[i] = level(100, 92)
	}
	return out
}

func skipped(sh *pipelineShared, vcOrd int) bool {
	n := len(sh.record.Samples)
	for i := 0; i < n; i++ {
		if sh.metricOut[vcOrd*n+i].cambiSkipped != sh.metricOut[vcOrd*n].cambiSkipped {
			return false
		}
	}
	return sh.metricOut[vcOrd*n].cambiSkipped
}

func TestDeferredBandingMeasuredWhenVMAFPasses(t *testing.T) {
	for _, gate := range []bandingGate{bandingGateComplete, bandingGateMonotone} {
		sh := bandingShared(t, gate, "reject", passingWindows(3))
		todo := sh.planDeferredCAMBI()
		if !slices.Equal(todo, []int{0, 1, 2}) || skipped(sh, 0) {
			t.Fatalf("gate %d: a candidate no VMAF limit rejects must be measured; todo %v", gate, todo)
		}
	}
}

func TestDeferredBandingSkippedAfterASampleBelowTheMinimum(t *testing.T) {
	for _, gate := range []bandingGate{bandingGateComplete, bandingGateMonotone} {
		windows := passingWindows(3)
		windows[1] = level(100, 86)
		sh := bandingShared(t, gate, "reject", windows, passingWindows(3))
		todo := sh.planDeferredCAMBI()
		if !skipped(sh, 0) || skipped(sh, 1) {
			t.Fatalf("gate %d: skipped %v/%v", gate, skipped(sh, 0), skipped(sh, 1))
		}
		if !slices.Equal(todo, []int{3, 4, 5}) {
			t.Fatalf("gate %d: only the passing candidate is measured, got %v", gate, todo)
		}
		for i := 0; i < 3; i++ {
			if !sh.metricOut[i].entry.CAMBISkipped {
				t.Fatalf("gate %d: skipped unit %d is not marked in its evidence row", gate, i)
			}
		}
	}
}

// A percentile can recover when the verification windows join, so only the
// complete gate skips on it.
func TestDeferredBandingP5OnlyRejectsOnACompleteGate(t *testing.T) {
	// 12% of the frames at 70, the rest at 95: sample mean 92, p5 70.
	lowTail := append(level(12, 70), level(88, 95)...)
	windows := [][]float64{lowTail, lowTail, lowTail}
	for _, tc := range []struct {
		gate        bandingGate
		enforcement string
		skip        bool
	}{
		{bandingGateComplete, "reject", true},
		{bandingGateMonotone, "reject", false},
		{bandingGateComplete, "observe", false},
	} {
		sh := bandingShared(t, tc.gate, tc.enforcement, windows)
		sh.planDeferredCAMBI()
		if skipped(sh, 0) != tc.skip {
			t.Fatalf("gate %d, guardrails %s: skipped %v, want %v", tc.gate, tc.enforcement, skipped(sh, 0), tc.skip)
		}
	}
}

// The worst window is a minimum over windows, which more windows cannot raise:
// it rejects on both gates.
func TestDeferredBandingWorstWindowRejectsOnBothGates(t *testing.T) {
	// 8 s in two 4 s windows at 80 and 98: sample mean 89, worst window 80.
	dip := append(level(100, 80), level(100, 98)...)
	for _, gate := range []bandingGate{bandingGateComplete, bandingGateMonotone} {
		sh := bandingShared(t, gate, "reject", [][]float64{dip, dip, dip})
		sh.planDeferredCAMBI()
		if !skipped(sh, 0) {
			t.Fatalf("gate %d: a worst window below its limit left the banding to measure", gate)
		}
	}
	sh := bandingShared(t, bandingGateComplete, "observe", [][]float64{dip, dip, dip})
	sh.planDeferredCAMBI()
	if skipped(sh, 0) {
		t.Fatal("an observe-only guardrail skipped the banding")
	}
}

// Missing VMAF evidence settles nothing.
func TestDeferredBandingMeasuredWhenVMAFIsIncomplete(t *testing.T) {
	sh := bandingShared(t, bandingGateComplete, "reject", passingWindows(3))
	sh.metricOut[1].vmaf, sh.metricOut[1].vmafStats = nil, nil
	if len(sh.planDeferredCAMBI()) != 3 || skipped(sh, 0) {
		t.Fatal("incomplete VMAF evidence skipped the banding")
	}
}

// A skipped unit is not incomplete evidence, and the VMAF rejection keeps its
// precedence over a CAMBI one.
func TestDeferredBandingSkipIsNotIncompleteEvidence(t *testing.T) {
	windows := passingWindows(3)
	windows[0] = level(100, 80)
	sh := bandingShared(t, bandingGateComplete, "reject", windows)
	sh.planDeferredCAMBI()
	e, reject := sh.evaluateCandidateQuality(0, "cand0")
	if reject != "quality_vmaf_p5_below_minimum" {
		t.Fatalf("reject %q, want the VMAF reason", reject)
	}
	if !slices.Contains(e.ReasonCodes, reasonCAMBINotMeasured) || slices.Contains(e.ReasonCodes, "quality_cambi_evidence_incomplete") {
		t.Fatalf("reason codes %v", e.ReasonCodes)
	}
	if e.CAMBI != nil {
		t.Fatal("banding evidence invented for skipped units")
	}
}

// The skip marker travels with the saved sample rows, so the merge of the
// discovery and verification evidence still treats the units as skipped.
func TestDeferredBandingMarkerSurvivesTheEvidenceMerge(t *testing.T) {
	windows := passingWindows(3)
	windows[0] = level(100, 80)
	sh := bandingShared(t, bandingGateComplete, "reject", windows)
	sh.planDeferredCAMBI()
	evidence := &BenchmarkExecutionEvidence{}
	for i := range sh.record.Samples {
		out := sh.metricOut[i]
		evidence.MetricSamples = append(evidence.MetricSamples, BenchmarkMetricSampleResult{CandidateID: "cand0", SampleIndex: i, VMAF: out.vmaf, VMAFStats: out.vmafStats, CAMBISkipped: out.entry.CAMBISkipped})
	}
	if err := mergeAutomaticQuality(context.Background(), sh.record, evidence, sh.vcs[0]); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Quality) != 1 || !slices.Contains(evidence.Quality[0].ReasonCodes, reasonCAMBINotMeasured) || slices.Contains(evidence.Quality[0].ReasonCodes, "quality_cambi_evidence_incomplete") {
		t.Fatalf("merged reason codes %+v", evidence.Quality)
	}
	agg := evidence.CandidateMetrics[len(evidence.CandidateMetrics)-1].Aggregate
	if agg.Valid {
		t.Fatalf("a candidate a VMAF limit rejects is valid after the merge: %+v", agg)
	}
}
