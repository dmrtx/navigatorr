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
