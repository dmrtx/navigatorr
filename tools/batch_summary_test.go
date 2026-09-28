package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestCompactBatchReportsLibraryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, status, want           string
		completed, failed, cancelled int
		dry, cancel                  bool
	}{
		{"ready", action.StatusCompleted, "candidates_ready", 6, 0, 0, false, false},
		{"partial", action.StatusCompleted, "partial", 2, 4, 0, false, false},
		{"failed", action.StatusCompleted, "failed", 0, 6, 0, false, false},
		{"cancelled", action.StatusCancelled, "cancelled", 0, 0, 6, false, true},
		{"historic_cancel", action.StatusCompleted, "cancelled", 0, 6, 0, false, true},
		{"cancel_unconfirmed", action.StatusFailed, "failed", 0, 0, 0, false, true},
		{"cancel_in_progress", action.StatusWaitingExternal, "cancelling", 0, 0, 0, false, true},
		{"preview", action.StatusCompleted, "preview", 0, 0, 0, true, false},
		{"waiting", action.StatusWaitingDecision, "needs_decision", 6, 0, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &action.ActionResult{ActionName: "transcode_batch", Status: tc.status,
				State: map[string]any{"cancel_requested": tc.cancel},
				Outputs: map[string]any{"series_title": "Example", "dry_run": tc.dry,
					"counts": map[string]int{"total": 6, "completed": tc.completed, "failed": tc.failed, "cancelled": tc.cancelled},
					"items":  strings.Repeat("large-items", 10000), "ephemeral_profile": strings.Repeat("large-profile", 10000)}}
			summary := toCompactSummary(res)
			if summary.Batch == nil || summary.Batch.Outcome != tc.want || summary.Batch.Total != 6 || summary.Batch.Completed != tc.completed {
				t.Fatalf("unexpected batch: %+v", summary.Batch)
			}
			b, err := json.Marshal(summary)
			if err != nil || len(b) > 2048 || strings.Contains(string(b), "large-") {
				t.Fatalf("summary not bounded: %d bytes %v", len(b), err)
			}
		})
	}
}

func TestContextEffectivePolicyDoesNotRepeatSeededDefault(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Deps{Store: st, Config: &config.Config{AllowDestructive: true}}
	if _, err := d.Store.SetPreference("global", "allow_destructive", "false", "default", 0); err != nil {
		t.Fatal(err)
	}
	s := server.NewMCPServer("test", "1")
	registerScanTools(s, d, nil)
	text := resultText(t, callTool(t, s, "get_context", map[string]any{}))
	var got struct {
		Preferences []struct {
			Key string `json:"key"`
		}
		Policy map[string]bool `json:"effective_policy"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Policy["allow_destructive"] || len(got.Preferences) != 0 {
		t.Fatalf("stale default present: %s", text)
	}
	if _, err := d.Store.SetPreference("global", "allow_destructive", "false", "user", 0); err != nil {
		t.Fatal(err)
	}
	text = resultText(t, callTool(t, s, "get_context", map[string]any{}))
	if err := json.Unmarshal([]byte(text), &got); err != nil || len(got.Preferences) != 1 {
		t.Fatalf("user preference lost: %s", text)
	}
}

func TestCompactBatchShowsSelectionWithoutInventingSampleEvidence(t *testing.T) {
	r := &action.ActionResult{ActionName: "transcode_batch", Status: action.StatusCompleted,
		State:   map[string]any{"batch_selection": action.BatchSelection{Profile: "anime-x265-calibrated", Priority: "balanced", Reason: "anime series"}},
		Outputs: map[string]any{"dry_run": true, "counts": map[string]int{"total": 3, "queued": 2, "skip": 1}}}
	s := toCompactSummary(r)
	if s.Batch.Selection == nil || s.Batch.Selection.Priority != "balanced" || s.Batch.SampleEvidence != nil || s.Batch.Outcome != "preview" {
		t.Fatalf("preview must explain selection without fabricated measurements: %+v", s.Batch)
	}
	r.Outputs["dry_run"] = false
	r.State["batch_evidence"] = action.BatchCalibrationEvidence{AcceptedSamples: 2, MinimumSampleVMAF: 92, EstimatedSavingsMin: 22, EstimatedSavingsMax: 35}
	r.State["shared_calibration_profile"] = strings.Repeat("private-recipe-details", 10000)
	s = toCompactSummary(r)
	b, err := json.Marshal(s)
	if err != nil || len(b) > 2048 || strings.Contains(string(b), "private-recipe") || s.Batch.SampleEvidence == nil || s.Batch.SampleEvidence.EstimatedSavingsMax != 35 {
		t.Fatalf("sample evidence missing or unbounded: %+v %v", s.Batch, err)
	}
}

func TestBatchGuidanceKeepsMonitoringDecisionsAndCandidatesSeparate(t *testing.T) {
	for _, tc := range []struct {
		status, outcome, hint string
		counts                map[string]int
	}{
		{action.StatusWaitingExternal, "in_progress", "action_status", map[string]int{"running": 1}},
		{action.StatusWaitingDecision, "needs_decision", "waiting_options", map[string]int{"completed": 1}},
		{action.StatusCompleted, "candidates_ready", "originals are unchanged", map[string]int{"completed": 1}},
		{action.StatusCompleted, "partial", "completed candidates may already exist", map[string]int{"completed": 1, "failed": 1}},
		{action.StatusCancelled, "cancelled", "", map[string]int{"cancelled": 1}},
	} {
		r := &action.ActionResult{ActionName: "transcode_batch", Status: tc.status, Outputs: map[string]any{"counts": tc.counts}}
		b := compactBatch(r)
		if b.Outcome != tc.outcome || !strings.Contains(b.NextStep, tc.hint) {
			t.Fatalf("misleading guidance: %+v", b)
		}
		if tc.status == action.StatusCancelled && b.NextStep != "" {
			t.Fatal("terminal cancellation should not suggest more work")
		}
	}
}
