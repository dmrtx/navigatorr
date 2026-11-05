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
var PacketDamagePattern = regexp.MustCompile(`(?i)\bpos(?:ition)?[: ]+\d+|EBML|corrupt|partial file|truncated`)

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

// DemuxDamage is the packet damage the container's demuxer reported while
// every packet of one stream was read.
type DemuxDamage struct {
	// Packets is how many packets of the stream were read.
	Packets int
	// Errors counts the damage reports; First is the first of them.
	Errors int
	First  string
}

// ScanDemuxDamage reads every packet of stream streamIndex without decoding
// it and reports the damage the demuxer found, as the worker's pre-encode scan
// does. A healthy file reads with no error at all. A failure to read (ffprobe
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
		"-show_entries", "packet=pts_time:format=format_name", "-of", "compact", path)
	cmd.WaitDelay = probeWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return damage, &ProbeError{Kind: ProbeUnavailable, Err: err}
	}
	// The format's name is printed last, so every context's error count is
	// kept until the run ends.
	type tagErrors struct {
		count int
		first string
	}
	byTag := map[string]*tagErrors{}
	var tail []string
	cmd.Stderr = &lineSink{line: func(line string) {
		tag, text, ok := DemuxerTag(line)
		if !ok {
			// ffprobe ends with the reason it stopped.
			if tail = append(tail, line); len(tail) > 5 {
				tail = tail[1:]
			}
			return
		}
		if !PacketDamagePattern.MatchString(text) {
			return
		}
		e := byTag[tag]
		if e == nil {
			e = &tagErrors{first: "[" + tag + "] " + text}
			byTag[tag] = e
		}
		e.count++
	}}
	if err := cmd.Start(); err != nil {
		return damage, &ProbeError{Kind: ProbeUnavailable, Err: err}
	}
	var formatName string
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, readErr := reader.ReadString('\n')
		switch {
		case strings.HasPrefix(line, "packet|"):
			damage.Packets++
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
	if err := ctx.Err(); err != nil {
		return damage, err
	}
	if waitErr != nil {
		return damage, fmt.Errorf("ffprobe packet scan failed: %w (stderr: %s)", waitErr, strings.Join(tail, "; "))
	}
	if formatName == "" {
		return damage, errors.New("ffprobe packet scan printed no format name")
	}
	if e := byTag[formatName]; e != nil {
		damage.Errors, damage.First = e.count, e.first
	}
	return damage, nil
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
