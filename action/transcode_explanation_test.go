package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestExplainTranscodeCompletedOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, reason, outcome string
		skipped, intact       bool
	}{
		{"quality rejected", "quality_not_met", "quality_rejected", true, true},
		{"preservation rejected", "preservation_not_met", "preservation_rejected", true, true},
		{"insufficient savings", "insufficient_savings", "size_rejected", true, true},
		{"growth", "size_growth", "size_rejected", true, true},
		{"legacy growth", "size_growth_exceeded", "size_rejected", true, true},
		{"no winner", "no_suitable_candidate", "no_suitable_candidate", true, true},
		{"already encoded", "already_target_codec", "no_changes", true, true},
		{"unknown skip", "future_reason", "no_changes", true, true},
		{"accepted", "", "candidate_ready", false, true},
		{"missing acceptance evidence", "", "completed", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Historical records can retain only output fields. Neither a skip
			// nor a terminal action may be called processing or candidate-ready.
			res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted,
				Inputs:  map[string]any{"mode": "size"},
				Outputs: map[string]any{"reason_code": tc.reason, "skipped": tc.skipped, "original_intact": tc.intact}}
			for _, stateOnly := range []bool{false, true} {
				if stateOnly {
					res.State = map[string]any{"reason_code": tc.reason, "skip_transcode": tc.skipped}
					delete(res.Outputs, "reason_code")
					delete(res.Outputs, "skipped")
				}
				explanation := ExplainTranscode(res)
				if explanation == nil || explanation.Outcome != tc.outcome || strings.Contains(explanation.Message, "Automatic testing") {
					t.Fatalf("stateOnly=%v: %+v", stateOnly, explanation)
				}
			}
		})
	}
}

func TestExplainTranscodeCurrentStatusPrecedesEarlierReason(t *testing.T) {
	for status, outcome := range map[string]string{
		StatusCancelled: "cancelled", StatusFailed: "execution_error", StatusWaitingDecision: "attention",
		StatusPending: "processing", StatusRunning: "processing", StatusWaitingExternal: "processing",
	} {
		t.Run(status, func(t *testing.T) {
			res := &ActionResult{ActionName: "transcode_media", Status: status, WaitingReason: "Review required",
				Inputs: map[string]any{"mode": "size"}, State: map[string]any{"reason_code": "no_suitable_candidate"}}
			got := ExplainTranscode(res)
			if got.Outcome != outcome || status == StatusWaitingDecision && got.Message != res.WaitingReason {
				t.Fatalf("stale reason masked current status: %+v", got)
			}
		})
	}
}

func TestTranscodePolicyRejectionPersistsTerminalWorkerObservation(t *testing.T) {
	for _, reason := range []string{"quality_not_met", "preservation_not_met", "insufficient_savings", "size_growth", "size_growth_exceeded", "already_target_codec"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Now().UTC()
			mock := &mockTranscodeExecutor{statusFunc: func(context.Context, string) (transcode.JobStatus, error) {
				return transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reason,
					JobTelemetry: transcode.JobTelemetry{Phase: "failed", FinishedAt: now}}, nil
			}}
			engine := NewEngine(EngineDeps{Transcode: mock})
			ec := &ExecutionContext{Inputs: map[string]any{"mode": "size"},
				State:   map[string]any{"job_id": "job", "transcode_status": "running"},
				Outputs: map[string]any{"transcode_status": "running", "transcode_phase": "validating_quality", "progress_is_stale": true}}
			result, err := engine.stepTranscodeWait(context.Background(), ec)
			if err != nil || result.Status != StepCompleted || !getBool(ec.State, "skip_transcode") {
				t.Fatalf("rejection: %+v, %v", result, err)
			}
			mergeMap(ec.Outputs, result.Outputs)
			// Match the persisted JSON read after a restart.
			data, err := json.Marshal(ec.Outputs)
			if err != nil {
				t.Fatal(err)
			}
			var outputs map[string]any
			if err := json.Unmarshal(data, &outputs); err != nil {
				t.Fatal(err)
			}
			if outputs["transcode_status"] != "failed" || outputs["transcode_phase"] != "failed" || outputs["progress_is_stale"] != false || outputs["worker_completed_at"] == nil || outputs["skipped"] != true {
				t.Fatalf("stale terminal telemetry: %s", data)
			}
			if outputs["reason_code"] != ec.State["reason_code"] {
				t.Fatalf("inconsistent rejection reason: %s", data)
			}
		})
	}
}

// A hardware winner is reported as such before anything is replaced; an x265
// winner and a search without a winner say nothing extra.
func TestExplainTranscodeNamesTheWinningEncoder(t *testing.T) {
	for _, tc := range []struct {
		name, codec, encoder string
		hardware             bool
	}{
		{"media engine", "hevc_videotoolbox", "hevc_videotoolbox", true},
		{"software", "libx265", "libx265", false},
		{"no winner", "", "", false},
	} {
		decision := map[string]any{"decision_reason": "no_valid_candidate"}
		if tc.codec != "" {
			decision = map[string]any{"decision_reason": "measured_plan_selected", "winner": map[string]any{"candidate_id": "c", "video_codec": tc.codec}}
		}
		res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted,
			Inputs:  map[string]any{"mode": "size"},
			Outputs: map[string]any{"original_intact": true, "benchmark_decision": decision}}
		e := ExplainTranscode(res)
		if e == nil || e.Encoder != tc.encoder || e.Hardware != tc.hardware {
			t.Fatalf("%s: %+v", tc.name, e)
		}
		if got := strings.Contains(e.Message, "media engine"); got != tc.hardware || tc.hardware && e.Outcome != "candidate_ready" {
			t.Fatalf("%s: message %q", tc.name, e.Message)
		}
	}
}

