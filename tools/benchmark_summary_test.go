package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
)

// A goal run from before the sample search was removed explains how it ended
// from its stored state, with the audio it planned.
func TestCompactBenchmarkExplainsAStoredNoWinnerAndActualAudioPlan(t *testing.T) {
	res := &action.ActionResult{ActionName: "transcode_media", Status: action.StatusCompleted, Inputs: map[string]any{"mode": "compact"}, State: map[string]any{
		"reason_code": "no_suitable_candidate",
		"plan":        transcode.Plan{AudioMode: "compact"},
		"original":    map[string]any{"audio": []map[string]any{{"index": 1, "codec": "eac3", "channels": 6, "bit_rate": 640000}, {"index": 2, "codec": "dts", "channels": 2, "language": "jpn"}}, "irrelevant": strings.Repeat("x", 2000)},
	}}
	// Exercise the decoded persisted JSON shape, not only typed live values.
	data, _ := json.Marshal(res)
	var persisted action.ActionResult
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	s := toCompactSummary(&persisted).Benchmark
	if s == nil || s.Outcome != "no_suitable_candidate" {
		t.Fatalf("bad summary: %+v", s)
	}
	if len(s.Audio) != 2 || s.Audio[0].Operation != "copy" || s.Audio[0].OutputCodec != "eac3" || s.Audio[0].TargetKbps != 0 || s.Audio[1].TargetKbps != 192 {
		t.Fatalf("incorrect audio conversion claim: %+v", s.Audio)
	}
	if !strings.Contains(s.NextStep, "Keep the original") || !strings.Contains(s.AudioNote, "Planned audio only") {
		t.Fatalf("missing next step or evidence limits: %+v", s)
	}
	b, _ := json.Marshal(s)
	if len(b) > 4096 || strings.Contains(string(b), "irrelevant") {
		t.Fatalf("unbounded summary: %d", len(b))
	}
}

// Sample tests have no template any more; the ones in history have no summary.
func TestCompactBenchmarkIsAbsentForSampleTests(t *testing.T) {
	res := &action.ActionResult{ActionName: "benchmark_transcode", Status: action.StatusCompleted, Inputs: map[string]any{"path": "/media/Show/S01E01.mkv"},
		State: map[string]any{"optimization_enabled": true, "benchmark_decision": map[string]any{"decision_reason": "all_candidates_invalid"}}}
	if s := compactBenchmark(res); s != nil {
		t.Fatalf("a sample test was given an encode summary: %+v", s)
	}
	if s := toCompactSummary(res).Benchmark; s != nil {
		t.Fatalf("a sample test was given an encode summary: %+v", s)
	}
}

func TestCompactBenchmarkExplainsUnavailableQualityPolicyInsteadOfEncoding(t *testing.T) {
	res := &action.ActionResult{ActionName: "transcode_media", Status: action.StatusWaitingDecision, WaitingReason: "quality_vmaf_model_unavailable: v1_1080p_3h; original preserved",
		Inputs: map[string]any{"mode": "compact"},
		State:  map[string]any{"reason_code": "quality_policy_unavailable", "plan_source": "profile_base"}}
	s := compactBenchmark(res)
	if s == nil || s.Outcome != "quality_policy_unavailable" || strings.Contains(s.Message, "Encoding") || !strings.Contains(s.NextStep, "Cancel") {
		t.Fatalf("paused workflow reported as encoding: %+v", s)
	}
	if s.PlanSource != "" || s.PlanNote != "" {
		t.Fatalf("provisional preflight plan reported as settled: %+v", s)
	}
	res.State["reason_code"] = "some_other_reason"
	if s = compactBenchmark(res); s.Outcome != "attention" || s.Message != res.WaitingReason {
		t.Fatalf("other attention reasons reported as encoding: %+v", s)
	}
}

func TestCompactBenchmarkReportsSettledPlanSource(t *testing.T) {
	note := "The profile's fixed settings."
	res := &action.ActionResult{ActionName: "transcode_media", Status: action.StatusCompleted, Inputs: map[string]any{"mode": "size"},
		Outputs: map[string]any{"plan_source": "profile_base", "plan_note": note},
		State:   map[string]any{"plan_source": "profile_base"}}
	if s := compactBenchmark(res); s.PlanSource != "profile_base" || s.PlanNote != note {
		t.Fatalf("plan not explained: %+v", s)
	}
	res = &action.ActionResult{ActionName: "transcode_media", Status: action.StatusRunning, Inputs: map[string]any{"mode": "size"},
		State: map[string]any{"plan_source": "profile_base"}}
	if s := compactBenchmark(res); s.PlanSource != "" || s.PlanNote != "" {
		t.Fatalf("a plan with no note was presented as settled: %+v", s)
	}
}

