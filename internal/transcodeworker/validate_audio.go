package transcodeworker

import (
	"context"
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// audioPacketSpan is one audio stream's measured packets, as
// mediainspect.AudioPacketAccumulator measures them.
type audioPacketSpan struct {
	start, end float64
	bytes      int64
	packets    int64
	// discarded counts the packets the container marks to be dropped.
	discarded int64
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
		out[index] = audioPacketSpan{start: s.Start, end: s.End, bytes: s.Bytes, packets: s.Packets, discarded: s.Discarded}
	}
	return out, nil
}

// validateAudioPackets holds each kept audio track's packets, as scanned with
// the candidate's other tracks (b), to the source's, measured before the
// encode. The source is never read again after the encode: without that
// measure (a test that injects the probe only) nothing is compared.
func validateAudioPackets(plan *transcode.Plan, src, dst SourceProbe, b map[int]audioPacketSpan) error {
	a := src.audioSpans
	if a == nil || b == nil {
		return nil
	}
	sv, cv := firstMainVideo(src.Streams), firstMainVideo(dst.Streams)
	if sv == nil || cv == nil || sv.StartTime == nil || cv.StartTime == nil {
		return fmt.Errorf("audio/video start timing unavailable (fail closed, never publish)")
	}
	sa, ca := plannedStreams(plan, src.Streams, "audio"), streamsOfKind(dst.Streams, "audio")
	if len(sa) != len(ca) {
		return fmt.Errorf("audio track count changed")
	}
	for i, s := range sa {
		x, xok := a[s.Index]
		y, yok := b[ca[i].Index]
		if !xok || !yok {
			return fmt.Errorf("audio stream %d has no measurable packets", i)
		}
		codec, kbps := transcode.AudioTarget(plan.AudioMode, s.AudioTrack())
		// Allow AAC priming/frame rounding; preserve existing source A/V
		// offset. A copied track's offset is not compared: its packets keep
		// their timestamps, and where the output's timeline starts (a
		// transport stream's video and audio start apart) is the muxer's
		// doing. Its packets are counted instead (copiedAudioPackets).
		offset := math.Abs((x.start - *sv.StartTime) - (y.start - *cv.StartTime))
		if (codec != "copy" && offset > 0.25) || math.Abs((x.end-x.start)-(y.end-y.start)) > 0.25 {
			return fmt.Errorf("audio stream %d duration or A/V offset changed (fail closed, never publish)", i)
		}
		if err := copiedAudioPackets(i, codec, x, y); err != nil {
			return err
		}
		if codec != "copy" && y.end-y.start > 10 {
			bps := float64(y.bytes) * 8 / (y.end - y.start)
			// AAC is variable bitrate; reject gross overshoot, not silence or padding.
			if bps > float64(transcode.AudioBitrateCeilingBps(kbps)) {
				return fmt.Errorf("compact audio stream %d bitrate %.0f exceeds nominal target tolerance", i, bps)
			}
		}
	}
	return nil
}

// copiedAudioPackets holds a copied track to the source's packet count. A copy
// carries every packet of the source track unchanged, but for those the
// source's container marks to be discarded (an edit list trimming the start),
// which the copy may leave out. One with fewer lost sound, however close its
// first and last packets stay to the source's.
func copiedAudioPackets(i int, codec string, src, cand audioPacketSpan) error {
	if codec != "copy" || (cand.packets <= src.packets && cand.packets >= src.packets-src.discarded) {
		return nil
	}
	return candidateInvalid(fmt.Errorf("%w: copied audio stream %d has %d packets, the source track %d; sound was lost or added in the copy (fail closed, never publish)", ErrContentNotPreserved, i, cand.packets, src.packets))
}
