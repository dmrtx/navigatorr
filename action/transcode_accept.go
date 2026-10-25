package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func (e *Engine) stepTranscodeAccept(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if strings.EqualFold(ec.Decision, "reject") {
		return rejectedCandidateResult(), nil
	}
	origPath := getString(ec.State, "resolved_path")
	candidate := getString(ec.State, "candidate_path")
	if candidate == "" {
		candidate = getString(ec.State, "output_path")
	}
	origSHA := getString(ec.State, "original_sha256")
	if origPath != "" && skippedWithoutWork(ec) {
		return acceptUntouchedSkip(ec, origPath, origSHA), nil
	}
	if res, conflict := originalDigestConflict(ec); conflict {
		return res, nil
	}
	if _, _, byMetadata := recordedOriginalStat(ec.State); byMetadata {
		return e.acceptByMetadata(ec, origPath, origSHA, candidate), nil
	}
	if origPath == "" || origSHA == "" {
		return StepResult{Status: StepFailed, Error: "original integrity baseline missing (fail closed)"}, nil
	}
	f, err := os.Open(origPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: original media %s is no longer accessible: %v", origPath, err)}, nil
	}
	var total int64
	if fi, statErr := f.Stat(); statErr == nil {
		total = fi.Size()
	}
	// A whole-file re-hash of a large original takes minutes after the worker
	// has finished. Persist it as coordinator work, queued and then measured,
	// so clients do not show the finished encode as the current state.
	var lastObservation time.Time
	var progressErr error
	observe := promotionHashProgress(func(_ string, read, size int64) error {
		if read != 0 && read != size && time.Since(lastObservation) < time.Second {
			return nil
		}
		lastObservation = time.Now()
		ec.State["work_progress"] = map[string]any{"phase": "verifying_original", "bytes_read": read, "total_bytes": size, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
		progressErr = e.persistExecutionState(ctx, ec)
		return progressErr
	})
	ec.State["work_progress"] = map[string]any{"phase": "waiting_for_hash_slot", "total_bytes": total, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if err := e.persistExecutionState(ctx, ec); err != nil {
		_ = f.Close()
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	h := sha256.New()
	_, err = e.hashMedia(ctx, h, &promotionContextReader{ctx: ctx, r: f, path: origPath, total: total, progress: observe})
	_ = f.Close()
	// The step's own result replaces this progress; never leave a finished or
	// abandoned hash looking current.
	delete(ec.State, "work_progress")
	if progressErr != nil {
		return StepResult{Status: StepFailed, Error: progressErr.Error()}, nil
	}
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: unable to hash original media %s: %v", origPath, err)}, nil
	}
	current := hex.EncodeToString(h.Sum(nil))
	if current != origSHA {
		ec.State["failure_classification"] = string(resilience.SourceChanged)
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: original file %s was modified (expected sha256 %s, got %s)", origPath, origSHA, current)}, nil
	}
	validation := markOriginalVerified(ec, "ok", "sha256")
	if getBool(ec.State, "skip_transcode") {
		return StepResult{Status: StepCompleted, Outputs: skipOutputs(ec, origPath, origSHA, "verified", validation)}, nil
	}
	return StepResult{Status: StepCompleted, Outputs: e.acceptOutputs(ec, origPath, origSHA, candidate, validation, "sha256", "Transcode completed and verified. Candidate output ready. Original file physically preserved and SHA-256 unchanged.")}, nil
}

// acceptOutputs is the result of an accepted candidate; check says how the
// original was verified and message says it in words.
func (e *Engine) acceptOutputs(ec *ExecutionContext, origPath, origSHA, candidate string, validation map[string]any, check, message string) map[string]any {
	out := map[string]any{"original_integrity": "verified", "original_integrity_check": check, "validation": validation, "candidate_path": candidate, "output_path": candidate, "original_path": origPath, "original_intact": true, "original_sha256": origSHA, "replace_original": false, "profile": getString(ec.State, "profile"), "recipe_version": getString(ec.State, "recipe_version"), "recipe_digest": getString(ec.State, "recipe_digest"), "plan_digest": getString(ec.State, "plan_digest"), "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count"), "applied_fallbacks": ec.State["applied_fallbacks"], "message": message}
	if c := ec.State["conversions"]; c != nil {
		out["conversions"] = c
	}
	if h := ec.State["failure_history"]; h != nil {
		out["failure_history"] = h
	}
	return out
}

// markOriginalVerified records a passed original integrity check in the
// validation report and the action's state, and says how it was established.
func markOriginalVerified(ec *ExecutionContext, shaStatus, check string) map[string]any {
	validation, _ := ec.State["validation"].(map[string]any)
	if validation == nil {
		validation = make(map[string]any)
	}
	delete(validation, "original_sha256_pending")
	validation["original_sha256"] = shaStatus
	validation["original_integrity"] = "verified"
	validation[originalIntegrityCheckKey] = check
	ec.State["validation"] = validation
	ec.Outputs["validation"] = validation
	ec.State["original_integrity"] = "verified"
	ec.Outputs["original_integrity"] = "verified"
	ec.State[originalIntegrityCheckKey] = check
	ec.Outputs[originalIntegrityCheckKey] = check
	return validation
}

// acceptByMetadata finishes an action preflighted without a coordinator hash.
// The original's content digest was taken once, by the worker, while it copied
// the file; the original's size and modification time are checked against the
// stat recorded at preflight, and no media is read. The result says so: the
// integrity is "verified" by metadata, not by a second hash.
func (e *Engine) acceptByMetadata(ec *ExecutionContext, origPath, origSHA, candidate string) StepResult {
	if origPath == "" {
		return StepResult{Status: StepFailed, Error: "original integrity baseline missing (fail closed)"}
	}
	// A skip that ran no worker job needs no digest; every other finish does,
	// because promotion verifies the original against it.
	if origSHA == "" && !getBool(ec.State, "skip_transcode") {
		return StepResult{Status: StepFailed, Error: "original integrity baseline missing: the worker never reported the source digest (fail closed)"}
	}
	if res := verifyOriginalMetadata(ec, origPath); res != nil {
		return *res
	}
	validation := markOriginalVerified(ec, "pinned_at_staging", "metadata")
	if getBool(ec.State, "skip_transcode") {
		return StepResult{Status: StepCompleted, Outputs: skipOutputs(ec, origPath, origSHA, "verified", validation)}
	}
	return StepResult{Status: StepCompleted, Outputs: e.acceptOutputs(ec, origPath, origSHA, candidate, validation, "metadata", "Transcode completed and verified. Candidate output ready. Original file preserved: its SHA-256 was taken once while the worker copied it, and its size and modification time are unchanged since it was inspected.")}
}

// workStateKeys are the state entries a worker job or its candidate leave
// behind. Any of them present means the original may have been read by a
// worker or a candidate may exist, so accept keeps the integrity check.
var workStateKeys = []string{"job_id", "external_reference", "transcode_job_id", "transcode_submitted", "candidate_path", "output_path"}

// skippedWithoutWork reports a skip decided before anything was submitted or
// produced, such as a source rejected by its probe. Nothing ran against the
// original, so there is nothing to re-verify; every other path keeps the hash.
func skippedWithoutWork(ec *ExecutionContext) bool {
	if !getBool(ec.State, "skip_transcode") || strings.EqualFold(ec.Decision, "reject") {
		return false
	}
	for _, key := range workStateKeys {
		if v, ok := ec.State[key]; ok && v != nil && v != "" && v != false {
			return false
		}
	}
	return true
}

// acceptUntouchedSkip finishes a skip that never touched the original. The
// integrity is reported as not applicable, never as verified: no hash was
// compared.
func acceptUntouchedSkip(ec *ExecutionContext, origPath, origSHA string) StepResult {
	delete(ec.State, "work_progress")
	ec.State["original_integrity"] = "not_applicable"
	ec.Outputs["original_integrity"] = "not_applicable"
	return StepResult{Status: StepCompleted, Outputs: skipOutputs(ec, origPath, origSHA, "not_applicable", nil)}
}

func skipOutputs(ec *ExecutionContext, origPath, origSHA, integrity string, validation map[string]any) map[string]any {
	decision := getString(ec.State, "auto_decision")
	out := map[string]any{
		"skipped":            true,
		"auto_decision":      decision,
		"auto_reasons":       ec.State["auto_reasons"],
		"original_path":      origPath,
		"original_intact":    true,
		"original_integrity": integrity,
		"message":            fmt.Sprintf("Transcode not performed: auto decision is %s. Original file preserved and untouched.", decision),
	}
	if origSHA != "" {
		out["original_sha256"] = origSHA
	}
	if validation != nil {
		out["validation"] = validation
	}
	if mode := getString(ec.Inputs, "mode"); mode != "" {
		out["mode"] = mode
		out["policy_digest"] = getString(ec.State, "policy_digest")
		out["reason_code"] = getString(ec.State, "reason_code")
		out["message"] = "Original preserved: " + getString(ec.State, "reason_code")
	}
	if p := getString(ec.State, "auto_profile"); p != "" {
		out["profile"] = p
	}
	if s, ok := ec.State["expected_savings_percent"]; ok {
		out["expected_savings_percent"] = s
	}
	return out
}
