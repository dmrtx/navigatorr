package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// This file holds the candidate checks that read packets, not the probe's
// stream list: the programme video's frames, and every kept subtitle's
// events. The probe says a track is there; only its packets say it is whole.

// videoCompleteness holds the candidate's programme video to the source's
// packet scan. The encode passes every source picture through with its own
// timestamp (sourceTimestampArgs), so the candidate has one frame for each
// picture a decoder shows from the source, and its pictures sit where the
// source's do. The container's duration cannot tell: it is the longest
// stream's, so a video that stops early hides behind audio that runs to the
// end.
//
// The source's pictures are counted before the encode (sourcePictures): its
// head is decoded, which says exactly which of the first packets show, and
// every later packet shows but for those the container marks to be
// discarded. Only a field-coded source, whose pictures may each take one
// packet or two, is held to a range.
//
// The candidate's first and last pictures are then held to the source's, by
// their times, not by the length of the last one: a variable frame rate has
// no one frame length. The candidate may start on a leading picture the
// source's first keyframe carries, or, for a source with no keyframe flagged,
// as many pictures late as the decoder skipped.
func videoCompleteness(src, cand sourceVideoScan, codec string, fps float64) error {
	if src.Packets == 0 {
		return candidateInvalid(fmt.Errorf("%w: the source's video was measured with no packets; nothing says what the converted video must hold (fail closed, never publish)", ErrContentNotPreserved))
	}
	if cand.Packets == 0 {
		return candidateInvalid(fmt.Errorf("%w: the converted video has no frames; the source has %d (fail closed, never publish)", ErrContentNotPreserved, src.Packets))
	}
	if cand.DemuxErrors > 0 {
		return candidateInvalid(fmt.Errorf("%w: the converted video is damaged: reading it reported %d error(s) (%s) (fail closed, never publish)", ErrContentNotPreserved, cand.DemuxErrors, truncateForLog(cand.FirstError, 200)))
	}
	p := sourcePictures(src, codec, fps)
	if cand.Packets < p.least || cand.Packets > p.most {
		source := fmt.Sprintf("the source shows %d", p.least)
		switch {
		case p.fields && cand.Packets > p.most:
			source = fmt.Sprintf("the field-coded source shows at most %d", p.most)
		case p.fields || p.least != p.most:
			source = fmt.Sprintf("the source shows at least %d", p.least)
		}
		return candidateInvalid(fmt.Errorf("%w: the converted video has %d frames, %s; frames were lost or added in the conversion (fail closed, never publish)", ErrContentNotPreserved, cand.Packets, source))
	}
	if !src.timed || !cand.timed || src.Discontinuous || cand.Discontinuous {
		// A timeline that restarts, or has no times, has no span to compare;
		// the frames do.
		return nil
	}
	// Half a frame of slack absorbs the containers' timestamp rounding and a
	// field-coded source's last field, which shows half a frame after its
	// picture's first.
	tol := 0.002 + p.frame/2
	srcSpan, candSpan := src.Last-src.First, cand.Last-cand.start()
	diff := candSpan - srcSpan
	if diff < -p.lateStart-tol || diff > (src.First-src.start())+tol {
		return candidateInvalid(fmt.Errorf("%w: the converted video's last frame shows %.3fs after its first, the source's %.3fs; its pictures are not where the source's are (fail closed, never publish)", ErrContentNotPreserved, candSpan, srcSpan))
	}
	return nil
}

// validateVideoCompleteness holds the candidate's programme video, as scanned
// with its other tracks, to the source's scan taken before the encode. The
// source is never read again after the encode.
func validateVideoCompleteness(srcProbe SourceProbe, cand sourceVideoScan) error {
	if srcProbe.videoScan == nil {
		return candidateInvalid(fmt.Errorf("%w: the source's video was not measured before the encode (fail closed, never publish)", ErrContentNotPreserved))
	}
	v := firstMainVideo(srcProbe.Streams)
	if v == nil {
		return candidateInvalid(fmt.Errorf("%w: the source has no programme video to hold the converted one to (fail closed, never publish)", ErrContentNotPreserved))
	}
	return videoCompleteness(*srcProbe.videoScan, cand, v.Codec, v.FPS)
}

// pictureCount is how many pictures a decoder shows from the source's video.
type pictureCount struct {
	// least and most bound the pictures; they are equal unless the source is
	// field-coded or its head could not be decoded far enough.
	least, most int
	// fields is set when some packets may be fields.
	fields bool
	// frame is one picture's duration (0 when unknown), and lateStart how
	// much later than the source's first picture the candidate's may be.
	frame, lateStart float64
}

