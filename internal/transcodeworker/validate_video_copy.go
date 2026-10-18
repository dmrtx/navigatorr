package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

var streamhashLine = regexp.MustCompile(`^0,v,SHA256=([0-9a-f]{64})$`)

// mainVideoStreamHash hashes the packet payloads of the first programme
// video stream (attached pictures excluded) without decoding it.
func mainVideoStreamHash(ctx context.Context, ffmpegPath, path string) (string, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-nostdin", "-v", "error", "-i", path, "-map", "0:V:0", "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-")
	stderr := newTailBuffer(4096)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("hashing video stream: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	m := streamhashLine.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("hashing video stream: unexpected streamhash output")
	}
	return "sha256:" + m[1], nil
}

// validateVideoCopy is the final check of a size v3 audio-only candidate:
// its programme video must be the source stream unchanged, the same coded
// parameters and the same packet bytes. It replaces perceptual validation,
// which has nothing to measure when no frame was re-encoded.
func (w *Worker) validateVideoCopy(ctx context.Context, source, candidate string, plan *transcode.Plan, candidateSHA string, candidateSize int64) (*transcode.FinalQualityEvidence, error) {
	if err := transcode.ValidateSizeAudioOnlyShape(plan); err != nil {
		return nil, err
	}
	copyEvidence := &transcode.VideoCopyEvidence{Method: transcode.VideoCopyMethod, Verdict: "fail"}
	evidence := &transcode.FinalQualityEvidence{Verdict: "fail", CandidateSHA256: candidateSHA, CandidateSizeBytes: candidateSize, PlanDigest: plan.PlanDigest, VideoCopy: copyEvidence}
	fail := func(code string, err error) (*transcode.FinalQualityEvidence, error) {
		copyEvidence.ReasonCodes = append(copyEvidence.ReasonCodes, code)
		evidence.ReasonCodes = append(evidence.ReasonCodes, code)
		return evidence, &transcode.PolicyRejectionError{ReasonCode: "preservation_not_met", Message: fmt.Sprintf("%s: %v", code, err)}
	}
	srcRep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, source)
	if err != nil {
		return nil, fmt.Errorf("video_copy_source_unreadable: %w", err)
	}
	candRep, err := mediainspect.InspectDetailed(ctx, w.ffprobePath, candidate)
	if err != nil {
		return nil, fmt.Errorf("video_copy_candidate_unreadable: %w", err)
	}
	sv, cv := srcRep.MainVideo(), candRep.MainVideo()
	if len(sv) != 1 || len(cv) != 1 {
		return fail("video_copy_stream_count_changed", fmt.Errorf("source has %d programme video streams, candidate %d", len(sv), len(cv)))
	}
	s, c := sv[0], cv[0]
	copyEvidence.SourceCodec = s.Codec
	evidence.SourceBitDepth, evidence.CandidateBitDepth = s.BitDepth, c.BitDepth
	if s.BitDepth != plan.ExpectedBitDepth {
		return fail("video_copy_source_changed", fmt.Errorf("source is %d-bit, plan froze %d-bit", s.BitDepth, plan.ExpectedBitDepth))
	}
	type params struct {
		codec, profile, pixfmt, field, rng, space, primaries, transfer string
		width, height, depth                                           int
	}
	of := func(v mediainspect.DetailedStream) params {
		return params{norm(v.Codec), norm(v.Profile), norm(v.PixelFormat), norm(v.FieldOrder), norm(v.ColorRange), norm(v.ColorSpace), norm(v.ColorPrimaries), norm(v.ColorTransfer), v.Width, v.Height, v.BitDepth}
	}
	if of(s) != of(c) {
		return fail("video_copy_parameters_changed", fmt.Errorf("source %+v, candidate %+v", of(s), of(c)))
	}
	if copyEvidence.SourceHash, err = mainVideoStreamHash(ctx, w.ffmpegPath, source); err != nil {
		return nil, err
	}
	if copyEvidence.CandidateHash, err = mainVideoStreamHash(ctx, w.ffmpegPath, candidate); err != nil {
		return nil, err
	}
	if copyEvidence.SourceHash != copyEvidence.CandidateHash {
		return fail("video_copy_packets_changed", fmt.Errorf("video packet hash differs"))
	}
	copyEvidence.Verdict, evidence.Verdict = "pass", "pass"
	return evidence, nil
}
