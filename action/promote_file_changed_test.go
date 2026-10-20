package action

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// The promotion hasher serves the original, the candidate and the recovery
// copies, so a file that changed under it says nothing about the source. It is
// recognised by type for the stable-read retry, and carries no failure class
// whatever its path says: a path with "source" and "sha" in it must not turn a
// changed candidate into source_changed.
func TestPromotionFileChangedIsTypedAndRoleNeutral(t *testing.T) {
	for _, phase := range []string{"opening", "hashing"} {
		err := fmt.Errorf("verifying: %w", &promotionFileChangedError{path: "/media/source/sha/shared_clip.mkv", phase: phase})
		if !transientPromotionHashError(err) {
			t.Errorf("%s: not recognised as a transient hash error", phase)
		}
		if got := resilience.ClassifyError(err); got == resilience.SourceChanged || got == resilience.CandidateInvalid {
			t.Errorf("%s: the hasher alone must not pick a role, got %s", phase, got)
		}
	}
	if transientPromotionHashError(errors.New("file changed while hashing: /media/clip.mkv")) {
		t.Error("an untyped error with the same words must not match")
	}
	if transientPromotionHashError(nil) {
		t.Error("nil matched")
	}
}