// fieldCodecs are the codecs whose pictures may be coded as two fields, each
// in a packet of its own (PAFF, from a transport stream).
var fieldCodecs = map[string]bool{"h264": true, "mpeg2video": true, "vc1": true}

// fieldPacketShare is how far, as a share of a frame, a packet must fall
// short of a whole frame to count as a possible field.
const fieldPacketShare = 0.6

// sourcePictures counts the pictures a decoder shows from the source.
//
// The decoded head says exactly how many of its packets show no picture: those
// before the first keyframe, the keyframe's leading pictures whose references
// were cut away and those the container marks to be discarded. Every packet
// after the head shows a picture but the discarded ones, so a frame-coded
// source has exactly Packets - (head packets - head pictures) - (discarded
// after the head) pictures, with no slack.
//
// A field-coded source is told by its head: more of its packets show no
// picture than the packets before the keyframe, its leading pictures and the
// discarded ones (the second field of each picture), or, for a source with no
// keyframe flagged, more than a quarter. A source whose head is frame-coded
// may still switch to fields later: a packet of a field codec lasting less
// than fieldPacketShare of a frame says so, and so does one whose time lies
// that close to the previous packet's, whatever length the container states
// for it (fieldSteps). Its pictures then lie between every packet that may
// show and half the possible fields fewer.
func sourcePictures(src sourceVideoScan, codec string, fps float64) pictureCount {
	var out pictureCount
	n := src.headPackets
	if !src.headDecoded {
		n, _ = src.headLength()
	}
	nonKey, leading, headDiscarded := src.headStats(n)
	out.frame = src.headFrame()
	if fps > 0 && out.frame <= 0 {
		out.frame = 1 / fps
	}
	// Every packet but those before the first keyframe and the discarded
	// ones, each counted once.
	most := src.Packets - src.LeadingNonKey - (src.Discarded - src.leadDiscarded)
	if !src.sawKey {
		most = src.Packets - src.Discarded
	}
	notShown := src.headPackets - src.headFrames
	switch {
	case src.fieldCoded(codec, out.frame, nonKey, leading, headDiscarded):
		possible, unknown := 0, 0
		for d, c := range src.durations {
			switch {
			case d <= 0:
				unknown += c
			case out.frame > 0 && float64(d)/1e6 < out.frame*fieldPacketShare:
				possible += c
			}
		}
		if src.headDecoded && src.headFrames < src.headPackets-nonKey-leading-headDiscarded {
			// The head shows fields: a packet whose length the container
			// does not state may be one.
			possible += unknown
		}
		if fieldCodecs[strings.ToLower(codec)] {
			possible = max(possible, src.fieldSteps(out.frame))
		}
		out.fields, out.most = true, most
		out.least = max(most-leading-possible/2, (most+1)/2)
	case !src.headDecoded:
		// A test's scan, never decoded: the leading pictures may or may not
		// show.
		out.least, out.most = most, most
		if src.sawKey {
			out.least = most - leading
		}
	default:
		exact := src.Packets - notShown - (src.Discarded - headDiscarded)
		out.least, out.most = exact, exact
		if _, complete := src.headLength(); !complete {
			// More packets before the first keyframe than the head holds:
			// those past it show nothing either.
			out.least = min(exact, most-leading)
		}
		if !src.sawKey {
			out.lateStart = float64(notShown) * out.frame
		}
	}
	return out
}

// fieldCoded reports whether some of the source's pictures may be coded as
// two packets (see sourcePictures).
func (s sourceVideoScan) fieldCoded(codec string, frame float64, nonKey, leading, discarded int) bool {
	if s.headDecoded {
		notShown := s.headPackets - s.headFrames
		if s.sawKey && notShown > nonKey+leading+discarded {
			return true
		}
		if !s.sawKey && 4*notShown > s.headPackets {
			return true
		}
	}
	if !fieldCodecs[strings.ToLower(codec)] || frame <= 0 {
		return false
	}
	for d, c := range s.durations {
		if d > 0 && c > 0 && float64(d)/1e6 < frame*fieldPacketShare {
			return true
		}
	}
	return s.fieldSteps(frame) > 0
}

// headFrame is one picture's duration as the decoded head shows it: the time
// its pictures from the first keyframe on span, over their count (0 when the
// head was not decoded or holds one picture). A field's packet is half a
// picture, so the packets' own lengths cannot tell it.
func (s sourceVideoScan) headFrame() float64 {
	if !s.headDecoded || s.headFrames < 2 || s.headPackets > len(s.head) {
		return 0
	}
	lo, hi, seen := 0.0, 0.0, false
	started, keyPTS := !s.sawKey, 0.0
	for _, p := range s.head[:s.headPackets] {
		if !started {
			if !p.key {
				continue
			}
			started, keyPTS = true, p.pts
		}
		if !p.timed || p.discard || (s.sawKey && p.pts < keyPTS) {
			continue
		}
		if !seen || p.pts < lo {
			lo = p.pts
		}
		if !seen || p.pts > hi {
			hi = p.pts
		}
		seen = true
	}
	return (hi - lo) / float64(s.headFrames)
}

