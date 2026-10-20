package transcodeworker

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// Final validation reports each window beside the pooled verdict, and fresh
// windows never decide it.
func TestSplitFinalWindowResultsKeepsFreshWindowsOutOfTheVerdict(t *testing.T) {
	q := &transcode.QualityValidationPlan{
		Samples:      []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 100, DurationSeconds: 10}, {Index: 1, StartSeconds: 700, DurationSeconds: 10}},
		FreshSamples: []transcode.BenchmarkSampleWindow{{Index: 2, StartSeconds: 400, DurationSeconds: 10}, {Index: 3, StartSeconds: 1000, DurationSeconds: 10}},
	}
	stats := func(mean float64) *quality.VMAFStats {
		return &quality.VMAFStats{Mean: mean, P5: mean - 3, Min: mean - 9}
	}
	results := []finalWindowResult{{vmaf: stats(92)}, {vmaf: stats(89)}, {vmaf: stats(70)}, {vmaf: stats(91)}}
	grid, scores, fresh := splitFinalWindowResults(q, results, -1)
	if len(grid) != 2 || len(fresh) != 2 || len(scores) != 4 {
		t.Fatalf("grid %d fresh %d scores %d", len(grid), len(fresh), len(scores))
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

	// A fresh window that failed to measure leaves the grid result alone.
	results = []finalWindowResult{{vmaf: stats(92)}, {vmaf: stats(89)}, {code: "quality_final_sample_misaligned"}, {}}
	grid, scores, fresh = splitFinalWindowResults(q, results, 2)
	if len(grid) != 2 || len(fresh) != 0 || len(scores) != 2 {
		t.Fatalf("dropped fresh windows: grid %d fresh %d scores %d", len(grid), len(fresh), len(scores))
	}
	// A fresh window measured before another one failed keeps its scores.
	results = []finalWindowResult{{vmaf: stats(92)}, {vmaf: stats(89)}, {vmaf: stats(70)}, {code: "quality_final_sample_misaligned"}}
	grid, scores, fresh = splitFinalWindowResults(q, results, 3)
	if len(grid) != 2 || len(fresh) != 1 || len(scores) != 3 || fresh[0].Mean != 70 {
		t.Fatalf("lost the fresh window measured before the failure: grid %d fresh %v scores %d", len(grid), fresh, len(scores))
	}
	// Without fresh windows the report is the grid alone.
	q.FreshSamples = nil
	grid, scores, fresh = splitFinalWindowResults(q, results[:2], -1)
	if len(grid) != 2 || len(fresh) != 0 || len(scores) != 2 {
		t.Fatalf("grid only: %d %d %d", len(grid), len(fresh), len(scores))
	}
}
