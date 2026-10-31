package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// sourceVideoScan is one pass over the packets of the source's programme
// video, read before the encode. A damaged container used to surface only at
// the final gate, hours later, as a window whose extracts misaligned, and the
// job was then run again to fail the same way.
type sourceVideoScan struct {
	Packets int
	// Start and End bound the video's presentation timeline: the earliest
	// packet timestamp and the latest packet's end.
	Start, End float64
	// Discontinuous is set when the timestamps jump back by more than
	// timestampResetSeconds (a broadcast recording whose clock restarts):
	// Start and End then no longer measure the video's length.
	Discontinuous bool
	// DemuxErrors counts the packet damage the container's demuxer reported
	// while reading. FirstError is the first of it, and FirstErrorByte the
	// byte offset it names (-1 when it names none).
	DemuxErrors    int
	FirstError     string
	FirstErrorByte int64
}

// Seconds is the video's length from its first picture, the origin the final
// windows are placed from.
func (s sourceVideoScan) Seconds() float64 {
	if s.Packets == 0 {
		return 0
	}
	return s.End - s.Start
}

// errorBytePattern finds the byte offset a demuxer error names ("at pos
// 27835247", "pos: 1234").
var errorBytePattern = regexp.MustCompile(`\bpos(?:ition)?[: ]+(\d+)`)

// packetDamagePattern is what a demuxer error says when the bytes it reads
// are damaged. A demuxer also reports, at error level, header or metadata it
// does not understand in files that play fine (a chapter track it cannot
// find, a master element longer than its parent); those name no position in
// the stream and are not damage.
var packetDamagePattern = regexp.MustCompile(`(?i)\bpos(?:ition)?[: ]+\d+|EBML|corrupt|partial file|truncated`)

// timestampResetSeconds is how far a packet's timestamp may fall behind the
// latest end seen before the timeline counts as restarted. Reordered
// B-frames fall back by a fraction of a second.
const timestampResetSeconds = 10.0

// scanSourceVideo reads every packet of stream streamIndex without decoding
// it. Healthy files of every container the library holds read with no demuxer
// error at all; the errors of a damaged one name where the damage sits.
// Errors of the decoders ffprobe opens to find stream parameters are not the
// container's and are not counted.
func scanSourceVideo(ctx context.Context, ffprobePath, path string, streamIndex int) (sourceVideoScan, error) {
	scan := sourceVideoScan{FirstErrorByte: -1}
	first := true
	var formatName string
	var parseErr error
	stdout := &lineWriter{line: func(line string) {
		fields := strings.Split(line, "|")
		switch fields[0] {
		case "packet":
			var pts, dts, dur float64
			hasPTS, hasDTS := false, false
			for _, f := range fields[1:] {
				key, value, _ := strings.Cut(f, "=")
				v, err := strconv.ParseFloat(value, 64)
				if err != nil {
					continue // N/A
				}
				switch key {
				case "pts_time":
					pts, hasPTS = v, true
				case "dts_time":
					dts, hasDTS = v, true
				case "duration_time":
					dur = v
				}
			}
			if !hasPTS {
				// AVI and raw streams can carry decode times only.
				if !hasDTS {
					return
				}
				pts = dts
			}
			if !first && pts < scan.End-timestampResetSeconds {
				scan.Discontinuous = true
			}
			if first || pts < scan.Start {
				scan.Start = pts
			}
			if first || pts+dur > scan.End {
				scan.End = pts + dur
			}
			first = false
			scan.Packets++
		case "format":
			for _, f := range fields[1:] {
				if key, value, _ := strings.Cut(f, "="); key == "format_name" {
					formatName = value
				}
			}
		default:
			if parseErr == nil && strings.TrimSpace(line) != "" {
				parseErr = fmt.Errorf("unexpected ffprobe packet output %q", truncateForLog(line, 120))
			}
		}
	}}
	// The format's name is printed last, so every source's error counts are
	// kept until the run ends.
	type tagErrors struct {
		count int
		first string
	}
	byTag := map[string]*tagErrors{}
	var stderrTail []string
	stderr := &lineWriter{line: func(line string) {
		tag, text, ok := demuxerTag(line)
		if !ok {
			// The last lines: ffprobe ends with the reason it stopped.
			if stderrTail = append(stderrTail, line); len(stderrTail) > 5 {
				stderrTail = stderrTail[1:]
			}
			return
		}
		if !packetDamagePattern.MatchString(text) {
			return
		}
		e := byTag[tag]
		if e == nil {
			e = &tagErrors{first: "[" + tag + "] " + text}
			byTag[tag] = e
		}
		e.count++
	}}
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,dts_time,duration_time:format=format_name", "-of", "compact", path)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := runCharged(ctx, cmd)
	stdout.flush()
	stderr.flush()
	if err != nil {
		return scan, fmt.Errorf("ffprobe video packet scan failed: %w (stderr: %s)", err, strings.Join(stderrTail, "; "))
	}
	if parseErr != nil {
		return scan, parseErr
	}
	if formatName == "" {
		return scan, errors.New("ffprobe video packet scan printed no format name")
	}
	if e := byTag[formatName]; e != nil {
		scan.DemuxErrors, scan.FirstError = e.count, e.first
		if m := errorBytePattern.FindStringSubmatch(e.first); m != nil {
			scan.FirstErrorByte, _ = strconv.ParseInt(m[1], 10, 64)
		}
	}
	return scan, nil
}