func TestCompactBenchmarkExplainsAudioOnlyFallback(t *testing.T) {
	res := &action.ActionResult{ActionName: "transcode_media", Status: action.StatusRunning, Inputs: map[string]any{"mode": "size"}, State: map[string]any{
		"plan_source": "audio_only",
		"plan":        transcode.Plan{Mode: "size", VideoCodec: transcode.VideoCodecCopy, AudioMode: transcode.AudioModeCompactLossless},
	}}
	data, _ := json.Marshal(res)
	var persisted action.ActionResult
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	s := toCompactSummary(&persisted).Benchmark
	if s == nil || s.Outcome != "audio_only" || !strings.Contains(s.Message, "copied unchanged") || !strings.Contains(s.NextStep, "bit-identical") {
		t.Fatalf("audio-only fallback summary: %+v", s)
	}
}

func TestCompactBenchmarkReportsTheSizeBudgetEncode(t *testing.T) {
	budget := &action.SizeBudgetRecord{Trigger: "budget_first", SourceBytes: 9000, BudgetBytes: 3000, TargetBytes: 2910, VideoBitrateKbps: 2400, Message: "Encoded once at the budget."}
	res := &action.ActionResult{ActionName: "transcode_media", Status: action.StatusRunning,
		Inputs:  map[string]any{"mode": "size"},
		Outputs: map[string]any{"plan_source": "size_budget", "plan_note": budget.Message, "size_budget": budget},
		State:   map[string]any{"plan_source": "size_budget"}}
	s := compactBenchmark(res)
	if s == nil || s.Outcome != "size_budget" || s.Message != budget.Message || s.PlanSource != "size_budget" || s.SizeBudget == nil || s.SizeBudget.TargetBytes != 2910 {
		t.Fatalf("size budget encode not reported: %+v", s)
	}
}

// A VideoToolbox ladder run says which full encode is running.
func TestCompactBenchmarkDescribesTheSizeBudgetLadder(t *testing.T) {
	ladder := func(rung int, plan *transcode.Plan, status string) *action.ActionResult {
		rec := &action.SizeBudgetRecord{Trigger: "budget_first", BudgetBytes: 2_000_000_000, Rung: rung, Message: "Encoding with VideoToolbox at quality 65 (step 1 of 4) to fit the 2.0 GB budget.",
			Choice: &action.SizeBudgetChoice{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Reason: action.SizeBudgetChoiceLadder}}
		state := map[string]any{"plan_source": "size_budget", "plan": plan, "size_budget": rec,
			"mode_policy": map[string]any{"mode": "size", "size": map[string]any{"size_budget": map[string]any{"vt_quality_ladder": []int{65, 58, 50}}}}, "transcode_status": "running"}
		return &action.ActionResult{ActionName: "transcode_media", Status: status, Inputs: map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size"}, State: state, Outputs: state}
	}
	s := compactBenchmark(ladder(0, &transcode.Plan{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 65}, action.StatusWaitingExternal))
	if s == nil || s.Outcome != "size_budget_ladder" || !strings.HasPrefix(s.Message, "Encoding with VideoToolbox at quality 65") || s.SizeBudget == nil {
		t.Fatalf("%+v", s)
	}
	if s.ProgressScope != "Worker progress is for the full encode with VideoToolbox at quality 65 (step 1 of 4)." {
		t.Fatalf("progress scope %q", s.ProgressScope)
	}
	if !strings.Contains(s.UserNextStep, "VideoToolbox at quality 65 (step 1 of 4)") {
		t.Fatalf("user next step %q", s.UserNextStep)
	}
	// The last step encodes at the budget's average bitrate.
	s = compactBenchmark(ladder(3, &transcode.Plan{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, AverageBitrateKbps: 4100}, action.StatusWaitingExternal))
	if !strings.Contains(s.ProgressScope, "an average bitrate of 4100 kbps (step 4 of 4)") {
		t.Fatalf("progress scope %q", s.ProgressScope)
	}
	if s = compactBenchmark(ladder(0, &transcode.Plan{Quality: 65}, action.StatusFailed)); s.Outcome != "execution_error" {
		t.Fatalf("failed ladder %+v", s)
	}
}
