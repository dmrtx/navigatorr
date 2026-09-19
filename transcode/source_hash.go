package transcode

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Source SHA-256 plumbing for the transcode I/O optimization.
//
// The coordinator already computes the original file's SHA-256 during preflight
// (before it ever submits a benchmark or a transcode). That digest is the
// strongest available immutable identity for the source bytes. It is forwarded
// to the worker so:
//   - the optimized benchmark and the full transcode can key the shared source
//     cache by content identity (path + size + mtime + SHA-256); and
//   - the worker can hash the LOCAL copied/downloaded bytes and refuse to
//     publish a cache entry whose content does not match the expected digest.
//
// The digest never adds another NAS read: only the coordinator's existing
// preflight read produces it, and the worker hashes only local bytes.

// NormalizeSourceSHA256 canonicalizes an optional expected source digest. An
// empty value is allowed for legacy callers and means "no content identity
// available"; callers that supply a value must provide a valid SHA-256.
func NormalizeSourceSHA256(raw string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	trimmed = strings.TrimPrefix(trimmed, "sha256:")
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) != 64 {
		return "", fmt.Errorf("source_sha256 must be 64 hex characters, got %d (fail closed)", len(trimmed))
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return "", fmt.Errorf("source_sha256 is not valid hex: %w (fail closed)", err)
	}
	return trimmed, nil
}

// ValidateSourceSHA256 accepts an empty value (legacy) or a valid SHA-256.
func ValidateSourceSHA256(raw string) error {
	_, err := NormalizeSourceSHA256(raw)
	return err
}

// SourceSHAMatches reports whether a computed hex digest matches the expected
// (normalized) digest. An empty expected digest matches anything.
func SourceSHAMatches(expected, actual string) bool {
	norm, err := NormalizeSourceSHA256(expected)
	if err != nil {
		return false
	}
	if norm == "" {
		return true
	}
	return strings.EqualFold(norm, strings.TrimSpace(actual))
}