// subtitlePackets is one subtitle stream's packets: how many, how many the
// container marks to be discarded and how many carry no time. events is how
// many lines a conversion of the track writes (counted when it is converted,
// see countSubtitleEvents).
type subtitlePackets struct {
	packets, discarded, untimed int64
	events                      int64
	counted                     bool
}

// add counts one packet from ffprobe's fields.
func (c *subtitlePackets) add(fields map[string]string) {
	c.packets++
	switch {
	case strings.Contains(fields["flags"], "D"):
		c.discarded++
	case !timedField(fields["pts_time"]) && !timedField(fields["dts_time"]):
		c.untimed++
	}
}

func timedField(v string) bool {
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// measuresSubtitlePackets reports whether the kept subtitles' packets are
// counted: whenever the output keeps any, unless a test injected the probe
// without the scan.
func (w *Worker) measuresSubtitlePackets(plan *transcode.Plan, src SourceProbe) bool {
	return len(plannedStreams(plan, src.Streams, "subtitle")) > 0 && (w.scanSubtitles != nil || !w.probeInjected())
}

// subtitlePacketsForJob uses the injected subtitle scan when present.
func (w *Worker) subtitlePacketsForJob(ctx context.Context, path string, indexes map[int]bool) (map[int]subtitlePackets, error) {
	if w.scanSubtitles != nil {
		return w.scanSubtitles(ctx, path)
	}
	scan, err := scanFilePackets(ctx, w.ffprobePath, path, packetScanRequest{video: -1, subtitles: indexes})
	return scan.subtitles, err
}

// packetScanFor is what the source's scan before the encode measures: the
// programme video, and the audio and subtitle tracks the output keeps whose
// packets the candidate's are held to. A dropped track is not read: its
// packets cannot fail a job that leaves it out. A test that injects the probe
// describes files it never wrote: only the kinds it injects scans for are
// measured there.
func (w *Worker) packetScanFor(plan *transcode.Plan, src SourceProbe) packetScanRequest {
	req := packetScanRequest{video: -1}
	if w.scanVideo != nil || !w.probeInjected() {
		req.video = programmeVideoIndex(src.Streams)
	}
	if w.measuresAudioPackets(plan, src) {
		req.audio = streamIndexes(plannedStreams(plan, src.Streams, "audio"))
	}
	if w.measuresSubtitlePackets(plan, src) {
		req.subtitles = streamIndexes(plannedStreams(plan, src.Streams, "subtitle"))
	}
	return req
}

// sourceMeasured fails when a kind the source's scan measures (packetScanFor)
// carries no measurement: the candidate's packets would then be held to
// nothing, and a check never passes for want of one.
func sourceMeasured(req packetScanRequest, src SourceProbe) error {
	var missing []string
	if req.video >= 0 && src.videoScan == nil {
		missing = append(missing, "video")
	}
	if req.audio != nil && src.audioSpans == nil {
		missing = append(missing, "audio")
	}
	if req.subtitles != nil && src.subtitlePackets == nil {
		missing = append(missing, "subtitle")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("the source's %s packets were not measured before the encode; the converted file's cannot be held to them (fail closed, never publish)", strings.Join(missing, " and "))
}

// candidateScanFor is what the candidate's scan after the encode measures:
// the same kinds as the source's, every one of the candidate's tracks.
func candidateScanFor(source packetScanRequest, cand []SourceStream, candVideo *SourceStream) packetScanRequest {
	req := packetScanRequest{video: -1}
	if source.video >= 0 && candVideo != nil {
		req.video = candVideo.Index
	}
	if source.audio != nil {
		req.audio = streamIndexes(streamsOfKind(cand, "audio"))
	}
	if source.subtitles != nil {
		req.subtitles = streamIndexes(streamsOfKind(cand, "subtitle"))
	}
	return req
}

func streamIndexes(streams []SourceStream) map[int]bool {
	out := make(map[int]bool, len(streams))
	for _, s := range streams {
		out[s.Index] = true
	}
	return out
}

// readsPackets reports whether the packet scans read the files themselves,
// not a test's injected scans.
func (w *Worker) readsPackets() bool {
	return w.scanVideo == nil && w.scanAudio == nil && w.scanSubtitles == nil && !w.probeInjected()
}

// packetsForJob reads a file's packets for the request in one pass, or with
// the per-kind scans a test injects to describe files it never wrote.
func (w *Worker) packetsForJob(ctx context.Context, path string, req packetScanRequest) (filePackets, error) {
	if w.readsPackets() {
		return scanFilePackets(ctx, w.ffprobePath, path, req)
	}
	out := filePackets{video: sourceVideoScan{FirstErrorByte: -1}}
	var err error
	if req.video >= 0 {
		if out.video, err = w.scanSourceVideoForJob(ctx, path, req.video); err != nil {
			return out, err
		}
	}
	if req.audio != nil {
		if out.audio, err = w.audioPacketSpansForJob(ctx, path); err != nil {
			return out, fmt.Errorf("audio packets: %w", err)
		}
	}
	if req.subtitles != nil {
		if out.subtitles, err = w.subtitlePacketsForJob(ctx, path, req.subtitles); err != nil {
			return out, fmt.Errorf("subtitle packets: %w", err)
		}
	}
	return out, nil
}

// scanSourcePackets reads the source's packets once, before the encode, for
// everything the candidate is held to after it (packetScanFor), and keeps the
// measures on the probe it returns. The candidate checks never read the
// source again. What the checks after the encode could only refuse is refused
// here, before hours of encoding: a video with no packets, a converted audio
// track with no timed packet to measure it by.
func (w *Worker) scanSourcePackets(ctx context.Context, plan *transcode.Plan, probe SourceProbe, path string) (SourceProbe, error) {
	req := w.packetScanFor(plan, probe)
	if !req.any() {
		return probe, nil
	}
	scan, err := w.packetsForJob(ctx, path, req)
	if err != nil {
		return probe, err
	}
	if req.video >= 0 {
		if w.readsPackets() {
			if scan.video.Packets == 0 {
				return probe, sourceDamaged(fmt.Errorf("%w: the source's video has no packets to convert", ErrSourceInvalid))
			}
			if err := decodeVideoHead(ctx, w.ffprobePath, path, req.video, &scan.video); err != nil {
				return probe, err
			}
		}
		probe.videoScan = &scan.video
	}
	if req.audio != nil {
		probe.audioSpans = nonNil(scan.audio)
		if err := convertedAudioMeasurable(plan, probe); err != nil {
			return probe, err
		}
	}
	if req.subtitles != nil {
		probe.subtitlePackets = nonNil(scan.subtitles)
		if w.readsPackets() {
			if err := w.countSubtitleEvents(ctx, plan, probe, path); err != nil {
				return probe, err
			}
		}
	}
	return probe, nil
}

// nonNil is m, or an empty map: a stream measured with no packets is still
// measured.
func nonNil[V any](m map[int]V) map[int]V {
	if m == nil {
		return map[int]V{}
	}
	return m
}

// countSubtitleEvents counts, before the encode, the lines each converted
// subtitle track comes out with: the conversion runs alone, writing its
// packets nowhere but to a count, through the same decoder and encoder the
// encode uses. The packets cannot tell it (a mov_text sample holding only
// spaces or a style box writes no line), and the encode's own output is what
// the count is held to, so only a run of the conversion is exact.
func (w *Worker) countSubtitleEvents(ctx context.Context, plan *transcode.Plan, probe SourceProbe, path string) error {
	type converted struct {
		index int
		codec string
	}
	var tracks []converted
	for _, s := range plannedStreams(plan, probe.Streams, "subtitle") {
		act, ok := subtitleActionFor(plan, s)
		if ok && norm(act.Operation) != "copy" {
			tracks = append(tracks, converted{s.Index, act.Codec})
		}
	}
	if len(tracks) == 0 {
		return nil
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].index < tracks[j].index })
	args := []string{"-nostdin", "-v", "error", "-i", path}
	for k, t := range tracks {
		args = append(args, "-map", fmt.Sprintf("0:%d", t.index), fmt.Sprintf("-c:s:%d", k), t.codec)
	}
	args = append(args, "-f", "framecrc", "-")
	cmd := exec.CommandContext(ctx, w.ffmpegPath, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runCharged(ctx, cmd); err != nil {
		return fmt.Errorf("counting the converted subtitles' lines failed: %w (stderr: %s)", err, truncateForLog(strings.TrimSpace(stderr.String()), 200))
	}
	counts := make([]int64, len(tracks))
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		field, _, _ := strings.Cut(line, ",")
		k, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || k < 0 || k >= len(tracks) {
			return fmt.Errorf("counting the converted subtitles' lines: unexpected output %q", truncateForLog(line, 120))
		}
		counts[k]++
	}
	for k, t := range tracks {
		c := probe.subtitlePackets[t.index]
		c.events, c.counted = counts[k], true
		probe.subtitlePackets[t.index] = c
	}
	return nil
}

