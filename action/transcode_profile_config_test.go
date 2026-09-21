package action

import (
	"strings"
	"testing"
)

func TestValidateTranscodeInputsRejectsIgnoredKnobs(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := &ExecutionContext{
		ActionName: "benchmark_transcode",
		Inputs: map[string]any{
			"path":           "/media/example.mkv",
			"profile":        "live-action-hevc",
			"tune":           "animation",
			"crf_candidates": []any{20.0, 22.0, 24.0},
		},
	}
	err := e.validateTranscodeInputs(ec)
	if err == nil || !strings.Contains(err.Error(), "unsupported input") || !strings.Contains(err.Error(), "profile_config") {
		t.Fatalf("expected explicit rejection of ignored knobs, got %v", err)
	}
}

func TestValidateTranscodeInputsAcceptsProfileConfig(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := &ExecutionContext{
		ActionName: "benchmark_transcode",
		Inputs: map[string]any{
			"path": "/media/example.mkv",
			"profile_config": map[string]any{
				"container": "mkv",
			},
		},
	}
	if err := e.validateTranscodeInputs(ec); err != nil {
		t.Fatalf("profile_config should be a declared input: %v", err)
	}
}
