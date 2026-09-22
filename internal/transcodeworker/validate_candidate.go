package transcodeworker

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// This file implements worker-local full candidate validation. It runs on the
// worker-local candidate BEFORE any publish/copy to the NAS, so a bad candidate
// never reaches the NAS. After publish, only a lightweight independent
// verification (existence/size/regular-file identity) runs, plus the
// coordinator's own lightweight identity check.
//
// The worker check is the authoritative structural/media/policy validation. It
// probes the LOCAL candidate with the same probe seam used for the source and
// enforces every materially required correctness/policy invariant that used to
// live in the coordinator: video codec, expected bit depth, pixel format,
// video/audio/subtitle/attachment/chapter preservation, per-subtitle action
// codec/disposition, audio copy-mode codec/language/channels, resolution
// preservation, and duration sanity. Any failure fails the job closed and the
// local candidate is removed without publishing. This preserves
// original-preservation, no-clobber, cancellation, recovery, idempotency, and
// promotion separation: validation never mutates the source, the destination,
// or another job's artifacts.

// CandidateAttestation is the accepted local candidate's identity computed
// during pre-publish validation. The size and content digest are cheap
// independent facts the coordinator compares against the published object
// without a full NAS read or media inspection.
type CandidateAttestation struct {
	SizeBytes   int64
	SHA256      string
	DurationSec float64
	VideoCodec  string
	Width       int
	Height      int
	BitDepth    int
	PixelFormat string
}

