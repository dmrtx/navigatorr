package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

func TestCompactBenchmarkExplainsNoWinnerAndActualAudioPlan(t *testing.T) {
	limit := 6.0
	res := &action.ActionResult{ActionName: "benchmark_transcode", Status: action.StatusCompleted, State: map[string]any{
		"optimization_enabled": true,
		"benchmark_request":    transcode.BenchmarkRequest{Quality: &transcode.BenchmarkQualityConfig{Banding: &transcode.BenchmarkBandingConfig{Enforcement: "reject", MaxMean: &limit}}},
		"benchmark_decision":   transcode.BenchmarkDecision{DecisionReason: "all_candidates_invalid", Evaluations: []transcode.BenchmarkCandidateEvaluation{{CandidateID: "crf16", EvaluationReason: "quality_cambi_mean_above_maximum"}}},
		"benchmark_quality":    []transcode.BenchmarkCandidateQualityEvidence{{CandidateID: "crf16", ReasonCodes: []string{"quality_cambi_mean_above_maximum"}, CAMBI: &quality.CAMBIStats{Mean: 8.86, Max: 17.64, WorstSourceSec: 1105.8}}},
		"plan":                 transcode.Plan{AudioMode: "compact"},
		"original":             map[string]any{"audio": []map[string]any{{"index": 1, "codec": "eac3", "channels": 6, "bit_rate": 640000}, {"index": 2, "codec": "dts", "channels": 2, "language": "jpn"}}, "irrelevant": strings.Repeat("x", 2000)},
	}}
	// Exercise the decoded persisted JSON shape, not only typed live values.
	data, _ := json.Marshal(res)
	var persisted action.ActionResult
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	s := toCompactSummary(&persisted).Benchmark
	if s == nil || s.Outcome != "no_acceptable_candidate" || len(s.Rejected) != 1 {
		t.Fatalf("bad summary: %+v", s)
	}
	check := s.Rejected[0].Checks[0]
	if check.Actual == nil || *check.Actual != 8.86 || check.Limit == nil || *check.Limit != 6 || !strings.Contains(check.Reason, "additional banding") {
		t.Fatalf("bad check: %+v", check)
	}
	if s.Rejected[0].ReviewSourceSeconds == nil || *s.Rejected[0].ReviewSourceSeconds != 1105.8 {
		t.Fatal("missing review location")
	}
	if len(s.Audio) != 2 || s.Audio[0].Operation != "copy" || s.Audio[0].OutputCodec != "eac3" || s.Audio[0].TargetKbps != 0 || s.Audio[1].TargetKbps != 192 {
		t.Fatalf("incorrect audio conversion claim: %+v", s.Audio)
	}
	if !strings.Contains(s.NextStep, "optimization.enabled=false") || !strings.Contains(s.AudioNote, "does not encode") {
		t.Fatalf("missing next step or evidence limits: %+v", s)
	}
	b, _ := json.Marshal(s)
	if len(b) > 4096 || strings.Contains(string(b), "irrelevant") {
		t.Fatalf("unbounded summary: %d", len(b))
	}
}

func TestCompactBenchmarkSeparatesProgressErrorsReviewAndWarnings(t *testing.T) {
	for _, tc := range []struct {
		status, worker, want string
		review               bool
	}{
		{action.StatusWaitingExternal, "running", "testing", false},
		{action.StatusFailed, "failed", "execution_error", false},
		{action.StatusWaitingDecision, "", "search_review_required", true},
		{action.StatusCancelled, "", "cancelled", true},
	} {
		s := compactBenchmark(&action.ActionResult{ActionName: "benchmark_transcode", Status: tc.status, State: map[string]any{"optimization_enabled": true, "benchmark_status": tc.worker, "benchmark_search_review_required": tc.review}})
		if s == nil || s.Outcome != tc.want {
			t.Fatalf("%+v: %+v", tc, s)
		}
	}
	if s := compactBenchmark(&action.ActionResult{ActionName: "transcode_media"}); s != nil {
		t.Fatal("direct encoding falsely described as benchmark")
	}
	r := &action.ActionResult{ActionName: "benchmark_transcode", Status: action.StatusCompleted, State: map[string]any{
		"benchmark_decision": transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{{CandidateID: "c", EvaluationReason: "sample_below_minimum_quality"}}},
		"benchmark_request":  transcode.BenchmarkRequest{Quality: &transcode.BenchmarkQualityConfig{Banding: &transcode.BenchmarkBandingConfig{Enforcement: "observe"}}},
		"benchmark_quality":  []transcode.BenchmarkCandidateQualityEvidence{{CandidateID: "c", ReasonCodes: []string{"quality_cambi_mean_above_maximum"}}},
	}}
	s := compactBenchmark(r)
	if len(s.Rejected[0].Checks) != 1 || s.Rejected[0].Checks[0].Code != "sample_below_minimum_quality" {
		t.Fatalf("observe warning reported as rejection: %+v", s.Rejected)
	}
}

func TestHistoricalSelectionAndFailedSampleExplanation(t *testing.T) {
	tolerance := 0.5
	r := &action.ActionResult{ActionName: "benchmark_transcode", Status: action.StatusCompleted, State: map[string]any{
		"benchmark_request": transcode.BenchmarkRequest{Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{MarginalTolerance: &tolerance}}},
		"benchmark_decision": transcode.BenchmarkDecision{DecisionReason: "target_reached_smallest_size", Winner: &transcode.BenchmarkWinner{CandidateID: "crf20", MetricType: "vmaf"}, Evaluations: []transcode.BenchmarkCandidateEvaluation{
			{CandidateID: "crf20", Eligible: true, TargetReached: true, Score: 95.6818}, {CandidateID: "crf26", Eligible: true, TargetReached: true, Score: 92.5063},
			{CandidateID: "q65", EvaluationReason: "sample_below_minimum_quality", FailedSamples: []transcode.BenchmarkSampleFailure{{SampleIndex: 2, SourceSeconds: 1127, Metric: "vmaf", Actual: 94.7, Minimum: 95}}},
		}},
	}}
	s := compactBenchmark(r)
	if s.Selection == nil || s.Selection.BestScore != 95.6818 || len(s.Selection.OutsideMargin) != 1 || !strings.Contains(s.Message, "margin") {
		t.Fatalf("selection explanation: %+v", s)
	}
	if len(s.Rejected) != 1 || len(s.Rejected[0].FailedSamples) != 1 || s.Rejected[0].FailedSamples[0].SourceSeconds != 1127 {
		t.Fatal("missing sample evidence")
	}
	delete(r.State, "benchmark_request")
	if compactBenchmark(r).Selection != nil {
		t.Fatal("invented historical tolerance without the request")
	}
}
