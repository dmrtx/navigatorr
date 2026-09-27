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
