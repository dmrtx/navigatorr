package transcodeworker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// videoPacket is one demuxed packet of the main video, as ffprobe reads it
// from the container: no stream copy runs, so nothing is dropped before the
// first keyframe and a stream with no flagged keyframe still has packets.
type videoPacket struct {
	pts    float64
	hasPTS bool
	size   int64
	key    bool
	hash   string
}

// videoStreamFacts are the stream-level fields a remux must keep.
type videoStreamFacts struct {
	extradata, aspect, rFrameRate, avgFrameRate string
}

// videoCopyTimestampTolerance absorbs the container clock: Matroska stores
// milliseconds, so an MP4 source's timestamps round by up to half a tick on
// each side.
const videoCopyTimestampTolerance = 0.0015

func compactFields(line string) map[string]string {
	fields := map[string]string{}
	for _, part := range strings.Split(line, "|") {
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[k] = v
		}
	}
	return fields
}

// probeVideoPackets lists the main video's packets (attached pictures
// excluded) with a SHA-256 of each payload.
func probeVideoPackets(ctx context.Context, ffprobePath, path string) ([]videoPacket, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-hide_banner", "-v", "error", "-select_streams", "V:0", "-show_packets", "-show_data_hash", "sha256",
		"-show_entries", "packet=pts_time,size,flags,data_hash", "-of", "compact=p=0", path)
	stderr := newTailBuffer(4096)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("listing video packets: %w", err)
	}
	var packets []videoPacket
	var parseErr error
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		f := compactFields(scanner.Text())
		size, err := strconv.ParseInt(f["size"], 10, 64)
		hash := strings.ToLower(strings.TrimPrefix(f["data_hash"], "SHA256:"))
		if err != nil || len(hash) != 64 || f["flags"] == "" {
			parseErr = fmt.Errorf("listing video packets: unexpected ffprobe packet line")
			continue
		}
		p := videoPacket{size: size, key: strings.HasPrefix(f["flags"], "K"), hash: hash}
		if v, err := strconv.ParseFloat(f["pts_time"], 64); err == nil {
			p.pts, p.hasPTS = v, true
		}
		packets = append(packets, p)
	}
	if err := scanner.Err(); err != nil && parseErr == nil {
		parseErr = fmt.Errorf("listing video packets: %w", err)
	}
	err = cmd.Wait()
	chargeCPU(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("listing video packets: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return packets, parseErr
}

func probeVideoStreamFacts(ctx context.Context, ffprobePath, path string) (videoStreamFacts, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-hide_banner", "-v", "error", "-select_streams", "V:0", "-show_data_hash", "sha256",
		"-show_entries", "stream=extradata_hash,sample_aspect_ratio,r_frame_rate,avg_frame_rate", "-of", "compact=p=0", path)
	out, err := cmd.Output()
	chargeCPU(ctx, cmd)
	if err != nil {
		return videoStreamFacts{}, fmt.Errorf("reading video stream fields: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		return videoStreamFacts{}, fmt.Errorf("reading video stream fields: expected one main video stream")
	}
	f := compactFields(lines[0])
	return videoStreamFacts{extradata: strings.ToLower(f["extradata_hash"]), aspect: f["sample_aspect_ratio"], rFrameRate: f["r_frame_rate"], avgFrameRate: f["avg_frame_rate"]}, nil
}

// videoPacketDigest binds the packet sequence: count, order, sizes,
// keyframe flags and payloads. Timestamps are compared separately, within
// the container clock tolerance.
func videoPacketDigest(packets []videoPacket) string {
	h := sha256.New()
	for _, p := range packets {
		fmt.Fprintf(h, "%d,%t,%s\n", p.size, p.key, p.hash)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// compareVideoPackets returns the reason code when the candidate's packets
// are not the source's packets, in order, with the same timing.
func compareVideoPackets(src, cand []videoPacket) (string, error) {
	if len(src) == 0 || len(cand) == 0 {
		return "video_copy_stream_empty", fmt.Errorf("source has %d video packets, candidate %d", len(src), len(cand))
	}
	if len(src) != len(cand) {
		return "video_copy_packets_changed", fmt.Errorf("source has %d video packets, candidate %d", len(src), len(cand))
	}
	srcBase, candBase := math.NaN(), math.NaN()
	for i := range src {
		s, c := src[i], cand[i]
		if s.size != c.size || s.key != c.key || s.hash != c.hash {
			return "video_copy_packets_changed", fmt.Errorf("video packet %d differs", i)
		}
		if s.hasPTS != c.hasPTS {
			return "video_copy_timestamps_changed", fmt.Errorf("video packet %d timestamp presence differs", i)
		}
		if !s.hasPTS {
			continue
		}
		if math.IsNaN(srcBase) {
			srcBase, candBase = s.pts, c.pts
		}
		if math.Abs((s.pts-srcBase)-(c.pts-candBase)) > videoCopyTimestampTolerance {
			return "video_copy_timestamps_changed", fmt.Errorf("video packet %d timestamp moved", i)
		}
	}
	return "", nil
}

// validateVideoCopy is the final check of a size v3 audio-only candidate:
// its programme video must be the source stream unchanged, the same coded
// parameters and stream fields and the same demuxed packets with the same
// timing. It replaces perceptual validation, which has nothing to measure
// when no frame was re-encoded.
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
		facts                                                          videoStreamFacts
	}
	var srcFacts, candFacts videoStreamFacts
	if srcFacts, err = probeVideoStreamFacts(ctx, w.ffprobePath, source); err != nil {
		return nil, err
	}
	if candFacts, err = probeVideoStreamFacts(ctx, w.ffprobePath, candidate); err != nil {
		return nil, err
	}
	of := func(v mediainspect.DetailedStream, f videoStreamFacts) params {
		return params{norm(v.Codec), norm(v.Profile), norm(v.PixelFormat), norm(v.FieldOrder), norm(v.ColorRange), norm(v.ColorSpace), norm(v.ColorPrimaries), norm(v.ColorTransfer), v.Width, v.Height, v.BitDepth, f}
	}
	if of(s, srcFacts) != of(c, candFacts) {
		return fail("video_copy_parameters_changed", fmt.Errorf("source %+v, candidate %+v", of(s, srcFacts), of(c, candFacts)))
	}
	// HDR and Dolby Vision signalling partly lives in the container (the
	// mastering display, light level and Dolby Vision configuration), which
	// identical packets do not prove; the audio-only goal admits such sources,
	// so their classification must survive the remux unchanged.
	if plan.ModePolicyVersion == transcode.ModePolicyV7 && (mediainspect.IsHDRorDolbyVisionReport(&srcRep) || mediainspect.IsHDRorDolbyVisionReport(&candRep)) {
		srcDR, err := classifySourceDynamicRange(ctx, w.ffprobePath, source, &srcRep)
		if err != nil {
			return nil, fmt.Errorf("video_copy_source_unreadable: %w", err)
		}
		candDR, err := classifySourceDynamicRange(ctx, w.ffprobePath, candidate, &candRep)
		if err != nil {
			return nil, fmt.Errorf("video_copy_candidate_unreadable: %w", err)
		}
		if !sameDynamicRange(srcDR, candDR) {
			return fail("video_copy_dynamic_range_changed", fmt.Errorf("source %+v, candidate %+v", srcDR, candDR))
		}
	}
	// Both files are read in parallel: the proof reads every packet.
	type listing struct {
		packets []videoPacket
		err     error
	}
	candDone := make(chan listing, 1)
	go func() {
		p, err := probeVideoPackets(ctx, w.ffprobePath, candidate)
		candDone <- listing{p, err}
	}()
	srcPackets, srcErr := probeVideoPackets(ctx, w.ffprobePath, source)
	cand := <-candDone
	if srcErr != nil {
		return nil, srcErr
	}
	if cand.err != nil {
		return nil, cand.err
	}
	copyEvidence.SourcePackets, copyEvidence.CandidatePackets = len(srcPackets), len(cand.packets)
	copyEvidence.SourceHash, copyEvidence.CandidateHash = videoPacketDigest(srcPackets), videoPacketDigest(cand.packets)
	if code, err := compareVideoPackets(srcPackets, cand.packets); code != "" {
		return fail(code, err)
	}
	copyEvidence.Verdict, evidence.Verdict = "pass", "pass"
	return evidence, nil
}

// sameDynamicRange compares two classifications of the same video stream,
// ignoring how many frames each read took.
func sameDynamicRange(a, b mediainspect.DynamicRange) bool {
	a.FramesProbed, b.FramesProbed = 0, 0
	return reflect.DeepEqual(a, b)
}