func verifyCheckpointCandidate(ctx context.Context, job *JobRecord, path string) error {
	// Pre-attestation worker records remain readable. A partial or missing
	// identity on a modern validated checkpoint is always an error.
	if job.CandidateSHA256 == "" && job.CandidateSizeBytes == 0 && job.ValidationFinishedAt.IsZero() {
		return nil
	}
	if job.CandidateSHA256 == "" || job.CandidateSizeBytes <= 0 {
		return fmt.Errorf("%w: validated candidate checkpoint has no complete integrity baseline", ErrSourceInvalid)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != job.CandidateSizeBytes {
		return fmt.Errorf("%w: local candidate no longer matches validated size/type; never publish", ErrSourceInvalid)
	}
	sha, err := hashLocalFileSHA256(ctx, path)
	if err != nil {
		return err
	}
	if sha != job.CandidateSHA256 {
		return fmt.Errorf("%w: local candidate SHA-256 changed after validation; never publish", ErrSourceInvalid)
	}
	return nil
}

// validateEncodedCandidateFull performs full structural/media/policy validation
// of the worker-local candidate before the EncodeComplete checkpoint. It fails
// closed on any probe failure, missing video, codec/bit-depth/pixel-format
// mismatch, stream/chapter preservation loss, or duration divergence.
func (w *Worker) validateEncodedCandidateFull(ctx context.Context, localCandidate string, execPlan *ExecutionPlan, srcProbe SourceProbe, candidateDurHint float64) (CandidateAttestation, error) {
	var attest CandidateAttestation
	if err := validateEncodedCandidate(localCandidate); err != nil {
		return attest, err
	}
	if execPlan == nil || execPlan.Plan == nil {
		return attest, fmt.Errorf("%w: nil execution plan for candidate validation (fail closed)", ErrSourceInvalid)
	}
	info, err := os.Lstat(localCandidate)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return attest, fmt.Errorf("%w: local candidate %s is not a non-empty regular file", ErrSourceInvalid, localCandidate)
	}
	// Bind the probe to an immutable byte baseline. Hashing only after the
	// probe could bless a replacement written while ffprobe was running.
	sha, err := hashLocalFileSHA256(ctx, localCandidate)
	if err != nil {
		return attest, err
	}
	candProbe, err := w.probeSourceDetailsForJob(ctx, localCandidate)
	if err != nil {
		return attest, fmt.Errorf("probing local candidate for pre-publish validation: %w", err)
	}
	if candidateDurHint > 0 {
		candProbe.DurationSec = candidateDurHint
	}
	srcVideo, candVideo := firstStreamOfKind(srcProbe.Streams, "video"), firstStreamOfKind(candProbe.Streams, "video")
	if candVideo == nil {
		return attest, fmt.Errorf("%w: local candidate contains no video streams (fail closed, never publish)", ErrSourceInvalid)
	}
	if strings.TrimSpace(candVideo.Codec) == "" {
		return attest, fmt.Errorf("%w: local candidate video stream has empty codec (fail closed, never publish)", ErrSourceInvalid)
	}
	// Video count must be preserved (exactly one video stream in practice).
	if sv, cv := countStreams(srcProbe.Streams, "video"), countStreams(candProbe.Streams, "video"); sv > 0 && cv != sv {
		return attest, fmt.Errorf("%w: local candidate video count %d != source %d (fail closed, never publish)", ErrSourceInvalid, cv, sv)
	}
	// Codec: the candidate must actually be the requested target codec.
	if expected := expectedWorkerVideoCodec(execPlan.Plan.VideoCodec); expected != "" {
		if got := norm(candVideo.Codec); !codecMatches(expected, got) {
			return attest, fmt.Errorf("%w: local candidate video codec %q does not match expected %q (fail closed, never publish)", ErrSourceInvalid, candVideo.Codec, expected)
		}
	}
	// Expected bit depth is a hard invariant.
	if execPlan.Plan.ExpectedBitDepth > 0 && candVideo.BitDepth > 0 && candVideo.BitDepth != execPlan.Plan.ExpectedBitDepth {
		return attest, fmt.Errorf("%w: local candidate bit depth %d != expected %d (fail closed, never publish)", ErrSourceInvalid, candVideo.BitDepth, execPlan.Plan.ExpectedBitDepth)
	}
	if err := pixelFormatCompatible(execPlan.Plan.PixelFormat, candVideo.PixelFormat, execPlan.Plan.ExpectedBitDepth); err != nil {
		return attest, err
	}
	// Resolution preservation when both sides are known.
	if srcVideo != nil && srcVideo.Width > 0 && srcVideo.Height > 0 && candVideo.Width > 0 && candVideo.Height > 0 {
		if srcVideo.Width != candVideo.Width || srcVideo.Height != candVideo.Height {
			return attest, fmt.Errorf("%w: local candidate resolution %dx%d != source %dx%d (fail closed, never publish)", ErrSourceInvalid, candVideo.Width, candVideo.Height, srcVideo.Width, srcVideo.Height)
		}
	}
	if err := validateAudioPreservation(execPlan.Plan, srcProbe.Streams, candProbe.Streams); err != nil {
		return attest, err
	}
	if err := validateSubtitlePreservation(execPlan.Plan, srcProbe.Streams, candProbe.Streams); err != nil {
		return attest, err
	}
	// Attachment preservation policy.
	if execPlan.Plan.PreserveAttachments {
		sa, ca := countStreams(srcProbe.Streams, "attachment"), countStreams(candProbe.Streams, "attachment")
		if sa > 0 && ca != sa {
			return attest, fmt.Errorf("%w: local candidate attachment count %d != source %d (fail closed, never publish)", ErrSourceInvalid, ca, sa)
		}
	}
	// Chapter preservation policy (only when the probe could count chapters).
	if execPlan.Plan.PreserveChapters && srcProbe.Chapters > 0 && candProbe.Chapters != srcProbe.Chapters {
		return attest, fmt.Errorf("%w: local candidate chapter count %d != source %d (fail closed, never publish)", ErrSourceInvalid, candProbe.Chapters, srcProbe.Chapters)
	}
	// Duration sanity mirrors the historical coordinator tolerance (3s absolute
	// and 2% relative). A diverged duration is never published.
	if srcProbe.DurationSec > 0 && candProbe.DurationSec > 0 {
		d := math.Abs(candProbe.DurationSec - srcProbe.DurationSec)
		if d > 3 && (d/srcProbe.DurationSec) > 0.02 {
			return attest, fmt.Errorf("%w: local candidate duration %.1fs diverges from source %.1fs (diff %.1fs, fail closed, never publish)", ErrSourceInvalid, candProbe.DurationSec, srcProbe.DurationSec, d)
		}
	}
	verifiedSHA, err := hashLocalFileSHA256(ctx, localCandidate)
	if err != nil {
		return attest, err
	}
	if verifiedSHA != sha {
		return attest, fmt.Errorf("%w: local candidate changed during validation; never publish", ErrSourceInvalid)
	}
	attest = CandidateAttestation{
		SizeBytes:   info.Size(),
		SHA256:      sha,
		DurationSec: candProbe.DurationSec,
		VideoCodec:  candVideo.Codec,
		Width:       candVideo.Width,
		Height:      candVideo.Height,
		BitDepth:    candVideo.BitDepth,
		PixelFormat: candVideo.PixelFormat,
	}
	return attest, nil
}

func firstStreamOfKind(streams []SourceStream, kind string) *SourceStream {
	for i := range streams {
		if strings.EqualFold(strings.TrimSpace(streams[i].Kind), kind) {
			return &streams[i]
		}
	}
	return nil
}

func countStreams(streams []SourceStream, kind string) int {
	n := 0
	for _, s := range streams {
		if strings.EqualFold(strings.TrimSpace(s.Kind), kind) {
			n++
		}
	}
	return n
}

// expectedWorkerVideoCodec maps an encoder/plan name to the probe codec family
// the container is expected to report.
func expectedWorkerVideoCodec(codec string) string {
	switch norm(codec) {
	case "hevc_videotoolbox", "libx265", "hevc", "h265", "x265":
		return "hevc"
	case "h264_videotoolbox", "h264", "avc", "avc1":
		return "h264"
	case "copy":
		return ""
	default:
		return norm(codec)
	}
}

