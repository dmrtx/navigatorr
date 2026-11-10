package transcodeworker

import (
	"context"
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/transcode"
)

// This file holds the candidate checks that read packets, not the probe's
// stream list: the programme video's frames, and every kept subtitle's
// events. The probe says a track is there; only its packets say it is whole.

// videoCompleteness holds the candidate's programme video to the source's
// packet scan. The encode passes every source picture through with its own
// timestamp (sourceTimestampArgs), so the candidate has as many pictures as
// the source, ending where the source's end. The container's duration cannot
// tell: it is the longest stream's, so a video that stops early hides behind
// audio that runs to the end.
//
// Only the packets a decoder shows no picture for may be missing: those at
// the start it drops (before the first keyframe, or an open picture group's
// leading pictures whose references were cut away; see startDropped) and
// those the container marks to be discarded, each packet counted once.
// Without them the candidate starts later, so its span may be shorter by as
// many pictures; it is never longer than the source's by more than one
// picture.
//
// A field-coded source (PAFF, from a transport stream) may carry a field in a
// packet of its own, and the candidate has one packet per picture: its
// pictures are counted as a range (see sourcePictures). fps is the source
// video's frame rate as the probe states it (0 when unknown).
func videoCompleteness(src, cand sourceVideoScan, fps float64) error {
	if src.Packets == 0 {
		// Nothing was scanned (a test that injects the probe only).
		return nil
	}
	if cand.Packets == 0 {
		return candidateInvalid(fmt.Errorf("%w: the converted video has no frames; the source has %d (fail closed, never publish)", ErrContentNotPreserved, src.Packets))
	}
	if cand.DemuxErrors > 0 {
		return candidateInvalid(fmt.Errorf("%w: the converted video is damaged: reading it reported %d error(s) (%s) (fail closed, never publish)", ErrContentNotPreserved, cand.DemuxErrors, truncateForLog(cand.FirstError, 200)))
	}
	dropped, droppedDiscarded := src.startDropped()
	if !src.sawKey && src.LeadingNonKey == src.Packets {
		// No packet is flagged a keyframe: nothing tells which ones a
		// decoder skips, so none may be missing but the discarded ones.
		dropped, droppedDiscarded = 0, 0
	}
	// A dropped packet that is also discarded is one packet a decoder
	// skips, not two.
	skippable := dropped + src.Discarded - droppedDiscarded
	p := sourcePictures(src, cand, fps)
	if cand.Packets > p.most || cand.Packets < p.least-skippable {
		source := fmt.Sprintf("the source %d", p.least)
		switch {
		case p.fields && cand.Packets > p.most:
			source = fmt.Sprintf("the field-coded source at most %d", p.most)
		case p.fields:
			source = fmt.Sprintf("the field-coded source at least %d", p.least)
		}
		return candidateInvalid(fmt.Errorf("%w: the converted video has %d frames, %s; frames were lost or added in the conversion (fail closed, never publish)", ErrContentNotPreserved, cand.Packets, source))
	}
	if src.Discontinuous || cand.Discontinuous {
		// A timeline that restarts has no length to compare; the frames do.
		return nil
	}
	frame := p.frame
	if frame <= 0 {
		frame = src.Seconds() / float64(p.least)
	}
	// A millisecond of slack absorbs the containers' timestamp rounding.
	const rounding = 0.001
	// The pictures the candidate may lack at its start: those it is short of
	// the source's count or, for a field-coded source, the skippable packets.
	missing := float64(skippable)
	if !p.fields {
		missing = float64(src.Packets - cand.Packets)
	}
	short := src.Seconds() - cand.Seconds()
	if short > frame*(1+missing)+rounding || -short > frame+rounding {
		return candidateInvalid(fmt.Errorf("%w: the converted video ends %.3fs after its first picture, the source's %.3fs; its last frame is not where the source's is (fail closed, never publish)", ErrContentNotPreserved, cand.Seconds(), src.Seconds()))
	}
	return nil
}

