package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// freshGatePlan is a two-window grid with two fresh windows, held to the size
// goal's VMAF limits (mean and each window 88, p5 84, worst window 85).
func freshGatePlan(gate bool) *transcode.QualityValidationPlan {
	f := func(v float64) *float64 { return &v }
	return &transcode.QualityValidationPlan{
		Metric:           "vmaf",
		Samples:          []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 100, DurationSeconds: 2}, {Index: 1, StartSeconds: 700, DurationSeconds: 2}},
		FreshSamples:     []transcode.BenchmarkSampleWindow{{Index: 2, StartSeconds: 400, DurationSeconds: 2}, {Index: 3, StartSeconds: 1000, DurationSeconds: 2}},
		GateFreshSamples: gate,
		Quality: transcode.BenchmarkQualityConfig{
			VMAF:            &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 88, GuardrailEnforcement: "reject", P5Minimum: f(84), WorstWindowMinimum: f(85), WorstWindowSeconds: 1},
			FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"},
		},
	}
}

// freshGateWindow is one window's VMAF evidence: eight frames at 4 FPS, every
// frame scoring score, parsed exactly as a real libvmaf log is.
func freshGateWindow(t *testing.T, index int, score float64) *quality.VMAFStats {
	t.Helper()
	type frame struct {
		FrameNum int                `json:"frameNum"`
		Metrics  map[string]float64 `json:"metrics"`
	}
	var frames []frame
	for i := range 8 {
		frames = append(frames, frame{FrameNum: i, Metrics: map[string]float64{"vmaf": score}})
	}
	data, err := json.Marshal(map[string]any{"frames": frames})
	if err != nil {
		t.Fatal(err)
	}
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, index, 0, 4, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &stats
}

// judgeFreshGate runs the windows' results through the same split and
// judgement validateFinalPerceptual applies after measuring.
func judgeFreshGate(q *transcode.QualityValidationPlan, results []finalWindowResult) (*transcode.FinalQualityEvidence, error) {
	e := &transcode.FinalQualityEvidence{Verdict: "fail"}
	verdict, scores, _, unmeasured := splitFinalWindowResults(q, results)
	e.Windows, e.FreshUnmeasured = scores, unmeasured
	return judgeFinalWindows(e, q, verdict, true, false, false, false)
}

// A fresh window below a VMAF limit fails the verdict when the plan gates
// fresh windows, and the same measurements pass when it does not.
func TestFreshWindowBelowLimitFailsOnlyAGatedVerdict(t *testing.T) {
	results := func() []finalWindowResult {
		return []finalWindowResult{{vmaf: freshGateWindow(t, 0, 95)}, {vmaf: freshGateWindow(t, 1, 94)}, {vmaf: freshGateWindow(t, 2, 80)}, {vmaf: freshGateWindow(t, 3, 93)}}
	}
	ungated, err := judgeFreshGate(freshGatePlan(false), results())
	if err != nil || ungated.Verdict != "pass" || len(ungated.ReasonCodes) != 0 {
		t.Fatalf("ungated verdict changed by a fresh window: %+v %v", ungated, err)
	}
	if ungated.VMAF == nil || ungated.VMAF.Mean != 94.5 || ungated.VMAF.FramesTotal != 16 {
		t.Fatalf("ungated statistics must pool the grid alone: %+v", ungated.VMAF)
	}

	gated, err := judgeFreshGate(freshGatePlan(true), results())
	if err == nil || gated.Verdict != "fail" {
		t.Fatalf("gated plan passed a fresh window at VMAF 80: %+v", gated)
	}
	for _, code := range []string{"quality_vmaf_sample_below_minimum", "quality_vmaf_p5_below_minimum", "quality_vmaf_worst_window_below_minimum"} {
		if !slices.Contains(gated.ReasonCodes, code) {
			t.Fatalf("gated reason codes %v lack %s", gated.ReasonCodes, code)
		}
	}
	if gated.VMAF == nil || gated.VMAF.FramesTotal != 32 || gated.VMAF.Min != 80 {
		t.Fatalf("gated statistics must pool grid and fresh windows: %+v", gated.VMAF)
	}

	// Fresh windows within every limit pass a gated plan too.
	clean := results()
	clean[2] = finalWindowResult{vmaf: freshGateWindow(t, 2, 91)}
	if e, err := judgeFreshGate(freshGatePlan(true), clean); err != nil || e.Verdict != "pass" {
		t.Fatalf("gated plan failed clean fresh windows: %+v %v", e, err)
	}
}

