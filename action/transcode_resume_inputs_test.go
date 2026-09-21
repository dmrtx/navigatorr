package action

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestTranscodeActionsRejectResumeInputMutation(t *testing.T) {
	for _, actionName := range []string{"transcode_media", "benchmark_transcode", "transcode_batch"} {
		t.Run(actionName, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "resume-inputs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			engine := NewEngine(EngineDeps{Store: st})
			engine.RegisterTemplate(ActionTemplate{
				Name:            actionName,
				Version:         99,
				ImmutableInputs: true,
				Steps: []StepDefinition{{
					Name: "wait",
					Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
						if ec.Decision == "" {
							return StepResult{
								Status:           StepWaitingExternal,
								WaitingCondition: "test_wait",
								WaitingReason:    "waiting for test resume",
							}, nil
						}
						return StepResult{Status: StepCompleted}, nil
					},
				}},
			})

			res, err := engine.Run(context.Background(), actionName, map[string]any{"path": "/immutable/source.mkv"})
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusWaitingExternal {
				t.Fatalf("expected waiting_external, got %s", res.Status)
			}

			before, err := st.GetActionInstance(res.ID)
			if err != nil || before == nil {
				t.Fatalf("reading action before resume mutation tests: inst=%v err=%v", before, err)
			}
			originalInputs := before.InputsJSON

			for _, tc := range []struct {
				name  string
				extra map[string]any
			}{
				{name: "unknown encoder knob", extra: map[string]any{"tune": "animation"}},
				{name: "size guardrail", extra: map[string]any{"max_size_increase_percent": 25.0}},
				{name: "expected codec", extra: map[string]any{"expected_video_codec": "h264"}},
				{name: "batch metric", extra: map[string]any{"metric": "ssim"}},
				{name: "batch min savings", extra: map[string]any{"min_savings_percent": 5.0}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if _, err := engine.Resume(context.Background(), res.ID, "", tc.extra); err == nil || !strings.Contains(err.Error(), "inputs are immutable after action creation") {
						t.Fatalf("expected immutable-input rejection, got %v", err)
					}
					after, err := st.GetActionInstance(res.ID)
					if err != nil || after == nil {
						t.Fatalf("reading action after rejected mutation: inst=%v err=%v", after, err)
					}
					if after.InputsJSON != originalInputs {
						t.Fatalf("resume mutated persisted inputs: before=%s after=%s", originalInputs, after.InputsJSON)
					}
				})
			}

			resumed, err := engine.Resume(context.Background(), res.ID, "continue", nil)
			if err != nil {
				t.Fatalf("decision-only resume should remain supported: %v", err)
			}
			if resumed.Status != StatusCompleted {
				t.Fatalf("decision-only resume should complete test action, got %s", resumed.Status)
			}
		})
	}
}

func TestBuiltinWorkflowImmutableInputPolicy(t *testing.T) {
	engine := NewEngine(EngineDeps{})
	for _, name := range []string{"transcode_media", "benchmark_transcode", "transcode_batch", "promote_transcode_candidate"} {
		tmpl, ok := engine.GetTemplate(name)
		if !ok {
			t.Fatalf("missing builtin template %q", name)
		}
		if !tmpl.ImmutableInputs {
			t.Fatalf("builtin template %q must declare ImmutableInputs=true", name)
		}
	}
}