// validateVideoCompleteness holds the candidate's programme video, as scanned
// with its other tracks, to the source's scan taken before the encode. The
// source is never read again after the encode: without that scan (a test that
// injects the probe only) nothing is compared.
func validateVideoCompleteness(srcProbe SourceProbe, cand sourceVideoScan) error {
	if srcProbe.videoScan == nil || srcProbe.videoScan.Packets == 0 {
		return nil
	}
	srcIndex := programmeVideoIndex(srcProbe.Streams)
	var fps float64
	for _, s := range srcProbe.Streams {
		if s.Index == srcIndex {
			fps = s.FPS
		}
	}
	return videoCompleteness(*srcProbe.videoScan, cand, fps)
}

// fieldTolerance is how far, as a fraction of a frame, a packet must fall
// short of a whole frame to count as a possible field.
const fieldTolerance = 0.15

// pictureCount is how many pictures the source's packets show.
type pictureCount struct {
	// least and most bound the pictures; they are the packet count unless
	// some packets may be fields.
	least, most int
	// fields is set when some packets may be fields.
	fields bool
	// frame is one picture's duration, for the end check.
	frame float64
}

// sourcePictures counts the source's pictures from its packets. A packet at
// least a frame long (a frame-coded picture, or a pulled-down one three
// fields long) is one picture. A shorter one may be a field: two make one
// picture, or one alone does, so the pictures lie between the whole packets
// plus half the possible fields, rounded up, and every packet. The frame is
// the longer of 1/fps from the probe and the candidate's median packet (one
// per picture), so a probe that states the field rate cannot make a field
// look like a picture. Packets whose length the container does not state are
// possible fields only when some packet with a length is one.
func sourcePictures(src, cand sourceVideoScan, fps float64) pictureCount {
	out := pictureCount{least: src.Packets, most: src.Packets, frame: src.LastFrame}
	var frame float64
	if fps > 0 {
		frame = 1 / fps
	}
	frame = math.Max(frame, cand.medianDuration())
	if frame <= 0 {
		return out
	}
	possible, unknown := 0, 0
	for d, n := range src.durations {
		switch {
		case d <= 0:
			unknown += n
		case float64(d)/1e6 < frame*(1-fieldTolerance):
			possible += n
		}
	}
	if possible == 0 {
		return out
	}
	possible += unknown
	out.least = src.Packets - possible + (possible+1)/2
	out.fields, out.frame = true, frame
	return out
}

// subtitlePackets is one subtitle stream's packets: how many, and how many
// hold more than two bytes. A mov_text sample of two bytes is its text length
// alone, zero: an empty sample the MP4 muxer writes to fill the gap between
// two lines, which a conversion drops.
type subtitlePackets struct {
	packets int64
	text    int64
}