// validateSubtitlePackets holds each kept subtitle's packets, as scanned with
// the candidate's other tracks, to the source's (see subtitleEvents), counted
// before the encode. The source is never read again after the encode.
func validateSubtitlePackets(plan *transcode.Plan, srcProbe SourceProbe, candProbe SourceProbe, cand map[int]subtitlePackets) error {
	if srcProbe.subtitlePackets == nil {
		return candidateInvalid(fmt.Errorf("%w: the source's subtitle packets were not counted before the encode (fail closed, never publish)", ErrContentNotPreserved))
	}
	return subtitleEvents(plan, srcProbe.Streams, candProbe.Streams, srcProbe.subtitlePackets, cand)
}

// subtitleEvents holds every kept subtitle to the source track's lines. A
// copied track carries every packet unchanged; only those its container marks
// to be discarded or that carry no time may be left out. A converted track
// has exactly the lines the conversion wrote when it was run before the
// encode (countSubtitleEvents): a track with none to count is refused, never
// passed. A track that should have lines never comes out with none.
func subtitleEvents(plan *transcode.Plan, src, cand []SourceStream, srcPackets, candPackets map[int]subtitlePackets) error {
	srcSubs, candSubs := plannedStreams(plan, src, "subtitle"), streamsOfKind(cand, "subtitle")
	if len(srcSubs) != len(candSubs) {
		return candidateInvalid(fmt.Errorf("%w: local candidate subtitle count %d != %s %d (fail closed, never publish)", ErrContentNotPreserved, len(candSubs), plannedLabel(plan), len(srcSubs)))
	}
	for i := range srcSubs {
		a, b := srcSubs[i], candSubs[i]
		act, ok := subtitleActionFor(plan, a)
		if !ok {
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d has no planned action (fail closed, never publish)", ErrContentNotPreserved, i))
		}
		x, y := srcPackets[a.Index], candPackets[b.Index]
		converted := norm(act.Operation) != "copy"
		least, most := x.packets-x.discarded-x.untimed, x.packets
		if converted {
			if !x.counted {
				return candidateInvalid(fmt.Errorf("%w: converted subtitle stream %d had its lines counted by no conversion before the encode (fail closed, never publish)", ErrContentNotPreserved, i))
			}
			least, most = x.events, x.events
		}
		if least > 0 && y.packets == 0 {
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d came out empty; the source track has %d lines (fail closed, never publish)", ErrContentNotPreserved, i, least))
		}
		if y.packets < least || y.packets > most {
			what, want := "copied", strconv.FormatInt(most, 10)
			if converted {
				what = "converted"
			} else if least != most {
				want = fmt.Sprintf("%d to %d", least, most)
			}
			return candidateInvalid(fmt.Errorf("%w: %s subtitle stream %d has %d lines, the source track %s; lines were lost or added (fail closed, never publish)", ErrContentNotPreserved, what, i, y.packets, want))
		}
	}
	return nil
}

