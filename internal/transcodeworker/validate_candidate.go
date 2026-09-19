package transcodeworker

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
)

// This file implements worker-local full candidate validation. It runs on the
// worker-local candidate BEFORE any publish/copy to the NAS, so a bad candidate
// never reaches the NAS. After publish, only a lightweight independent
// verification (existence/size/regular-file identity) runs, plus the
// coordinator's own independent verification.
//
// The worker check is structural/media validation, not a duplicate of every
// coordinator policy nuance: it probes the local candidate with the same probe
// seam used for the source, requires decodable video, requires stream-count
// sanity against the source/execution plan, requires duration sanity, and
// requires a non-empty regular file. Any failure fails the job closed and the
// local candidate is removed without publishing. This preserves
// original-preservation, no-clobber, cancellation, recovery, idempotency, and
// promotion separation: validation never mutates the source, the destination,
// or another job's artifacts.

// validateEncodedCandidateFull performs full structural/media validation of the
// worker-local candidate before the EncodeComplete checkpoint. execPlan and
// sourceStreams describe the winning encode; candidateDur is the probed
// candidate duration (0 when unknown). It fails closed on any probe failure,
// missing video, audio-count mismatch, or duration divergence.
func (w *Worker) validateEncodedCandidateFull(ctx context.Context, localCandidate string, execPlan *ExecutionPlan, sourceStreams []SourceStream, sourceDur, candidateDurHint float64) error {
	if err := validateEncodedCandidate(localCandidate); err != nil {
		return err
	}
	if execPlan == nil || execPlan.Plan == nil {
		return fmt.Errorf("%w: nil execution plan for candidate validation (fail closed)", ErrSourceInvalid)
	}
	candStreams, candDur, err := w.probeSourceForJob(ctx, localCandidate)
	if err != nil {
		return fmt.Errorf("probing local candidate for pre-publish validation: %w", err)
	}
	if candidateDurHint > 0 {
		candDur = candidateDurHint
	}
	var srcVideo, candVideo, srcAudio, candAudio int
	for _, s := range sourceStreams {
		switch strings.ToLower(strings.TrimSpace(s.Kind)) {
		case "video":
			srcVideo++
		case "audio":
			srcAudio++
		}
	}
	for _, s := range candStreams {
		switch strings.ToLower(strings.TrimSpace(s.Kind)) {
		case "video":
			candVideo++
			if strings.TrimSpace(s.Codec) == "" {
				return fmt.Errorf("%w: local candidate video stream has empty codec (fail closed)", ErrSourceInvalid)
			}
		case "audio":
			candAudio++
		}
	}
	if candVideo == 0 {
		return fmt.Errorf("%w: local candidate contains no video streams (fail closed, never publish)", ErrSourceInvalid)
	}
	// Video count must be preserved (exactly one video stream in practice).
	if srcVideo > 0 && candVideo != srcVideo {
		return fmt.Errorf("%w: local candidate video count %d != source %d (fail closed, never publish)", ErrSourceInvalid, candVideo, srcVideo)
	}
	// Audio count must be preserved; codec/channel specifics remain the
	// coordinator's full post-publish policy check (defense in depth).
	if candAudio != srcAudio {
		return fmt.Errorf("%w: local candidate audio count %d != source %d (fail closed, never publish)", ErrSourceInvalid, candAudio, srcAudio)
	}
	// Duration sanity mirrors the coordinator tolerance (3s absolute and 2%
	// relative). A diverged duration is never published.
	if sourceDur > 0 && candDur > 0 {
		d := math.Abs(candDur - sourceDur)
		if d > 3 && (d/sourceDur) > 0.02 {
			return fmt.Errorf("%w: local candidate duration %.1fs diverges from source %.1fs (diff %.1fs, fail closed, never publish)", ErrSourceInvalid, candDur, sourceDur, d)
		}
	}
	return nil
}

// verifyPublishedCandidate performs the lightweight independent post-publish
// verification: the destination must exist as a non-empty regular file whose
// size exactly matches the accepted local candidate. It performs only stats,
// never full content reads or full media probes, so it stays cheap after the
// network copy. Any mismatch fails closed without marking the job complete.
func verifyPublishedCandidate(ctx context.Context, localCandidate, destination string) error {
	if strings.TrimSpace(localCandidate) == "" || strings.TrimSpace(destination) == "" {
		return fmt.Errorf("%w: empty candidate/destination for post-publish verification", ErrDestinationInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	localInfo, err := os.Lstat(localCandidate)
	if err != nil {
		return fmt.Errorf("%w: statting local candidate %s: %v", ErrStorageIO, localCandidate, err)
	}
	if !localInfo.Mode().IsRegular() || localInfo.Size() == 0 {
		return fmt.Errorf("%w: local candidate %s is not a non-empty regular file", ErrSourceInvalid, localCandidate)
	}
	destInfo, err := os.Lstat(destination)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: published destination %s is missing after publish (fail closed)", ErrStorageIO, destination)
		}
		return fmt.Errorf("%w: statting published destination %s: %v", ErrStorageIO, destination, err)
	}
	if !destInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: published destination %s is not a regular file (fail closed)", ErrStorageIO, destination)
	}
	if destInfo.Size() == 0 {
		return fmt.Errorf("%w: published destination %s is empty (fail closed)", ErrStorageIO, destination)
	}
	if destInfo.Size() != localInfo.Size() {
		return fmt.Errorf("%w: published destination size %d != accepted candidate size %d (fail closed)", ErrSizeMismatch, destInfo.Size(), localInfo.Size())
	}
	return nil
}
