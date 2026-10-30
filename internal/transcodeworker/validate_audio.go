package transcodeworker

import (
	"context"
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// audioPacketSpan is one audio stream's measured packets, as
// mediainspect.MeasureAudioPackets measures them.
type audioPacketSpan struct {
	start, end float64
	bytes      int64
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
		out[index] = audioPacketSpan{start: s.Start, end: s.End, bytes: s.Bytes}
	}
	return out, nil
}

func (w *Worker) validateAudioPackets(ctx context.Context, plan *transcode.Plan, source, candidate string, src, dst SourceProbe) error {
	a := src.audioSpans
	if a == nil {
		var err error
		if a, err = audioPacketSpans(ctx, w.ffprobePath, source); err != nil {
			return fmt.Errorf("source audio validation: %w", err)
		}
	}
	b, err := audioPacketSpans(ctx, w.ffprobePath, candidate)
	if err != nil {
		return fmt.Errorf("candidate audio validation: %w", err)
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
		// Allow AAC priming/frame rounding; preserve existing source A/V offset.
		if math.Abs((x.start-*sv.StartTime)-(y.start-*cv.StartTime)) > 0.25 || math.Abs((x.end-x.start)-(y.end-y.start)) > 0.25 {
			return fmt.Errorf("audio stream %d duration or A/V offset changed (fail closed, never publish)", i)
		}
		if codec, kbps := transcode.AudioTarget(plan.AudioMode, s.AudioTrack()); codec != "copy" && y.end-y.start > 10 {
			bps := float64(y.bytes) * 8 / (y.end - y.start)
			// AAC is variable bitrate; reject gross overshoot, not silence or padding.
			if bps > float64(transcode.AudioBitrateCeilingBps(kbps)) {
				return fmt.Errorf("compact audio stream %d bitrate %.0f exceeds nominal target tolerance", i, bps)
			}
		}
	}
	return nil
}