// subtitleActionFor is the plan's action for a source subtitle. A legacy
// worker-shim plan carries none; its actions are derived as the execution
// plan derived them.
func subtitleActionFor(plan *transcode.Plan, s SourceStream) (transcode.SubtitleAction, bool) {
	for _, a := range plan.SubtitleActions {
		if a.TypeIndex == s.TypeIndex {
			return a, true
		}
	}
	if plan.RecipeVersion == "legacy-worker-shim" {
		return legacyHevcVTSubtitleAction(s)
	}
	return transcode.SubtitleAction{}, false
}

// attachmentsPreserved holds the candidate's attachments (fonts, mostly) to
// the source's by the rule the coordinator's check uses too
// (transcode.AttachmentChange): as many, and every source file among them by
// name, type and size.
func attachmentsPreserved(src, cand []SourceStream) error {
	files := func(streams []SourceStream) []transcode.AttachedFile {
		var out []transcode.AttachedFile
		for _, s := range streamsOfKind(streams, "attachment") {
			out = append(out, transcode.AttachedFile{Name: s.Filename, MimeType: s.MimeType, Size: s.AttachmentBytes})
		}
		return out
	}
	if change := transcode.AttachmentChange(files(src), files(cand)); change != "" {
		return candidateInvalid(fmt.Errorf("%w: %s (fail closed, never publish)", ErrContentNotPreserved, change))
	}
	return nil
}