// A fresh window whose score cannot be taken fails a gated plan like a grid
// window, and is reported, not dropped, by an ungated one, whose other fresh
// window is still measured.
func TestUnmeasurableFreshWindow(t *testing.T) {
	broken := errors.New("vmaf log incomplete")
	measureWithCode := func(failing int, code string) (func(context.Context, int) finalWindowResult, *atomic.Int32) {
		calls := &atomic.Int32{}
		return func(ctx context.Context, pos int) finalWindowResult {
			calls.Add(1)
			if pos == failing {
				return finalWindowResult{code: code, err: broken}
			}
			return finalWindowResult{vmaf: freshGateWindow(t, pos, 93)}
		}, calls
	}
	measureWith := func(failing int) (func(context.Context, int) finalWindowResult, *atomic.Int32) {
		return measureWithCode(failing, "quality_vmaf_evidence_incomplete")
	}

	// Ungated: the first fresh window fails to measure.
	q := freshGatePlan(false)
	measure, calls := measureWith(2)
	results, failed, err := runFinalWindows(context.Background(), 4, 1, measureFreshWindows(q, measure))
	if err != nil || failed != -1 || calls.Load() != 4 {
		t.Fatalf("ungated: failed %d calls %d err %v", failed, calls.Load(), err)
	}
	e, err := judgeFreshGate(q, results)
	if err != nil || e.Verdict != "pass" {
		t.Fatalf("ungated verdict changed by an unmeasurable fresh window: %+v %v", e, err)
	}
	want := []transcode.UnmeasuredWindow{{StartSeconds: 400, DurationSeconds: 2, ReasonCode: "quality_vmaf_evidence_incomplete"}}
	if !slices.Equal(e.FreshUnmeasured, want) {
		t.Fatalf("unmeasured fresh window not reported: %+v", e.FreshUnmeasured)
	}
	fresh := 0
	for _, w := range e.Windows {
		if w.Role == transcode.WindowRoleFresh {
			fresh++
			if w.StartSeconds != 1000 {
				t.Fatalf("wrong fresh window reported: %+v", w)
			}
		}
	}
	if fresh != 1 || len(e.Windows) != 3 {
		t.Fatalf("the measurable fresh window must still be reported: %+v", e.Windows)
	}

	// Gated: the same failure fails validation with the window's own reason.
	q = freshGatePlan(true)
	measure, _ = measureWith(2)
	results, failed, err = runFinalWindows(context.Background(), 4, 1, measureFreshWindows(q, measure))
	if err != nil || failed != 2 || results[2].code != "quality_vmaf_evidence_incomplete" || !errors.Is(results[2].err, broken) || results[2].unmeasured != "" {
		t.Fatalf("gated: failed %d result %+v err %v", failed, results[2], err)
	}

	// A fresh window whose pictures are missing from the encode or out of
	// place fails even an ungated plan: that is lost content, not a score.
	for _, code := range []string{"quality_final_sample_misaligned", "quality_final_sample_extract_failed"} {
		measure, _ = measureWithCode(2, code)
		results, failed, err = runFinalWindows(context.Background(), 4, 1, measureFreshWindows(freshGatePlan(false), measure))
		if err != nil || failed != 2 || results[2].code != code || results[2].unmeasured != "" {
			t.Fatalf("ungated %s: failed %d result %+v err %v", code, failed, results[2], err)
		}
	}

	// A grid window that cannot be measured fails either way.
	for _, gate := range []bool{false, true} {
		measure, _ = measureWith(1)
		results, failed, err = runFinalWindows(context.Background(), 4, 1, measureFreshWindows(freshGatePlan(gate), measure))
		if err != nil || failed != 1 || results[1].code == "" {
			t.Fatalf("gate %v: grid failure lost: failed %d %+v %v", gate, failed, results[1], err)
		}
	}
}

// Frozen plans without the gate keep their digest: the field is omitted until
// it is set.
func TestGateFreshSamplesLeavesUngatedPlanDigestAlone(t *testing.T) {
	q := freshGatePlan(false)
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["gate_fresh_samples"]; ok {
		t.Fatalf("ungated plan serialises the gate: %s", data)
	}
	ungated, err := transcode.DigestPlan(&transcode.Plan{QualityValidation: q})
	if err != nil {
		t.Fatal(err)
	}
	gated, err := transcode.DigestPlan(&transcode.Plan{QualityValidation: freshGatePlan(true)})
	if err != nil || gated == ungated {
		t.Fatalf("the gate must be part of the plan digest: %s %s %v", gated, ungated, err)
	}
}
