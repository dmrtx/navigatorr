package resilience

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyAndRetryPolicy(t *testing.T) {
	cases := map[string]FailureClass{
		"worker busy: maximum parallel jobs reached": WorkerBusy,
		"ssh: connection timed out":                  SSHTransient,
		"ssh: connection refused":                    WorkerUnreachable,
		"subtitle codec mov_text is not supported":   ContainerSubtitleIncompatible,
		"Invalid data found when processing input":   FFmpegInputCorrupt,
		"codec mismatch":                             ValidationCodecMismatch,
		"source sha changed":                         SourceChanged,
		"some new ffmpeg problem":                    Unclassified,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("Classify(%q)=%s want %s", msg, got, want)
		}
	}
	if !Retryable(WorkerBusy, []string{"worker_busy"}) {
		t.Fatal("worker_busy should be retryable when explicitly allowed")
	}
	if Retryable(FFmpegInputCorrupt, []string{"worker_busy", "ssh_transient"}) {
		t.Fatal("deterministic corruption must not retry")
	}
}

type typedFailure struct{ class string }

func (e typedFailure) Error() string        { return "typed failure" }
func (e typedFailure) FailureClass() string { return e.class }

// Text nothing recognizes names no cause. Calling it ffmpeg_unknown blamed
// ffmpeg for failures that never ran it.
func TestUnrecognizedTextIsUnclassifiedNotFFmpeg(t *testing.T) {
	for _, msg := range []string{"missing automatic verification sample", "", "something odd"} {
		if got := Classify(msg); got != Unclassified {
			t.Errorf("Classify(%q)=%s want %s", msg, got, Unclassified)
		}
	}
	if Unclassified == FFmpegUnknown {
		t.Fatal("unclassified and ffmpeg_unknown must stay distinct")
	}
}

// A class the producer declared wins over whatever the message text suggests.
func TestTypedClassWinsOverMessageText(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", typedFailure{class: string(BenchmarkEvidenceIncomplete)})
	if got := ClassifyError(err); got != BenchmarkEvidenceIncomplete {
		t.Fatalf("got %s", got)
	}
	conflicting := fmt.Errorf("invalid data found when processing input: %w", typedFailure{class: string(InternalInvariant)})
	if got := ClassifyError(conflicting); got != InternalInvariant {
		t.Fatalf("typed class must beat the text, got %s", got)
	}
	if got := ClassifyError(errors.New("no class here")); got != Unclassified {
		t.Fatalf("got %s", got)
	}
	// Serialized form survives a round trip through an error string.
	if got := Classify("benchmark_evidence_incomplete: window 3 missing"); got != BenchmarkEvidenceIncomplete {
		t.Fatalf("got %s", got)
	}
}

func TestNewClassesAreNeverAutoRetried(t *testing.T) {
	allowed := []string{"worker_busy", "ssh_transient", "worker_unreachable", "encoder_temporarily_unavailable", string(Unclassified), string(BenchmarkEvidenceIncomplete), string(InternalInvariant)}
	for _, class := range []FailureClass{Unclassified, BenchmarkEvidenceIncomplete, InternalInvariant} {
		if Retryable(class, allowed) {
			t.Errorf("%s must not be retryable even when listed", class)
		}
	}
}
