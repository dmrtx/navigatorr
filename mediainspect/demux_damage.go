package mediainspect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// PacketDamagePattern is what a demuxer error says when the bytes it reads
// are damaged. A demuxer also reports, at error level, header or metadata it
// does not understand in files that play fine (a chapter track it cannot
// find, a master element longer than its parent); those name no position in
// the stream and are not damage. The worker's source scan and the
// coordinator's re-read of a file the worker called damaged both use it, so
// the two cannot disagree about what damage is.
var PacketDamagePattern = regexp.MustCompile(`(?i)\bpos(?:ition)?\.?[: ]+\d+|EBML|corrupt|partial file|truncated`)

// ReadFaultPattern is what a demuxer says when its read of the file failed
// ("Read error at pos. 4021": the I/O layer returned an error, as a dropped SMB
// session does), as opposed to bytes it read and found wrong ("File ended
// prematurely at pos. 4021" is a truncated file). A read fault names a
// position too, but it is no evidence about the file: the scan stopped where
// the read did, so it judged nothing, and it is tested before
// PacketDamagePattern.
var ReadFaultPattern = regexp.MustCompile(`(?i)\bread error\b`)

// DemuxerTag splits an ffmpeg log line "[matroska,webm @ 0x7f..] text" into
// its context name and text, dropping the context's address.
func DemuxerTag(line string) (tag, text string, ok bool) {
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

// DemuxDamage is what the container's demuxer reported while every packet of
// one stream was read.
type DemuxDamage struct {
	// Packets is how many packets of the stream were read.
	Packets int
	// Seconds is the stretch of the stream's timeline the packets covered, from
	// the earliest packet to the end of the latest. A read that stopped early
	// covers less than the stream lasts.
	Seconds float64
	// Errors counts the damage reports; First is the first of them.
	Errors int
	First  string
	// Unrecognised counts the other error lines the container's demuxer (or
	// no context at all) printed: not damage by PacketDamagePattern, but not
	// proof of a clean read either. ffprobe ends a read that fails mid-file
	// as if the file ended, with only such a line to say so (a read fault,
	// ReadFaultPattern, which is also counted here and never as damage).
	Unrecognised      int
	FirstUnrecognised string
}

// ScanDemuxDamage reads every packet of stream streamIndex without decoding
// it and reports what the demuxer said, as the worker's pre-encode scan does.
// A healthy file reads with no error at all. A failure to read (ffprobe
// missing, exiting with an error, cancelled) is returned as an error: the
// file was not judged either way.
func ScanDemuxDamage(ctx context.Context, ffprobePath, path string, streamIndex int) (DemuxDamage, error) {
	var damage DemuxDamage
	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	if ffprobePath == "" {
		return damage, &ProbeError{Kind: ProbeUnavailable, Err: errors.New("ffprobe not found")}
	}
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,duration_time:format=format_name", "-of", "compact", path)
	cmd.WaitDelay = probeWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return damage, &ProbeError{Kind: ProbeUnavailable, Err: err}
	}
	// The format's name is printed last, so every context's lines are kept
	// until the run ends.
	type tagLines struct {
		damaged, other           int
		firstDamaged, firstOther string
	}
	byTag := map[string]*tagLines{}
	var tail []string
	untagged, firstUntagged := 0, ""
	sink := &lineSink{line: func(line string) {
		if strings.TrimSpace(line) == "" {
			return
		}
		tag, text, ok := DemuxerTag(line)
		if !ok {
			// ffprobe ends with the reason it stopped.
			if tail = append(tail, line); len(tail) > 5 {
				tail = tail[1:]
			}
			if untagged++; untagged == 1 {
				firstUntagged = line
			}
			return
		}
		e := byTag[tag]
		if e == nil {
			e = &tagLines{}
			byTag[tag] = e
		}
		if !ReadFaultPattern.MatchString(text) && PacketDamagePattern.MatchString(text) {
			if e.damaged++; e.damaged == 1 {
				e.firstDamaged = "[" + tag + "] " + text
			}
			return
		}
		if e.other++; e.other == 1 {
			e.firstOther = "[" + tag + "] " + text
		}
	}}
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		return damage, &ProbeError{Kind: ProbeUnavailable, Err: err}
	}
	var formatName string
	var first, end float64
	timed := false
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, readErr := reader.ReadString('\n')
		switch {
		case strings.HasPrefix(line, "packet|"):
			damage.Packets++
			values := compactValues(line)
			if pts, ok := values["pts_time"]; ok {
				if !timed || pts < first {
					first = pts
				}
				if e := pts + values["duration_time"]; !timed || e > end {
					end = e
				}
				timed = true
			}
		case strings.HasPrefix(line, "format|"):
			for _, f := range strings.Split(strings.TrimSpace(line), "|")[1:] {
				if key, value, _ := strings.Cut(f, "="); key == "format_name" {
					formatName = value
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				_ = cmd.Process.Kill()
			}
			break
		}
	}
	waitErr := cmd.Wait()
	sink.flush()
	if err := ctx.Err(); err != nil {
		return damage, err
	}
	if waitErr != nil {
		return damage, fmt.Errorf("ffprobe packet scan failed: %w (stderr: %s)", waitErr, strings.Join(tail, "; "))
	}
	if formatName == "" {
		return damage, errors.New("ffprobe packet scan printed no format name")
	}
	if end > first {
		damage.Seconds = end - first
	}
	damage.Unrecognised, damage.FirstUnrecognised = untagged, firstUntagged
	if e := byTag[formatName]; e != nil {
		damage.Errors, damage.First = e.damaged, e.firstDamaged
		if e.other > 0 {
			if damage.Unrecognised == 0 {
				damage.FirstUnrecognised = e.firstOther
			}
			damage.Unrecognised += e.other
		}
	}
	return damage, nil
}

// compactValues reads the numeric key=value fields of one ffprobe compact line.
func compactValues(line string) map[string]float64 {
	values := map[string]float64{}
	for _, f := range strings.Split(strings.TrimSpace(line), "|")[1:] {
		if key, value, ok := strings.Cut(f, "="); ok {
			if n, err := strconv.ParseFloat(value, 64); err == nil {
				values[key] = n
			}
		}
	}
	return values
}

// lineSink hands a process's stderr to line one line at a time.
type lineSink struct {
	buf  []byte
	line func(string)
}

func (w *lineSink) Write(p []byte) (int, error) {
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

// flush hands over a last line that ended without a newline.
func (w *lineSink) flush() {
	if len(w.buf) > 0 {
		w.line(strings.TrimRight(string(w.buf), "\r"))
		w.buf = w.buf[:0]
	}
}
