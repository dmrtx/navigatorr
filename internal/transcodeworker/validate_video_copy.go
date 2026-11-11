package transcodeworker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"reflect"
	"slices"
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
	// paramSets is the set of parameter-set NAL units (SPS, PPS, VPS) in the
	// extradata, however it is packaged: Annex-B in a transport stream,
	// avcC/hvcC in MP4 and Matroska. Empty when the codec has none or the
	// record does not parse.
	paramSets string
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

// probeVideoPackets lists the packets of the video stream with the given
// index, each with a SHA-256 of its payload. The caller passes the programme
// video's index: "the first video" can be a preview track listed before it.
func probeVideoPackets(ctx context.Context, ffprobePath, path string, stream int) ([]videoPacket, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-hide_banner", "-v", "error", "-select_streams", strconv.Itoa(stream), "-show_packets", "-show_data_hash", "sha256",
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

func probeVideoStreamFacts(ctx context.Context, ffprobePath, path string, stream int) (videoStreamFacts, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-hide_banner", "-v", "error", "-select_streams", strconv.Itoa(stream), "-show_data",
		"-show_entries", "stream=codec_name,extradata,sample_aspect_ratio,r_frame_rate,avg_frame_rate", "-of", "json", path)
	out, err := cmd.Output()
	chargeCPU(ctx, cmd)
	if err != nil {
		return videoStreamFacts{}, fmt.Errorf("reading video stream fields: %w", err)
	}
	var probe struct {
		Streams []struct {
			Codec        string `json:"codec_name"`
			Extradata    string `json:"extradata"`
			Aspect       string `json:"sample_aspect_ratio"`
			RFrameRate   string `json:"r_frame_rate"`
			AvgFrameRate string `json:"avg_frame_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return videoStreamFacts{}, fmt.Errorf("reading video stream fields: %w", err)
	}
	if len(probe.Streams) != 1 {
		return videoStreamFacts{}, fmt.Errorf("reading video stream fields: expected one main video stream")
	}
	st := probe.Streams[0]
	extradata, err := parseFFprobeHexDump(st.Extradata)
	if err != nil {
		return videoStreamFacts{}, fmt.Errorf("reading video extradata: %w", err)
	}
	if norm(st.Codec) == "hevc" {
		extradata = normalizeHVCC(extradata)
	}
	sum := sha256.Sum256(extradata)
	return videoStreamFacts{extradata: "sha256:" + hex.EncodeToString(sum[:]), paramSets: canonicalParameterSets(norm(st.Codec), extradata), aspect: st.Aspect, rFrameRate: st.RFrameRate, avgFrameRate: st.AvgFrameRate}, nil
}

// canonicalParameterSets digests the parameter-set NAL units of an H.264 or
// HEVC extradata (an avcC or hvcC record, or Annex-B) by their bytes, sorted
// and de-duplicated, so the same sets in another packaging give the same
// digest. It returns "" for other codecs or a record that does not parse.
func canonicalParameterSets(codec string, data []byte) string {
	if codec != "h264" && codec != "hevc" {
		return ""
	}
	var nals [][]byte
	switch {
	case len(data) >= 4 && (data[0] == 0 && data[1] == 0 && (data[2] == 1 || data[2] == 0 && data[3] == 1)):
		for _, n := range splitAnnexB(data) {
			if len(n) == 0 {
				continue
			}
			t := int(n[0] & 0x1f)
			isSet := t == 7 || t == 8
			if codec == "hevc" {
				t = int(n[0]>>1) & 0x3f
				isSet = t >= 32 && t <= 34
			}
			if isSet {
				nals = append(nals, n)
			}
		}
	case codec == "h264" && len(data) >= 7 && data[0] == 1:
		pos := 5
		for _, group := range []int{int(data[pos] & 0x1f), -1} {
			if group < 0 {
				if pos >= len(data) {
					return ""
				}
				group = int(data[pos])
			}
			pos++
			for range group {
				if pos+2 > len(data) {
					return ""
				}
				n := int(data[pos])<<8 | int(data[pos+1])
				pos += 2
				if pos+n > len(data) {
					return ""
				}
				nals = append(nals, data[pos:pos+n])
				pos += n
			}
		}
	case codec == "hevc" && len(data) >= 23 && data[0] == 1:
		pos := 23
		for range int(data[22]) {
			if pos+3 > len(data) {
				return ""
			}
			count := int(data[pos+1])<<8 | int(data[pos+2])
			pos += 3
			for range count {
				if pos+2 > len(data) {
					return ""
				}
				n := int(data[pos])<<8 | int(data[pos+1])
				pos += 2
				if pos+n > len(data) {
					return ""
				}
				nals = append(nals, data[pos:pos+n])
				pos += n
			}
		}
	default:
		return ""
	}
	if len(nals) == 0 {
		return ""
	}
	digests := make([]string, 0, len(nals))
	for _, n := range nals {
		sum := sha256.Sum256(n)
		digests = append(digests, hex.EncodeToString(sum[:]))
	}
	slices.Sort(digests)
	digests = slices.Compact(digests)
	sum := sha256.Sum256([]byte(strings.Join(digests, ",")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// splitAnnexB returns the NAL units of an Annex-B buffer, without their
// start codes or trailing zero bytes.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		if start >= 0 {
			nals = append(nals, bytes.TrimRight(data[start:i], "\x00"))
		}
		start = i + 3
		i += 2
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, bytes.TrimRight(data[start:], "\x00"))
	}
	return nals
}

// probeCanonicalPackets lists the main video's packets as an Annex-B stream
// with the parameter sets and access unit delimiters taken out, one size and
// hash per packet. A transport stream carries its packets in Annex-B and
// Matroska and MP4 length-prefixed, with the parameter sets in the record;
// in this form the same coded pictures read the same in either. Only
// H.264 and HEVC have the conversion; any other codec returns an error.
func probeCanonicalPackets(ctx context.Context, ffmpegPath, path string, stream int, codec string) ([]videoPacket, error) {
	var bsf string
	switch codec {
	case "h264":
		bsf = "h264_mp4toannexb,filter_units=remove_types=7|8|9"
	case "hevc":
		bsf = "hevc_mp4toannexb,filter_units=remove_types=32|33|34|35"
	default:
		return nil, fmt.Errorf("no canonical packet form for %q", codec)
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, "-nostdin", "-hide_banner", "-v", "error", "-i", path, "-map", "0:"+strconv.Itoa(stream),
		"-c", "copy", "-copyinkf", "-bsf:v", bsf, "-f", "framehash", "-hash", "sha256", "-")
	stderr := newTailBuffer(4096)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	chargeCPU(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("listing canonical video packets: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var packets []videoPacket
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 6 {
			return nil, fmt.Errorf("listing canonical video packets: unexpected line")
		}
		size, err := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
		hash := strings.TrimSpace(f[5])
		if err != nil || len(hash) != 64 {
			return nil, fmt.Errorf("listing canonical video packets: unexpected line")
		}
		packets = append(packets, videoPacket{size: size, hash: hash})
	}
	return packets, nil
}

// canonicalPacketsMatch reports whether the candidate's packets are the
// source's after the packaging of the container is taken out, with the
// ffprobe listings' flags and timestamps carried over unchanged. It is
// consulted only when the exact comparison failed.
func (w *Worker) canonicalPacketsMatch(ctx context.Context, source, candidate, codec string, sIdx, cIdx int, src, cand []videoPacket) bool {
	if len(src) != len(cand) {
		return false
	}
	sc, err := probeCanonicalPackets(ctx, w.ffmpegPath, source, sIdx, codec)
	if err != nil || len(sc) != len(src) {
		return false
	}
	cc, err := probeCanonicalPackets(ctx, w.ffmpegPath, candidate, cIdx, codec)
	if err != nil || len(cc) != len(cand) {
		return false
	}
	s2, c2 := slices.Clone(src), slices.Clone(cand)
	for i := range s2 {
		s2[i].size, s2[i].hash = sc[i].size, sc[i].hash
		c2[i].size, c2[i].hash = cc[i].size, cc[i].hash
	}
	code, _ := compareVideoPackets(s2, c2)
	return code == ""
}

// parseFFprobeHexDump decodes ffprobe's -show_data dump: lines of an
// 8-digit offset, a colon, then up to 16 bytes as 4-digit groups in a fixed
// 40-column field before the ASCII column.
func parseFFprobeHexDump(dump string) ([]byte, error) {
	var data []byte
	for _, line := range strings.Split(dump, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 10 || line[8:10] != ": " {
			return nil, fmt.Errorf("unexpected hex dump line %q", line)
		}
		field := line[10:]
		if len(field) > 40 {
			field = field[:40]
		}
		b, err := hex.DecodeString(strings.ReplaceAll(field, " ", ""))
		if err != nil {
			return nil, fmt.Errorf("unexpected hex dump line %q: %w", line, err)
		}
		if offset, err := strconv.ParseUint(line[:8], 16, 32); err != nil || int(offset) != len(data) {
			return nil, fmt.Errorf("hex dump offset %q out of order", line[:8])
		}
		data = append(data, b...)
	}
	return data, nil
}

// normalizeHVCC clears the array_completeness bit of each parameter-set
// array in an HEVC decoder configuration record. The bit only says whether
// every parameter set of that type is in the record or may also be in-band;
// FFmpeg's Matroska muxer writes it as 0, so a plain remux of an mkvmerge
// file otherwise reads as a changed record although every VPS, SPS and PPS
// byte (and every packet) is the source's. A record that does not parse is
// returned unchanged and compared as is.
func normalizeHVCC(rec []byte) []byte {
	const header = 23 // 22 fixed bytes, then numOfArrays
	if len(rec) < header || rec[0] != 1 {
		return rec
	}
	out := append([]byte(nil), rec...)
	pos := header
	for range int(rec[header-1]) {
		if pos+3 > len(out) {
			return rec
		}
		out[pos] &^= 0x80
		n := int(out[pos+1])<<8 | int(out[pos+2])
		pos += 3
		for range n {
			if pos+2 > len(out) {
				return rec
			}
			pos += 2 + (int(out[pos])<<8 | int(out[pos+1]))
		}
	}
	if pos != len(out) {
		return rec
	}
	return out
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
	if srcFacts, err = probeVideoStreamFacts(ctx, w.ffprobePath, source, s.Index); err != nil {
		return nil, err
	}
	if candFacts, err = probeVideoStreamFacts(ctx, w.ffprobePath, candidate, c.Index); err != nil {
		return nil, err
	}
	// The parameter sets are compared below: their packaging (Annex-B in a
	// transport stream, avcC/hvcC in Matroska) changes the extradata bytes of a
	// correct remux.
	of := func(v mediainspect.DetailedStream, f videoStreamFacts) params {
		f.extradata, f.paramSets = "", ""
		return params{norm(v.Codec), norm(v.Profile), norm(v.PixelFormat), norm(v.FieldOrder), norm(v.ColorRange), norm(v.ColorSpace), norm(v.ColorPrimaries), norm(v.ColorTransfer), v.Width, v.Height, v.BitDepth, f}
	}
	if of(s, srcFacts) != of(c, candFacts) {
		return fail("video_copy_parameters_changed", fmt.Errorf("source %+v, candidate %+v", of(s, srcFacts), of(c, candFacts)))
	}
	repackaged := srcFacts.extradata != candFacts.extradata
	if repackaged && (srcFacts.paramSets == "" || srcFacts.paramSets != candFacts.paramSets) {
		return fail("video_copy_parameters_changed", fmt.Errorf("parameter sets differ: source %+v, candidate %+v", srcFacts, candFacts))
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
		p, err := probeVideoPackets(ctx, w.ffprobePath, candidate, c.Index)
		candDone <- listing{p, err}
	}()
	srcPackets, srcErr := probeVideoPackets(ctx, w.ffprobePath, source, s.Index)
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
		// A transport stream's packets are Annex-B and Matroska's are
		// length-prefixed, so a correct remux reads differently packet by
		// packet; the same pictures in the canonical form are the same copy.
		if !(code == "video_copy_packets_changed" && repackaged && w.canonicalPacketsMatch(ctx, source, candidate, norm(s.Codec), s.Index, c.Index, srcPackets, cand.packets)) {
			return fail(code, err)
		}
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