// demuxerTag splits an ffmpeg log line "[matroska,webm @ 0x7f..] text" into
// its context name and text, dropping the context's address.
func demuxerTag(line string) (tag, text string, ok bool) {
	rest, found := strings.CutPrefix(line, "[")
	if !found {
		return "", "", false
	}
	inside, text, found := strings.Cut(rest, "]")
	if !found {
		return "", "", false
	}
	tag, _, _ = strings.Cut(inside, " @ ")
	return tag, strings.TrimSpace(text), tag != ""
}

// damage is the job's failure when the demuxer reported errors in the video.
// The class keeps every retry away: the bytes stay damaged.
func (s sourceVideoScan) damage(sizeBytes int64) error {
	if s.DemuxErrors == 0 {
		return nil
	}
	where := ""
	if s.FirstErrorByte >= 0 && sizeBytes > 0 && s.FirstErrorByte <= sizeBytes {
		where = fmt.Sprintf(", the first %d%% into the file", s.FirstErrorByte*100/sizeBytes)
	}
	noun := "errors"
	if s.DemuxErrors == 1 {
		noun = "error"
	}
	return sourceDamaged(fmt.Errorf("%s: the source file is damaged: reading its video the demuxer reported %d %s%s (%s); download or remux the file again, a retry fails the same way",
		resilience.FFmpegInputCorrupt, s.DemuxErrors, noun, where, truncateForLog(s.FirstError, 200)))
}

// finalWindowsFitVideo refuses final windows that run past the end of the
// video. A container can last longer than its video (a subtitle or attachment
// carries its duration); a window placed there has no pictures to extract,
// which the final gate only found out after the whole encode.
func finalWindowsFitVideo(plan *transcode.Plan, scan sourceVideoScan) error {
	if plan == nil || plan.QualityValidation == nil || scan.Packets == 0 || scan.Discontinuous {
		return nil
	}
	q := plan.QualityValidation
	video := scan.Seconds()
	for _, window := range append(append([]transcode.BenchmarkSampleWindow(nil), q.Samples...), q.FreshSamples...) {
		// Half a second of slack: a window a frame past the end is the
		// final gate's to judge, as before.
		if end := window.StartSeconds + window.DurationSeconds; end > video+0.5 {
			return &classedError{class: resilience.InternalInvariant, err: fmt.Errorf("quality_final_window_beyond_video: final window %d ends %.1fs after the first picture, but the video ends at %.1fs while the container runs on; the windows were planned on the container's length",
				window.Index, end, video)}
		}
	}
	return nil
}

// programmeVideoIndex is the stream index of the source's first programme
// video stream, or -1.
func programmeVideoIndex(streams []SourceStream) int {
	for _, s := range streams {
		if strings.EqualFold(strings.TrimSpace(s.Kind), "video") && !s.IsAttachedPicture() {
			return s.Index
		}
	}
	return -1
}

// lineWriter hands each complete line written to it to line. One goroutine
// writes to it (os/exec copies each pipe on its own).
type lineWriter struct {
	buf  []byte
	line func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	start := 0
	for i, b := range w.buf {
		if b == '\n' {
			w.line(strings.TrimRight(string(w.buf[start:i]), "\r"))
			start = i + 1
		}
	}
	w.buf = append(w.buf[:0], w.buf[start:]...)
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(strings.TrimRight(string(w.buf), "\r"))
		w.buf = w.buf[:0]
	}
}

// truncateForLog shortens s to at most n bytes of valid UTF-8.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
