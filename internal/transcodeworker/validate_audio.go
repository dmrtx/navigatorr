package transcodeworker

import (
	"context"
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// audioPacketSpan is one audio stream's measured packets, as
// mediainspect.AudioPacketAccumulator measures them: the span and bytes of
// its timed packets, and how many packets it has in all, of which timed, how
// many the container marks to be discarded and how many carry no time.
type audioPacketSpan struct {
	start, end                         float64
	bytes                              int64
	packets, timed, discarded, untimed int64
}

func audioSpanOf(s mediainspect.AudioPacketSpan) audioPacketSpan {
	return audioPacketSpan{start: s.Start, end: s.End, bytes: s.Bytes, packets: s.Packets, timed: s.Timed, discarded: s.Discarded, untimed: s.Untimed}
}

func (s audioPacketSpan) seconds() float64 { return s.end - s.start }

// packetSeconds is the average length of one of its timed packets (0 with
// none).
func (s audioPacketSpan) packetSeconds() float64 {
	if s.timed == 0 {
		return 0
	}
	return s.seconds() / float64(s.timed)
}

// needsAudioPacketValidation reports whether the plan re-encodes audio, so
// the candidate's audio timing and bitrate are measured against the source.
func needsAudioPacketValidation(plan *transcode.Plan, src SourceProbe) bool {
	switch plan.AudioMode {
	case "compact", "he_aac", transcode.AudioModeCompactLossless:
		return countStreams(src.Streams, "audio") > 0
	}
	return transcode.IsAudioLevelMode(plan.AudioMode) && countStreams(src.Streams, "audio") > 0
}

// measuresAudioPackets reports whether the candidate's audio tracks are
// measured from their packets. Every plan's are, copied tracks included: a
// copy that lost packets is as short as a truncated encode. Only a test that
// injects the probe without the packet scan describes files it never wrote,
// so plans that copy their audio are not measured there.
func (w *Worker) measuresAudioPackets(plan *transcode.Plan, src SourceProbe) bool {
	if needsAudioPacketValidation(plan, src) {
		return true
	}
	return countStreams(src.Streams, "audio") > 0 && (w.scanAudio != nil || !w.probeInjected())
}

// audioPacketSpansForJob uses the injected audio scan when present.
func (w *Worker) audioPacketSpansForJob(ctx context.Context, path string) (map[int]audioPacketSpan, error) {
	if w.scanAudio != nil {
		return w.scanAudio(ctx, path)
	}
	return audioPacketSpans(ctx, w.ffprobePath, path)
}

// audioPacketSpans reads actual packet timing/size, not inherited Matroska
// DURATION/BPS tags. The scan is mediainspect.MeasureAudioPackets, the one
// promotion runs too, so both measure the same bytes the same way. Its
// failures keep the worker's own wording and classification: a ProbeError
// would otherwise reclassify them as media_probe_failed.
func audioPacketSpans(ctx context.Context, ffprobe, path string) (map[int]audioPacketSpan, error) {
	measured, err := mediainspect.MeasureAudioPackets(ctx, ffprobe, path)
	if err != nil {
		pe, ok := mediainspect.AsProbeError(err)
		switch {
		case !ok:
			return nil, err
		case (pe.Kind == mediainspect.ProbeBadOutput || pe.Kind == mediainspect.ProbeUnavailable) && pe.Err != nil:
			return nil, pe.Err
		case pe.Kind == mediainspect.ProbeToolExit:
			return nil, fmt.Errorf("audio packet probe failed: %w", pe.Err)
		default:
			return nil, fmt.Errorf("audio packet probe failed: %s", pe.Error())
		}
	}
	out := make(map[int]audioPacketSpan, len(measured))
	for index, s := range measured {
		out[index] = audioSpanOf(s)
	}
	return out, nil
}

// convertedAudioMeasurable refuses, before the encode, a kept audio track the
// plan converts that the checks after it could not measure: one with no timed
// packet has no length to hold the conversion's to, and a source video with
// no start time leaves the conversion's offset unmeasured. A copied track is
// held to its packet count instead, which needs neither.
func convertedAudioMeasurable(plan *transcode.Plan, src SourceProbe) error {
	for i, s := range plannedStreams(plan, src.Streams, "audio") {
		if codec, _ := transcode.AudioTarget(plan.AudioMode, s.AudioTrack()); codec == "copy" {
			continue
		}
		if x := src.audioSpans[s.Index]; x.timed == 0 {
			return sourceDamaged(fmt.Errorf("%w: audio stream %d has %d packets and none with a time; a conversion of it could not be checked for lost sound", ErrSourceInvalid, i, x.packets))
		}
		if v := firstMainVideo(src.Streams); v == nil || v.StartTime == nil {
			return sourceDamaged(fmt.Errorf("%w: the source's video has no start time; a converted audio track's offset from it could not be checked", ErrSourceInvalid))
		}
	}
	return nil
}

// validateAudioPackets holds each kept audio track's packets, as scanned with
// the candidate's other tracks (b), to the source's, measured before the
// encode. The source is never read again after the encode. A copied track is
// held to its packet count and length; a converted one to its length, its
// offset from the video and its bitrate.
func validateAudioPackets(plan *transcode.Plan, src, dst SourceProbe, b map[int]audioPacketSpan) error {
	a := src.audioSpans
	if a == nil {
		return candidateInvalid(fmt.Errorf("%w: the source's audio packets were not measured before the encode (fail closed, never publish)", ErrContentNotPreserved))
	}
	sa, ca := plannedStreams(plan, src.Streams, "audio"), streamsOfKind(dst.Streams, "audio")
	if len(sa) != len(ca) {
		return candidateInvalid(fmt.Errorf("%w: the converted file has %d audio tracks, %s %d (fail closed, never publish)", ErrContentNotPreserved, len(ca), plannedLabel(plan), len(sa)))
	}
	for i, s := range sa {
		x, y := a[s.Index], b[ca[i].Index]
		codec, kbps := transcode.AudioTarget(plan.AudioMode, s.AudioTrack())
		var err error
		if codec == "copy" {
			err = copiedAudioPackets(i, x, y)
		} else {
			err = convertedAudioPackets(i, kbps, x, y, src, dst)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// audioLengthSlack is how far a track's length may move: AAC priming and
// frame rounding.
const audioLengthSlack = 0.25

// copiedAudioPackets holds a copied track to the source's packets. A copy
// carries every packet unchanged, but for those the source's container marks
// to be discarded (an edit list trimming the start), which the copy may leave
// out: one with fewer lost sound. Its length is compared when both sides have
// timed packets; a packet the source carries with no time or marks discarded
// is timed in the copy, so the copy may run longer by as many packets.
func copiedAudioPackets(i int, src, cand audioPacketSpan) error {
	if cand.packets > src.packets || cand.packets < src.packets-src.discarded {
		return candidateInvalid(fmt.Errorf("%w: copied audio stream %d has %d packets, the source track %d; sound was lost or added in the copy (fail closed, never publish)", ErrContentNotPreserved, i, cand.packets, src.packets))
	}
	if src.timed == 0 || cand.timed == 0 {
		return nil
	}
	return audioLength(i, "copied", src, cand, float64(src.untimed+src.discarded)*src.packetSeconds())
}

// convertedAudioPackets holds a converted track to the source's length and
// offset from the video, and its bitrate to the target's ceiling. Packets the
// source carries with no time are decoded too, so the conversion may run
// longer by as many packets.
func convertedAudioPackets(i, kbps int, src, cand audioPacketSpan, srcProbe, dst SourceProbe) error {
	if src.timed == 0 || cand.timed == 0 {
		return candidateInvalid(fmt.Errorf("%w: converted audio stream %d has %d timed packets, the source track %d; its length cannot be compared (fail closed, never publish)", ErrContentNotPreserved, i, cand.timed, src.timed))
	}
	if err := audioLength(i, "converted", src, cand, float64(src.untimed)*src.packetSeconds()); err != nil {
		return err
	}
	sv, cv := firstMainVideo(srcProbe.Streams), firstMainVideo(dst.Streams)
	if sv == nil || cv == nil || sv.StartTime == nil || cv.StartTime == nil {
		return candidateInvalid(fmt.Errorf("%w: converted audio stream %d: the video's start time is missing, so the audio's offset from it cannot be compared (fail closed, never publish)", ErrContentNotPreserved, i))
	}
	before, after := src.start-*sv.StartTime, cand.start-*cv.StartTime
	if math.Abs(before-after) > audioLengthSlack {
		return candidateInvalid(fmt.Errorf("%w: converted audio stream %d starts %.3fs from the video, the source track %.3fs; sound moved against the picture (fail closed, never publish)", ErrContentNotPreserved, i, after, before))
	}
	if d := cand.seconds(); d > 10 {
		bps := float64(cand.bytes) * 8 / d
		// AAC is variable bitrate; reject gross overshoot, not silence or padding.
		if ceiling := transcode.AudioBitrateCeilingBps(kbps); bps > float64(ceiling) {
			return candidateInvalid(fmt.Errorf("%w: converted audio stream %d averages %.0f b/s, over the %d b/s its %d kb/s target allows (fail closed, never publish)", ErrContentNotPreserved, i, bps, ceiling, kbps))
		}
	}
	return nil
}

// audioLength holds a track's length to the source's: never shorter by more
// than the slack, never longer by more than the slack and longer.
func audioLength(i int, what string, src, cand audioPacketSpan, longer float64) error {
	d := cand.seconds() - src.seconds()
	if d < -audioLengthSlack || d > audioLengthSlack+longer {
		return candidateInvalid(fmt.Errorf("%w: %s audio stream %d lasts %.3fs, the source track %.3fs; sound was lost or added (fail closed, never publish)", ErrContentNotPreserved, what, i, cand.seconds(), src.seconds()))
	}
	return nil
}
