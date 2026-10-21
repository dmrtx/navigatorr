package action

// Focused size-guardrail tests for transcode_media.
//
// A direct transcode_media call may omit min_savings_percent and
// max_size_increase_percent; the effective production defaults (15% minimum
// savings, 0% allowed growth) then apply to the finished encode.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// A fixed profile's real encode can end up larger than the original. With
// max_size_increase_percent omitted, validate_result must stop in
// waiting_decision using the effective default (0%).
func TestTranscodeMedia_PostTranscodeGuardrailAppliesDefaultMaxIncrease(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			// Source fixture is tens of bytes; write a much larger candidate
			// so the real result exceeds the original.
			if err := os.WriteFile(req.CandidatePath, make([]byte, 64*1024), 0644); err != nil {
				t.Errorf("writing oversized candidate: %v", err)
			}
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}
	engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "hevc-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatalf("expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected StatusWaitingDecision for oversized candidate under default 0%% guardrail, got %s (error: %s)", res.Status, res.Error)
	}
	if !strings.Contains(res.WaitingReason, "max_size_increase_percent") {
		t.Errorf("expected waiting reason to mention max_size_increase_percent, got %q", res.WaitingReason)
	}
}

// E (unit). Effective guardrail resolution: omitted inputs fall back to the
// established production behavior (15% minimum savings, 0% allowed growth);
// explicit caller values override; configured policy wins over the fallback.
func TestResolveTranscodeSizeGuardrails_DefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name      string
		inputs    map[string]any
		configMin float64
		wantMin   float64
		wantMax   float64
	}{
		{"omitted guardrails default to 15/0", map[string]any{}, 0, 15, 0},
		{"nil inputs default to 15/0", nil, 0, 15, 0},
		{"configured minimum wins over fallback", map[string]any{}, 20, 20, 0},
		{"explicit minimum overrides config", map[string]any{"min_savings_percent": 5.0}, 20, 5, 0},
		{"explicit zeros mean disabled minimum and zero allowed growth", map[string]any{"min_savings_percent": 0.0, "max_size_increase_percent": 0.0}, 15, 0, 0},
		{"explicit growth allowance respected", map[string]any{"max_size_increase_percent": 10.0}, 0, 15, 10},
		{"explicit negative minimum respected", map[string]any{"min_savings_percent": -30.0}, 0, -30, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMin, gotMax := resolveTranscodeSizeGuardrails(tc.inputs, tc.configMin)
			if gotMin != tc.wantMin || gotMax != tc.wantMax {
				t.Errorf("resolveTranscodeSizeGuardrails = (%v, %v), want (%v, %v)", gotMin, gotMax, tc.wantMin, tc.wantMax)
			}
		})
	}
}
