package transcodeworker

import (
	"errors"
	"fmt"
	"strings"
)

// This file defines the Phase 6B1 operational storage model: the staging and
// finalization state vocabularies plus small helpers used to derive safe
// restart behaviour. None of it participates in the transcode Plan or the
// execution-spec digest.

// StagingState is the durable operational staging state of a job.
type StagingState string

const (
	StagingStateNotRequired StagingState = "not_required"
	StagingStatePending     StagingState = "pending"
	StagingStateStaging     StagingState = "staging"
	StagingStateReady       StagingState = "ready"
)

// FinalizationState is the durable operational finalization state of a job.
type FinalizationState string

const (
	FinalizationStateNotRequired FinalizationState = "not_required"
	FinalizationStatePending     FinalizationState = "pending"
	FinalizationStateFinalizing  FinalizationState = "finalizing"
	FinalizationStateCompleted   FinalizationState = "completed"
)

var (
	// ErrInvalidStagingState indicates an unrecognized staging state value.
	ErrInvalidStagingState = errors.New("invalid staging state")
	// ErrInvalidFinalizationState indicates an unrecognized finalization state.
	ErrInvalidFinalizationState = errors.New("invalid finalization state")
)

// IsKnownStagingState reports whether raw is an explicit, recognized staging
// state. The blank value is deliberately not "known": it is the legacy
// unspecified value preserved for backward compatibility.
func IsKnownStagingState(raw string) bool {
	switch StagingState(strings.TrimSpace(raw)) {
	case StagingStateNotRequired, StagingStatePending, StagingStateStaging, StagingStateReady:
		return true
	default:
		return false
	}
}

// ValidateStagingState fails closed on an unrecognized staging state while
// preserving blank/zero as legacy/unspecified.
func ValidateStagingState(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if !IsKnownStagingState(raw) {
		return fmt.Errorf("%w: %q (want not_required|pending|staging|ready)", ErrInvalidStagingState, raw)
	}
	return nil
}

// IsKnownFinalizationState reports whether raw is an explicit, recognized
// finalization state. Blank is legacy/unspecified, not known.
func IsKnownFinalizationState(raw string) bool {
	switch FinalizationState(strings.TrimSpace(raw)) {
	case FinalizationStateNotRequired, FinalizationStatePending, FinalizationStateFinalizing, FinalizationStateCompleted:
		return true
	default:
		return false
	}
}

// ValidateFinalizationState fails closed on an unrecognized finalization state
// while preserving blank/zero as legacy/unspecified.
func ValidateFinalizationState(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if !IsKnownFinalizationState(raw) {
		return fmt.Errorf("%w: %q (want not_required|pending|finalizing|completed)", ErrInvalidFinalizationState, raw)
	}
	return nil
}

// PartialPathFor returns the exact finalization partial path for a destination
// and job id: "<destination>.partial.<jobID>". This mirrors the path used by
// FinalizeOutputAtomic.
func PartialPathFor(destination, jobID string) string {
	return destination + ".partial." + jobID
}

// IsPostEncodeFinalizationPending reports whether encoding has completed and
// the output still awaits finalization. A dead/missing runner on such a job
// must be restarted for finalization and must never be classified
// runner_killed.
func IsPostEncodeFinalizationPending(job *JobRecord) bool {
	if job == nil || !job.EncodeComplete {
		return false
	}
	switch FinalizationState(strings.TrimSpace(job.FinalizationState)) {
	case FinalizationStatePending, FinalizationStateFinalizing:
		return true
	default:
		return false
	}
}

// Destination returns the intended semantic destination, falling back to the
// legacy Candidate field for records persisted before IntendedDestination
// existed. It never mutates the record.
func (j *JobRecord) Destination() string {
	if j == nil {
		return ""
	}
	if strings.TrimSpace(j.IntendedDestination) != "" {
		return j.IntendedDestination
	}
	return j.Candidate
}

// EffectiveInput returns the operational encoder input path, falling back to
// the semantic Source for legacy records. It never mutates the record.
func (j *JobRecord) EffectiveInput() string {
	if j == nil {
		return ""
	}
	if strings.TrimSpace(j.EffectiveInputPath) != "" {
		return j.EffectiveInputPath
	}
	return j.Source
}

// LocalCandidate returns the local candidate path, falling back to the semantic
// Candidate for legacy records and local-only jobs. It never mutates the
// record.
func (j *JobRecord) LocalCandidate() string {
	if j == nil {
		return ""
	}
	if strings.TrimSpace(j.LocalCandidatePath) != "" {
		return j.LocalCandidatePath
	}
	return j.Candidate
}
