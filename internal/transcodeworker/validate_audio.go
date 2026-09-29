package transcodeworker

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

type audioPacketSpan struct {
	start, end float64
	bytes      int64
}

// Read actual packet timing/size, not inherited Matroska DURATION/BPS tags.
// Streaming output keeps memory bounded even for feature-length files.
func audioPacketSpans(ctx context.Context, ffprobe, path string) (map[int]audioPacketSpan, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "a", "-show_packets", "-show_entries", "packet=stream_index,pts_time,duration_time,size", "-of", "compact=p=0", path)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = newTailBuffer(4096)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out := map[int]audioPacketSpan{}
	scanner := bufio.NewScanner(pipe)
	var parseErr error
	for scanner.Scan() {
		fields := map[string]string{}
		for _, part := range strings.Split(scanner.Text(), "|") {
			k, v, ok := strings.Cut(part, "=")
			if ok {
				fields[k] = v
			}
		}
		if fields["stream_index"] == "" {
			continue
		}
		index, e1 := strconv.Atoi(fields["stream_index"])
		pts, e2 := strconv.ParseFloat(fields["pts_time"], 64)
		duration, e3 := strconv.ParseFloat(fields["duration_time"], 64)
		size, e4 := strconv.ParseInt(fields["size"], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || !isFiniteQuality(pts) || !isFiniteQuality(duration) || duration <= 0 || size <= 0 || index < 0 || len(out) > 128 {
			parseErr = fmt.Errorf("audio packet timing/size unavailable")
			break
		}
		span, exists := out[index]
		if !exists {
			span.start, span.end = pts, pts+duration
		} else {
			span.start = math.Min(span.start, pts)
			span.end = math.Max(span.end, pts+duration)
		}
		span.bytes += size
		out[index] = span
	}
	if parseErr == nil {
		parseErr = scanner.Err()
	}
	if parseErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("audio packet probe failed: %w", waitErr)
	}
	return out, nil
}

func (w *Worker) validateAudioPackets(ctx context.Context, plan *transcode.Plan, source, candidate string, src, dst SourceProbe) error {
	a, err := audioPacketSpans(ctx, w.ffprobePath, source)
	if err != nil {
		return fmt.Errorf("source audio validation: %w", err)
	}
	b, err := audioPacketSpans(ctx, w.ffprobePath, candidate)
	if err != nil {
		return fmt.Errorf("candidate audio validation: %w", err)
	}
	sv, cv := firstStreamOfKind(src.Streams, "video"), firstStreamOfKind(dst.Streams, "video")
	if sv == nil || cv == nil || sv.StartTime == nil || cv.StartTime == nil {
		return fmt.Errorf("audio/video start timing unavailable (fail closed, never publish)")
	}
	sa, ca := streamsOfKind(src.Streams, "audio"), streamsOfKind(dst.Streams, "audio")
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
		if codec, kbps := transcode.AudioTarget(plan.AudioMode, s.Codec, s.Channels); codec != "copy" && y.end-y.start > 10 {
			bps := float64(y.bytes) * 8 / (y.end - y.start)
			// AAC is variable bitrate; reject gross overshoot, not silence or padding.
			if bps > float64(kbps*2000+16000) {
				return fmt.Errorf("compact audio stream %d bitrate %.0f exceeds nominal target tolerance", i, bps)
			}
		}
	}
	return nil
}
