package transcode

// Strong transcode-submit idempotency (PR3): canonical execution-spec digest
// shared by HTTP and SSH executors and the worker so all transports generate
// the same value for the same immutable execution request.
//
// Digest contents (canonical):
//   - cleaned source path (filepath.Clean)
//   - cleaned candidate path (filepath.Clean)
//   - normalized profile (lowercase/trimmed, empty -> "hevc-vt")
//   - resolved plan digest (DigestPlan over the normalized plan) plus the
//     canonical plan JSON itself (so recipe/pixel-format/subtitle-action
//     changes alter the digest even if a stale plan_digest were echoed)
//
// Callers must NOT trust arbitrary caller-supplied digest text: the worker
// recomputes this value from the resolved plan and rejects mismatches.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// NormalizeTranscodeProfile returns the canonical profile name used in the
// execution-spec digest. Empty selects the legacy default.
func NormalizeTranscodeProfile(profile string) string {
	p := strings.ToLower(strings.TrimSpace(profile))
	if p == "" {
		return "hevc-vt"
	}
	return p
}

// ValidateTranscodeIdempotencyKey fail-closes on empty, oversized, or
// control-character keys. Keys are opaque (not filesystem paths) so the
// character set is intentionally permissive.
func ValidateTranscodeIdempotencyKey(key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return fmt.Errorf("idempotency_key is required")
	}
	if len(trimmed) > 256 {
		return fmt.Errorf("idempotency_key exceeds max length 256")
	}
	if strings.ContainsRune(trimmed, '\x00') || strings.ContainsAny(trimmed, "\n\r") {
		return fmt.Errorf("idempotency_key contains invalid control characters (fail closed)")
	}
	return nil
}

// DefaultTranscodeIdempotencyKey returns the effective key: explicit caller
// key wins, otherwise the (trimmed) job ID is used so legacy callers that
// only know job-<InstanceID> keep stable behavior and concurrent same-ID
// submissions still deduplicate.
func DefaultTranscodeIdempotencyKey(jobID, key string) string {
	if strings.TrimSpace(key) != "" {
		return strings.TrimSpace(key)
	}
	return strings.TrimSpace(jobID)
}

type canonicalExecutionSpec struct {
	Source     string `json:"source"`
	Candidate  string `json:"candidate"`
	Profile    string `json:"profile"`
	PlanDigest string `json:"plan_digest"`
	Plan       *Plan  `json:"plan,omitempty"`
}

// DigestTranscodeExecutionSpec computes the canonical digest. plan may be
// nil for legacy profile-only callers; the digest then covers the normalized
// profile with an empty plan digest (executors with a nil plan should
// preferably omit the digest and let the worker compute it over the resolved
// plan instead of sending a profile-only value).
//
// Canonicalization guarantee: only plan CONTENT influences the digest. A plan
// with an empty, valid, or stale/tampered PlanDigest hashes identically;
// only an actual content change alters the result.
func DigestTranscodeExecutionSpec(sourcePath, candidatePath, profile string, plan *Plan) (string, error) {
	cleanSource := filepath.Clean(strings.TrimSpace(sourcePath))
	cleanCandidate := filepath.Clean(strings.TrimSpace(candidatePath))
	if cleanSource == "" || cleanSource == "." {
		return "", fmt.Errorf("source path is required for execution spec digest")
	}
	if cleanCandidate == "" || cleanCandidate == "." {
		return "", fmt.Errorf("candidate path is required for execution spec digest")
	}
	normProfile := NormalizeTranscodeProfile(profile)
	var planDigest string
	var planCopy *Plan
	if plan != nil {
		// Canonicalize fully from content: DigestPlan already ignores
		// PlanDigest, but the embedded plan JSON must not vary with a
		// caller/stored stale digest either. Clear, hash, then stamp the
		// canonical digest so empty/valid/stale inputs hash identically.
		cp := *plan
		cp.PlanDigest = ""
		d, err := DigestPlan(&cp)
		if err != nil {
			return "", fmt.Errorf("digest plan for execution spec: %w", err)
		}
		planDigest = d
		cp.PlanDigest = d
		planCopy = &cp
	}
	canon := canonicalExecutionSpec{
		Source:     cleanSource,
		Candidate:  cleanCandidate,
		Profile:    normProfile,
		PlanDigest: planDigest,
		Plan:       planCopy,
	}
	b, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("serializing execution spec: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
