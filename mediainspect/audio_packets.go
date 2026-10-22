package mediainspect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// AudioPacketSpan is one audio stream's measured packets: the time from its
// first packet's start to its last packet's end, and the bytes in between.
type AudioPacketSpan struct {
	Start, End float64
	Bytes      int64
}

// Seconds is the span's duration.
func (s AudioPacketSpan) Seconds() float64 { return s.End - s.Start }

// BitsPerSecond is the stream's measured average bitrate, or zero for a span
// with no duration.
func (s AudioPacketSpan) BitsPerSecond() float64 {
	if d := s.Seconds(); d > 0 {
		return float64(s.Bytes) * 8 / d
	}
	return 0
}

// audioPacketStallTimeout bounds how long the packet scan may go without
// reading another packet. A whole-file scan has no fixed deadline (its length
// follows the file's), but a stalled network read must not hang the caller.
// It is a variable only so tests can shrink it.
var audioPacketStallTimeout = 2 * time.Minute

// SetAudioPacketStallTimeoutForTest shrinks the packet scan's stall timeout
// so another package's tests can exercise a stalled scan, and returns the
// function that restores it. It is for tests only; it is not safe to call
// while a scan is running.
func SetAudioPacketStallTimeoutForTest(d time.Duration) (restore func()) {
	old := audioPacketStallTimeout
	audioPacketStallTimeout = d
	return func() { audioPacketStallTimeout = old }
}

// MeasureAudioPackets measures every audio stream's packets, keyed by stream
// index. It reads what the bitstream holds, never the container's statistics
// tags (BPS, NUMBER_OF_BYTES, DURATION), which a muxer can copy from another
// file. It is the worker's pre-publish packet check
// (internal/transcodeworker/validate_audio.go) in the same terms: a packet
// with no duration (TrueHD in Matroska) ends where the next one starts, so
// the last one is extended by the last positive gap between packets.
//
// A failure is a *ProbeError (the file was not measured); a caller
// cancellation comes back as the context error.
func MeasureAudioPackets(ctx context.Context, ffprobePath, path string) (map[int]AudioPacketSpan, error) {
	start := time.Now()
	stallTimeout := audioPacketStallTimeout
	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	if ffprobePath == "" {
		return nil, &ProbeError{Kind: ProbeUnavailable, Err: errors.New("ffprobe not found")}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ffprobe not started: %w", err)
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(pctx, ffprobePath, "-v", "error", "-select_streams", "a", "-show_packets", "-show_entries", "packet=stream_index,pts_time,duration_time,size", "-of", "compact=p=0", path)
	cmd.WaitDelay = probeWaitDelay
	stderr := &cappedBuffer{limit: probeStderrCap}
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &ProbeError{Kind: ProbeUnavailable, Err: err, Elapsed: time.Since(start)}
	}
	if err := cmd.Start(); err != nil {
		return nil, &ProbeError{Kind: ProbeUnavailable, Err: err, Elapsed: time.Since(start)}
	}

	// The watchdog kills a scan that stopped reading packets.
	var progress atomic.Int64
	progress.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		tick := time.NewTicker(stallTimeout / 4)
		defer tick.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-pctx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, progress.Load())) > stallTimeout {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	type span struct {
		AudioPacketSpan
		lastPTS, gap float64
		openEnd      bool
	}
	spans := map[int]*span{}
	var parseErr error
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		progress.Store(time.Now().UnixNano())
		fields := map[string]string{}
		for _, part := range strings.Split(scanner.Text(), "|") {
			if k, v, ok := strings.Cut(part, "="); ok {
				fields[k] = v
			}
		}
		if fields["stream_index"] == "" {
			continue
		}
		index, e1 := strconv.Atoi(fields["stream_index"])
		pts, e2 := strconv.ParseFloat(fields["pts_time"], 64)
		duration, e3 := 0.0, error(nil)
		if fields["duration_time"] != "N/A" {
			duration, e3 = strconv.ParseFloat(fields["duration_time"], 64)
			if e3 == nil && (!finite(duration) || duration <= 0) {
				e3 = errors.New("invalid duration")
			}
		}
		size, e4 := strconv.ParseInt(fields["size"], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || !finite(pts) || size <= 0 || index < 0 || len(spans) > 128 {
			parseErr = errors.New("audio packet timing/size unavailable")
			break
		}
		s, exists := spans[index]
		if !exists {
			s = &span{AudioPacketSpan: AudioPacketSpan{Start: pts, End: pts}}
			spans[index] = s
		} else if gap := pts - s.lastPTS; gap > 0 {
			s.gap = gap
		}
		s.Start = math.Min(s.Start, pts)
		if end := pts + duration; end > s.End || (end == s.End && duration == 0) {
			s.End, s.openEnd = end, duration == 0
		}
		s.lastPTS = pts
		s.Bytes += size
	}
	if parseErr == nil {
		parseErr = scanner.Err()
	}
	out := make(map[int]AudioPacketSpan, len(spans))
	if parseErr == nil {
		for index, s := range spans {
			if s.openEnd {
				if s.gap <= 0 {
					parseErr = errors.New("audio packet timing/size unavailable")
					break
				}
				s.End += s.gap
			}
			out[index] = s.AudioPacketSpan
		}
	}
	if parseErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(start)
	if parent := ctx.Err(); parent != nil {
		return nil, fmt.Errorf("audio packet scan interrupted: %w", parent)
	}
	if stalled.Load() {
		return nil, &ProbeError{Kind: ProbeTimeout, Err: fmt.Errorf("no audio packet read for %s", stallTimeout), Stderr: stderr.String(), Elapsed: elapsed}
	}
	if parseErr != nil {
		return nil, &ProbeError{Kind: ProbeBadOutput, Err: parseErr, Stderr: stderr.String(), Elapsed: elapsed}
	}
	if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return nil, &ProbeError{Kind: ProbeToolExit, Err: waitErr, Stderr: stderr.String(), Elapsed: elapsed}
	}
	return out, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