// The explanation carries the verification attempts and puts the winner's
// sample scores next to its final scores, window by window.
func TestExplainTranscodeReportsVerificationAndWindows(t *testing.T) {
	res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted,
		Inputs: map[string]any{"mode": "size"},
		Outputs: map[string]any{
			"original_intact": true,
			"automatic_search": transcode.BenchmarkAutomaticEvidence{
				Verification: &transcode.BenchmarkVerificationEvidence{Attempts: []transcode.BenchmarkVerificationAttempt{
					{CandidateID: "a", Role: transcode.VerificationRoleSmallest, Windows: 5, Outcome: transcode.VerificationQualityFailed},
					{CandidateID: "b", Role: transcode.VerificationRoleSafest, Windows: 5, Outcome: transcode.VerificationPassed},
				}},
				WinnerWindows: []transcode.BenchmarkWindowScore{{StartSeconds: 600, DurationSeconds: 10, Role: transcode.WindowRoleDiscovery, VMAFMean: 93, VMAFP5: 90, VMAFMin: 88}},
			},
			"quality_evidence": transcode.FinalQualityEvidence{Verdict: "pass", Windows: []transcode.FinalWindowScore{
				{StartSeconds: 600, DurationSeconds: 10, Role: "grid", VMAFMean: 91, VMAFP5: 88, VMAFMin: 85},
				{StartSeconds: 1200, DurationSeconds: 10, Role: "fresh", VMAFMean: 89, VMAFP5: 86, VMAFMin: 83},
			}},
		}}
	e := ExplainTranscode(res)
	if e == nil || e.Verification == nil || len(e.Verification.Attempts) != 2 || len(e.Windows) != 2 {
		t.Fatalf("explanation %+v", e)
	}
	if w := e.Windows[0]; w.Search == nil || w.Final == nil || w.Search.VMAFMean != 93 || w.Final.VMAFMean != 91 {
		t.Fatalf("first window %+v", w)
	}
	if w := e.Windows[1]; w.Role != transcode.WindowRoleFresh || w.Search != nil || w.Final.VMAFMean != 89 {
		t.Fatalf("fresh window %+v", w)
	}
}

// A search that ran out of time says so, instead of reporting that every
// configuration failed.
func TestExplainTranscodeNamesTheSearchTimeLimit(t *testing.T) {
	res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted,
		Inputs:  map[string]any{"mode": "size"},
		State:   map[string]any{"reason_code": "no_suitable_candidate"},
		Outputs: map[string]any{"automatic_search": transcode.BenchmarkAutomaticEvidence{Verification: &transcode.BenchmarkVerificationEvidence{StoppedBy: transcode.VerificationStoppedTimeCap, Unverified: []string{"a", "b"}}}}}
	e := ExplainTranscode(res)
	if e == nil || e.Outcome != "no_suitable_candidate" || !strings.Contains(e.Message, "ran out of time") || !strings.Contains(e.Message, "2 never verified") {
		t.Fatalf("explanation %+v", e)
	}
	res.Outputs["automatic_search"] = transcode.BenchmarkAutomaticEvidence{}
	if e := ExplainTranscode(res); strings.Contains(e.Message, "out of time") {
		t.Fatalf("no time-limit claim without evidence: %q", e.Message)
	}
}

// An audio-only encode that landed over the budget is explained with the
// budget, the size it came out at and the gap, as the x265 encode's is.
func TestExplainTranscodeAudioOnlyOvershoot(t *testing.T) {
	plan := &transcode.Plan{SizePolicy: &transcode.SizePolicy{MaxCandidateBytes: 5_000_000_000}}
	for _, reason := range []string{SizeBudgetExceeded, "insufficient_savings"} {
		res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted,
			Inputs:  map[string]any{"mode": "size"},
			State:   map[string]any{"reason_code": reason, "skip_transcode": true, "plan_source": "audio_only", "plan": plan},
			Outputs: map[string]any{"original_intact": true, "candidate_size_bytes": int64(5_500_000_000)}}
		e := ExplainTranscode(res)
		if e == nil || e.Outcome != "size_rejected" {
			t.Fatalf("%s: %+v", reason, e)
		}
		for _, want := range []string{"5.50 GB", "5.00 GB budget", "500 MB", "10.0%", "Original preserved"} {
			if !strings.Contains(e.Message, want) {
				t.Fatalf("%s: message %q lacks %q", reason, e.Message, want)
			}
		}
	}
	// The worker may refuse the file without reporting its size.
	res := &ActionResult{ActionName: "transcode_media", Status: StatusCompleted, Inputs: map[string]any{"mode": "size"},
		State:   map[string]any{"reason_code": SizeBudgetExceeded, "skip_transcode": true, "plan_source": "audio_only", "plan": plan},
		Outputs: map[string]any{"original_intact": true}}
	if e := ExplainTranscode(res); e == nil || e.Outcome != "size_rejected" || !strings.Contains(e.Message, "5.00 GB budget") {
		t.Fatalf("no size: %+v", e)
	}
	// Any other plan is not described as an audio-only overshoot.
	res.State["plan_source"] = ""
	if e := ExplainTranscode(res); e == nil || strings.Contains(e.Message, "lossless audio") {
		t.Fatalf("other plan: %+v", e)
	}
}
