package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func (e *Engine) stepTranscodeAccept(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if strings.EqualFold(ec.Decision, "reject") {
		return StepResult{Status: StepFailed, Error: "transcode candidate rejected by user decision; original file remains untouched"}, nil
	}
	origPath := getString(ec.State, "resolved_path")
	candidate := getString(ec.State, "candidate_path")
	if candidate == "" {
		candidate = getString(ec.State, "output_path")
	}
	origSHA := getString(ec.State, "original_sha256")
	if origPath == "" || origSHA == "" {
		return StepResult{Status: StepFailed, Error: "original integrity baseline missing (fail closed)"}, nil
	}
	f, err := os.Open(origPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: original media %s is no longer accessible: %v", origPath, err)}, nil
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: unable to hash original media %s: %v", origPath, err)}, nil
	}
	current := hex.EncodeToString(h.Sum(nil))
	if current != origSHA {
		ec.State["failure_classification"] = string(resilience.SourceChanged)
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: original file %s was modified (expected sha256 %s, got %s)", origPath, origSHA, current)}, nil
	}
	out := map[string]any{"candidate_path": candidate, "output_path": candidate, "original_path": origPath, "original_intact": true, "original_sha256": origSHA, "replace_original": false, "profile": getString(ec.State, "profile"), "recipe_version": getString(ec.State, "recipe_version"), "recipe_digest": getString(ec.State, "recipe_digest"), "plan_digest": getString(ec.State, "plan_digest"), "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count"), "applied_fallbacks": ec.State["applied_fallbacks"], "message": "Transcode completed and verified. Candidate output ready. Original file physically preserved and SHA-256 unchanged."}
	if c := ec.State["conversions"]; c != nil {
		out["conversions"] = c
	}
	if h := ec.State["failure_history"]; h != nil {
		out["failure_history"] = h
	}
	return StepResult{Status: StepCompleted, Outputs: out}, nil
}
