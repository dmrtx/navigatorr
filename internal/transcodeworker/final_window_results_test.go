package transcodeworker

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// Final validation reports each window beside the pooled verdict, and fresh
// windows of an ungated plan never decide it.
func TestSplitFinalWindowResultsKeepsFreshWindowsOutOfTheVerdict(t *testing.T) {
	q := &transcode.QualityValidationPlan{
		Samples:      []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 100, DurationSeconds: 10}, {Index: 1, StartSeconds: 700, DurationSeconds: 10}},
		FreshSamples: []transcode.BenchmarkSampleWindow{{Index: 2, StartSeconds: 400, DurationSeconds: 10}, {Index: 3, StartSeconds: 1000, DurationSeconds: 10}},
	}
	stats := func(mean float64) *quality.VMAFStats {
		return &quality.VMAFStats{Mean: mean, P5: mean - 3, Min: mean - 9}
	}
	results := []finalWindowResult{{vmaf: stats(92)}, {vmaf: stats(89)}, {vmaf: stats(70)}, {vmaf: stats(91)}}
	grid, scores, fresh, unmeasured := splitFinalWindowResults(q, results)
	if len(grid) != 2 || len(fresh) != 2 || len(scores) != 4 || unmeasured != nil {
		t.Fatalf("grid %d fresh %d scores %d unmeasured %v", len(grid), len(fresh), len(scores), unmeasured)
	}
	var order []float64
	roles := map[string]int{}
	for _, s := range scores {
		order = append(order, s.StartSeconds)
		roles[s.Role]++
	}
	if order[0] != 100 || order[1] != 400 || order[2] != 700 || order[3] != 1000 || roles["grid"] != 2 || roles["fresh"] != 2 {
		t.Fatalf("scores %+v", scores)
	}
	if scores[1].VMAFMean != 70 || scores[1].VMAFP5 != 67 || scores[1].VMAFMin != 61 || scores[1].DurationSeconds != 10 {
		t.Fatalf("fresh row %+v", scores[1])
	}

	// A fresh window that could not be measured leaves the grid result alone
	// and is reported with its reason; the other fresh window keeps its scores.
	results = []finalWindowResult{{vmaf: stats(92)}, {vmaf: stats(89)}, {unmeasured: "quality_final_sample_misaligned"}, {vmaf: stats(70)}}
	grid, scores, fresh, unmeasured = splitFinalWindowResults(q, results)
	if len(grid) != 2 || len(fresh) != 1 || len(scores) != 3 || fresh[0].Mean != 70 {
		t.Fatalf("unmeasured fresh window: grid %d fresh %v scores %d", len(grid), fresh, len(scores))
	}
	if len(unmeasured) != 1 || unmeasured[0] != (transcode.UnmeasuredWindow{StartSeconds: 400, DurationSeconds: 10, ReasonCode: "quality_final_sample_misaligned"}) {
		t.Fatalf("unmeasured report %+v", unmeasured)
	}
	// A plan that gates its fresh windows judges them with the grid.
	q.GateFreshSamples = true
	results = []finalWindowResult{{vmaf: stats(92)}, {vmaf: stats(89)}, {vmaf: stats(70)}, {vmaf: stats(91)}}
	grid, scores, fresh, unmeasured = splitFinalWindowResults(q, results)
	if len(grid) != 4 || len(fresh) != 2 || len(scores) != 4 || unmeasured != nil {
		t.Fatalf("gated: verdict %d fresh %d scores %d unmeasured %v", len(grid), len(fresh), len(scores), unmeasured)
	}
	q.GateFreshSamples = false
	// Without fresh windows the report is the grid alone.
	q.FreshSamples = nil
	grid, scores, fresh, _ = splitFinalWindowResults(q, results[:2])
	if len(grid) != 2 || len(fresh) != 0 || len(scores) != 2 {
		t.Fatalf("grid only: %d %d %d", len(grid), len(fresh), len(scores))
	}
}