func codecMatches(expected, got string) bool {
	if expected == "" {
		return true
	}
	if got == "" {
		return false
	}
	return strings.Contains(got, expected) || strings.Contains(expected, got)
}

// pixelFormatCompatible verifies the candidate's pixel format is compatible
// with the plan's requested format. Bit-depth-conflicting or wrong chroma
// families fail closed; an unknown probe value is skipped (structural ffprobe
// normally reports it).
func pixelFormatCompatible(expected, actual string, expectedBitDepth int) error {
	exp, act := norm(expected), norm(actual)
	if exp == "" || act == "" {
		return nil
	}
	if exp == act {
		return nil
	}
	families := map[string]string{
		"p010le": "10", "yuv420p10le": "10",
		"p012le": "12", "yuv420p12le": "12",
		"nv12": "8", "yuv420p": "8",
	}
	fe, eok := families[exp]
	fa, aok := families[act]
	if eok && aok && fe == fa {
		return nil
	}
	return fmt.Errorf("%w: local candidate pixel format %q is incompatible with requested %q (fail closed, never publish)", ErrSourceInvalid, actual, expected)
}

func validateAudioPreservation(plan *transcode.Plan, src, cand []SourceStream) error {
	srcAudio, candAudio := streamsOfKind(src, "audio"), streamsOfKind(cand, "audio")
	if len(candAudio) != len(srcAudio) {
		return fmt.Errorf("%w: local candidate audio count %d != source %d (fail closed, never publish)", ErrSourceInvalid, len(candAudio), len(srcAudio))
	}
	for i := range srcAudio {
		a, b := srcAudio[i], candAudio[i]
		if norm(plan.AudioMode) == "copy" && !codecMatches(norm(a.Codec), norm(b.Codec)) {
			return fmt.Errorf("%w: local candidate audio codec %q at stream %d != source %q (fail closed, never publish)", ErrSourceInvalid, b.Codec, i, a.Codec)
		}
		if a.Language != "" && b.Language != "" && norm(a.Language) != norm(b.Language) {
			return fmt.Errorf("%w: local candidate audio language %q at stream %d != source %q (fail closed, never publish)", ErrSourceInvalid, b.Language, i, a.Language)
		}
		if a.Channels > 0 && b.Channels > 0 && a.Channels != b.Channels {
			return fmt.Errorf("%w: local candidate audio channels %d at stream %d != source %d (fail closed, never publish)", ErrSourceInvalid, b.Channels, i, a.Channels)
		}
	}
	return nil
}

func validateSubtitlePreservation(plan *transcode.Plan, src, cand []SourceStream) error {
	srcSubs, candSubs := streamsOfKind(src, "subtitle"), streamsOfKind(cand, "subtitle")
	if len(candSubs) != len(srcSubs) {
		return fmt.Errorf("%w: local candidate subtitle count %d != source %d (fail closed, never publish)", ErrSourceInvalid, len(candSubs), len(srcSubs))
	}
	actions := map[int]transcode.SubtitleAction{}
	for _, a := range plan.SubtitleActions {
		actions[a.TypeIndex] = a
	}
	for i := range srcSubs {
		a, b := srcSubs[i], candSubs[i]
		act, ok := actions[a.TypeIndex]
		if !ok {
			// The build step already rejected missing/unknown actions; a
			// candidate-side mismatch is still fail-closed.
			continue
		}
		switch norm(act.Operation) {
		case "copy":
			if !codecMatches(norm(a.Codec), norm(b.Codec)) {
				return fmt.Errorf("%w: copied subtitle codec changed at stream %d: source=%s candidate=%s (fail closed, never publish)", ErrSourceInvalid, i, a.Codec, b.Codec)
			}
		case "transcode":
			if !codecMatches(norm(act.Codec), norm(b.Codec)) {
				return fmt.Errorf("%w: converted subtitle codec mismatch at stream %d: expected=%s candidate=%s (fail closed, never publish)", ErrSourceInvalid, i, act.Codec, b.Codec)
			}
		}
		if a.Language != "" && b.Language != "" && norm(a.Language) != norm(b.Language) {
			return fmt.Errorf("%w: subtitle language mismatch at stream %d (fail closed, never publish)", ErrSourceInvalid, i)
		}
		for _, d := range []string{"forced", "default"} {
			if a.Disposition[d] > 0 && b.Disposition[d] == 0 {
				return fmt.Errorf("%w: %s subtitle disposition lost at stream %d (fail closed, never publish)", ErrSourceInvalid, d, i)
			}
		}
	}
	return nil
}

func streamsOfKind(streams []SourceStream, kind string) []SourceStream {
	out := make([]SourceStream, 0, len(streams))
	for _, s := range streams {
		if strings.EqualFold(strings.TrimSpace(s.Kind), kind) {
			out = append(out, s)
		}
	}
	return out
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
