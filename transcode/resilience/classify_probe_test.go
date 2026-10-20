package resilience

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// A probe failure is classified by its kind. Its message holds the file path
// and ffprobe's stderr, and a path such as ".../source/shared_clip.mkv" holds
// the words "source" and "sha" without the source having changed. A false
// source_changed blocks a manual retry that would have worked.
func TestProbeErrorIsClassifiedByKindNotByItsMessage(t *testing.T) {
	if mediainspect.ProbeFailureClass != string(MediaProbeFailed) {
		t.Fatalf("mediainspect names the class %q, resilience %q", mediainspect.ProbeFailureClass, MediaProbeFailed)
	}
	paths := []string{
		"/media/source/shared_clip.mkv",
		"/media/shared/source_clip.mkv",
		"/media/sha/source.mkv",
	}
	for _, kind := range []mediainspect.ProbeKind{mediainspect.ProbeTimeout, mediainspect.ProbeUnavailable, mediainspect.ProbeToolExit, mediainspect.ProbeBadOutput} {
		for _, path := range paths {
			probe := &mediainspect.ProbeError{Kind: kind, Err: errors.New("exit status 1"), Stderr: path + ": End of file", Elapsed: time.Second}
			wrapped := fmt.Errorf("inspecting source media %s: %w", path, probe)
			if got := ClassifyError(wrapped); got != MediaProbeFailed {
				t.Errorf("%s probe of %s: class %s, want %s", kind, path, got, MediaProbeFailed)
			}
			if Retryable(ClassifyError(wrapped), []string{string(MediaProbeFailed)}) {
				t.Errorf("a probe failure must not be retried automatically")
			}
		}
	}
}

// The text heuristic names the messages the code writes, not a pair of words.
func TestSourceChangedIsOnlyTheExplicitMessages(t *testing.T) {
	notChanged := []string{
		"inspecting source media /media/source/shared_clip.mkv: ffprobe tool_exit after 1s: exit status 1: /media/source/shared_clip.mkv: End of file",
		"/shared/source/clip.mkv: Invalid argument",
		"reading source sha1sum file failed",
		"source media /media/shows/sha/clip.mkv could not be probed (fail closed)",
		"ssh: source host sha256 fingerprint unknown",
	}
	for _, msg := range notChanged {
		if got := Classify(msg); got == SourceChanged {
			t.Errorf("Classify(%q) = source_changed", msg)
		}
		// Inside the SMB boundary the same words were also enough.
		if got := Classify("smb: " + msg); got == SourceChanged {
			t.Errorf("Classify(smb: %q) = source_changed", msg)
		}
	}
	changed := []string{
		"subtitle #2 source index changed: plan=3 actual=4 (fail closed)",
		"subtitle #2 source codec changed: plan=subrip actual=ass (fail closed)",
		"resolved plan references missing subtitle #1 (source changed; fail closed)",
		"source file /media/clip.mkv was concurrently modified (size 1->2, mtime a->b): failing closed",
		"source media modified during benchmark (integrity violation, fail closed): size changed",
		"original changed; output cannot be accepted for the feed",
		"source_changed: the file differs",
		// The worker's own detections, which used to rely on an accident of
		// wording.
		"sizemismatch: local source bytes do not match expected sha256 (fail closed, cache entry never published)",
	}
	for _, msg := range changed {
		if got := Classify(msg); got != SourceChanged {
			t.Errorf("Classify(%q) = %s, want source_changed", msg, got)
		}
	}
	if got := Classify("smb: SMB source changed while downloading: copied=1 stat=2"); got != SourceChanged {
		t.Errorf("SMB source change: %s", got)
	}
}

// candidate_invalid is a candidate that failed its own integrity check. It is
// serialized in the message, never retried automatically, and is not a claim
// about the source.
func TestCandidateInvalidClass(t *testing.T) {
	typed := &classedForTest{class: CandidateInvalid, msg: "transcode source file invalid: digest mismatch for /work/source/sha/shared_clip.mkv"}
	if got := ClassifyError(fmt.Errorf("validating: %w", typed)); got != CandidateInvalid {
		t.Errorf("typed candidate failure class %s", got)
	}
	if got := Classify("candidate_invalid: the encoder output is empty"); got != CandidateInvalid {
		t.Errorf("serialized prefix class %s", got)
	}
	if got := Classify("transcode source file invalid: local candidate no longer matches its accepted sha256"); got == SourceChanged {
		t.Errorf("a candidate failure was read as source_changed")
	}
	if Retryable(CandidateInvalid, []string{"candidate_invalid"}) {
		t.Error("candidate_invalid must never be retried automatically, even when listed")
	}
}

type classedForTest struct {
	class FailureClass
	msg   string
}

func (e *classedForTest) Error() string        { return e.msg }
func (e *classedForTest) FailureClass() string { return string(e.class) }
