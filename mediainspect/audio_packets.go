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
// first played packet's start to its last played packet's end, and the bytes
// those packets carry (Timed of them). A packet the container marks to be
// dropped (Discarded: an edit list that trims the track's start) is never
// played, and one with no time (Untimed) cannot be placed: both are counted
// in Packets and left out of the span and its bytes, so a copy that drops
// them measures as long as the source.
type AudioPacketSpan struct {
	Start, End float64
	Bytes      int64
	Packets    int64
	Timed      int64
	Discarded  int64
	Untimed    int64
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
// It is atomic only so tests can shrink it without racing a running scan.
var audioPacketStallTimeout atomic.Int64

func init() { audioPacketStallTimeout.Store(int64(2 * time.Minute)) }

// SetAudioPacketStallTimeoutForTest shrinks the packet scan's stall timeout
// so another package's tests can exercise a stalled scan, and returns the
// function that restores it. It is for tests only.
func SetAudioPacketStallTimeoutForTest(d time.Duration) (restore func()) {
	old := audioPacketStallTimeout.Swap(int64(d))
	return func() { audioPacketStallTimeout.Store(old) }
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
	stallTimeout := time.Duration(audioPacketStallTimeout.Load())
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
	cmd := exec.CommandContext(pctx, ffprobePath, "-v", "error", "-select_streams", "a", "-show_packets", "-show_entries", "packet=stream_index,pts_time,duration_time,size,flags", "-of", "compact=p=0", path)
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

	acc := &AudioPacketAccumulator{}
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
		if parseErr = acc.Add(fields); parseErr != nil {
			break
		}
	}
	if parseErr == nil {
		parseErr = scanner.Err()
	}
	var out map[int]AudioPacketSpan
	if parseErr == nil {
		out, parseErr = acc.Spans()
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

// AudioPacketAccumulator measures audio packets read in file order, keyed by
// stream index, as MeasureAudioPackets does. A caller that reads a file's
// packets for other reasons too feeds it the audio ones, so the file is read
// once.
type AudioPacketAccumulator struct {
	spans map[int]*audioSpanState
}

type audioSpanState struct {
	AudioPacketSpan
	lastPTS, gap float64
	openEnd      bool
}

// errAudioPacketTiming is every failure to measure a packet.
var errAudioPacketTiming = errors.New("audio packet timing/size unavailable")

// Add measures one packet from ffprobe's fields: stream_index, pts_time (N/A
// when the packet has no time), duration_time (N/A or 0 when the container
// states none), size and, when shown, flags. Only a field ffprobe could not
// have printed fails it.
func (a *AudioPacketAccumulator) Add(fields map[string]string) error {
	index, e1 := strconv.Atoi(fields["stream_index"])
	duration, e3 := 0.0, error(nil)
	if fields["duration_time"] != "N/A" {
		duration, e3 = strconv.ParseFloat(fields["duration_time"], 64)
		if e3 == nil && (!finite(duration) || duration < 0) {
			e3 = errors.New("invalid duration")
		}
	}
	size, e4 := strconv.ParseInt(fields["size"], 10, 64)
	pts, e2 := 0.0, error(nil)
	timed := fields["pts_time"] != "N/A"
	if timed {
		pts, e2 = strconv.ParseFloat(fields["pts_time"], 64)
	}
	if a.spans == nil {
		a.spans = map[int]*audioSpanState{}
	}
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || !finite(pts) || size < 0 || index < 0 || len(a.spans) > 128 {
		return errAudioPacketTiming
	}
	s, exists := a.spans[index]
	if !exists {
		s = &audioSpanState{}
		a.spans[index] = s
	}
	s.Packets++
	switch {
	case strings.Contains(fields["flags"], "D"):
		s.Discarded++
		return nil
	case !timed:
		s.Untimed++
		return nil
	}
	if s.Timed == 0 {
		s.Start, s.End = pts, pts
	} else if gap := pts - s.lastPTS; gap > 0 {
		s.gap = gap
	}
	s.Start = math.Min(s.Start, pts)
	if end := pts + duration; end > s.End || (end == s.End && duration == 0) {
		s.End, s.openEnd = end, duration == 0
	}
	s.lastPTS = pts
	s.Bytes += size
	s.Timed++
	return nil
}

// Spans is every measured stream's span. A packet with no duration (TrueHD
// in Matroska) ends where the next one starts, so a track that ends on one is
// extended by its last positive gap between packets; with no gap to go by
// (one timed packet) the span ends where that packet starts.
func (a *AudioPacketAccumulator) Spans() (map[int]AudioPacketSpan, error) {
	out := make(map[int]AudioPacketSpan, len(a.spans))
	for index, s := range a.spans {
		if s.openEnd {
			s.End += s.gap
			s.openEnd = false
		}
		out[index] = s.AudioPacketSpan
	}
	return out, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