// movTextEmptySampleBytes is the size of a mov_text sample with no text.
const movTextEmptySampleBytes = 2

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
// packets cannot fail a job that leaves it out.
func (w *Worker) packetScanFor(plan *transcode.Plan, src SourceProbe) packetScanRequest {
	req := packetScanRequest{video: programmeVideoIndex(src.Streams)}
	if w.measuresAudioPackets(plan, src) {
		req.audio = streamIndexes(plannedStreams(plan, src.Streams, "audio"))
	}
	if w.measuresSubtitlePackets(plan, src) {
		req.subtitles = streamIndexes(plannedStreams(plan, src.Streams, "subtitle"))
	}
	return req
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
// source again.
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
			if err := decodeVideoHead(ctx, w.ffprobePath, path, req.video, &scan.video); err != nil {
				return probe, err
			}
		}
		probe.videoScan = &scan.video
	}
	if req.audio != nil {
		probe.audioSpans = nonNil(scan.audio)
	}
	if req.subtitles != nil {
		probe.subtitlePackets = nonNil(scan.subtitles)
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

// validateSubtitlePackets holds each kept subtitle's packets, as scanned with
// the candidate's other tracks, to the source's (see subtitleEvents), counted
// before the encode. The source is never read again after the encode: without
// that count nothing is compared.
func validateSubtitlePackets(plan *transcode.Plan, srcProbe SourceProbe, candProbe SourceProbe, cand map[int]subtitlePackets) error {
	if srcProbe.subtitlePackets == nil || cand == nil {
		return nil
	}
	return subtitleEvents(plan, srcProbe.Streams, candProbe.Streams, srcProbe.subtitlePackets, cand)
}

// subtitleEvents holds every kept subtitle to the source track's events. A
// copied track carries every packet unchanged, so the counts are equal. A
// converted track (mov_text to SubRip, the default recipe's one conversion)
// has one packet per line of text: the source's empty gap samples have no
// counterpart, so the candidate's packets equal the source's samples that hold
// text. Any other conversion has no exact count and is held only to being
// non-empty. A track whose source had packets never comes out with none.
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
		want := x.packets
		if norm(act.Operation) != "copy" && norm(a.Codec) == "mov_text" {
			want = x.text
		}
		if x.packets > 0 && y.packets == 0 {
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d came out empty; the source track has %d lines (fail closed, never publish)", ErrContentNotPreserved, i, want))
		}
		converted := norm(act.Operation) != "copy"
		if converted && norm(a.Codec) != "mov_text" {
			// A conversion a recipe may add has no known packet-per-line
			// rule (formats merge or split events): only an empty track
			// says lines were lost.
			continue
		}
		if y.packets != want {
			what := "copied"
			if converted {
				what = "converted"
			}
			return candidateInvalid(fmt.Errorf("%w: %s subtitle stream %d has %d lines, the source track %d; lines were lost or added (fail closed, never publish)", ErrContentNotPreserved, what, i, y.packets, want))
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

// attachmentKey identifies an attachment by what it carries: its name, its
// type and its size.
type attachmentKey struct {
	filename, mimetype string
	size               int64
}

// attachmentsPreserved holds the candidate's attachments (fonts, mostly) to
// the source's: as many, and every source file among them by name, type and
// size, however many of each. A source without attachments has none to lose
// and is not checked. A cover the encode re-attaches (covers) is the cover
// art check's: should the probe show it as an attachment rather than as a
// picture, it is not counted as one.
func attachmentsPreserved(covers []StreamAction, src, cand []SourceStream) error {
	sa := streamsOfKind(src, "attachment")
	if len(sa) == 0 {
		return nil
	}
	reattached := map[[2]string]int{}
	for _, c := range covers {
		reattached[[2]string{c.Filename, norm(c.MimeType)}]++
	}
	var ca []SourceStream
	for _, s := range streamsOfKind(cand, "attachment") {
		if k := [2]string{s.Filename, norm(s.MimeType)}; reattached[k] > 0 {
			reattached[k]--
			continue
		}
		ca = append(ca, s)
	}
	if len(ca) != len(sa) {
		return candidateInvalid(fmt.Errorf("%w: local candidate attachment count %d != source %d (fail closed, never publish)", ErrContentNotPreserved, len(ca), len(sa)))
	}
	count := map[attachmentKey]int{}
	for _, s := range ca {
		count[attachmentKey{s.Filename, norm(s.MimeType), s.AttachmentBytes}]++
	}
	// The first source file missing is reported, in file order.
	for _, s := range sa {
		k := attachmentKey{s.Filename, norm(s.MimeType), s.AttachmentBytes}
		if count[k] == 0 {
			return candidateInvalid(fmt.Errorf("%w: attachment %q (%s, %d bytes) is missing from the converted file (fail closed, never publish)", ErrContentNotPreserved, k.filename, k.mimetype, k.size))
		}
		count[k]--
	}
	return nil
}
