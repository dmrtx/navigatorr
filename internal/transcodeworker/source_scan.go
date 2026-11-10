package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
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
	// LeadingNonKey counts the packets read before the first keyframe: a
	// decoder has nothing to show them with, so an encode may begin without
	// them. Discarded counts the packets the container marks to be dropped
	// (an edit list that starts the video later), which no decoder shows.
	// LastFrame is the duration of the picture that ends the video.
	LeadingNonKey, Discarded int
	// leadDiscarded counts the packets that are both: a packet a decoder
	// skips for two reasons is still one packet.
	leadDiscarded int
	sawKey        bool
	LastFrame     float64
	// The head is the packets read before the second keyframe: the first
	// picture group with whatever precedes it. headDiscarded counts its
	// discarded packets. leadingPictures counts the packets after the first
	// keyframe in decode order that show before it: an open picture group's
	// leading pictures, which may refer to pictures before the file's start.
	// firstKeyPTS is the first keyframe's time, keys the keyframes seen.
	headPackets, headDiscarded, leadingPictures int
	firstKeyPTS                                 float64
	keys                                        int
	// headFrames is how many pictures a decoder shows from the head's
	// packets, when headDecoded (see decodeVideoHead).
	headFrames  int
	headDecoded bool
	// durations counts the packets by duration, in microseconds (0 when the
	// container states none): a field-coded source shows itself by packets
	// shorter than a frame.
	durations map[int64]int
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

// medianDuration is the median packet duration in seconds, or 0 when the scan
// kept none.
func (s sourceVideoScan) medianDuration() float64 {
	n := 0
	keys := make([]int64, 0, len(s.durations))
	for d, c := range s.durations {
		keys = append(keys, d)
		n += c
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	seen := 0
	for _, d := range keys {
		if seen += s.durations[d]; 2*seen >= n {
			return float64(d) / 1e6
		}
	}
	return 0
}

// errorBytePattern finds the byte offset a demuxer error names ("at pos
// 27835247", "pos: 1234", "File ended prematurely at pos. 4021").
var errorBytePattern = regexp.MustCompile(`\bpos(?:ition)?\.?[: ]+(\d+)`)

// packetDamagePattern is what a demuxer error says when the bytes it reads
// are damaged. It is shared with the coordinator, which reads a file this scan
// called damaged again before the job is left blaming the file.
var packetDamagePattern = mediainspect.PacketDamagePattern

// timestampResetSeconds is how far a packet's timestamp may fall behind the
// latest end seen before the timeline counts as restarted. Reordered
// B-frames fall back by a fraction of a second.
const timestampResetSeconds = mediainspect.TimestampResetSeconds

// scanSourceVideo reads every packet of stream streamIndex without decoding
// it (see scanFilePackets).
func scanSourceVideo(ctx context.Context, ffprobePath, path string, streamIndex int) (sourceVideoScan, error) {
	scan, err := scanFilePackets(ctx, ffprobePath, path, packetScanRequest{video: streamIndex})
	return scan.video, err
}

// packetScanRequest is what one read of a file's packets measures: the
// programme video (its stream index, or -1) and the audio and subtitle
// streams listed by index.
type packetScanRequest struct {
	video            int
	audio, subtitles map[int]bool
}

func (r packetScanRequest) any() bool {
	return r.video >= 0 || len(r.audio) > 0 || len(r.subtitles) > 0
}

// filePackets is what scanFilePackets measured. audio and subtitles hold an
// entry for every requested stream that had packets.
type filePackets struct {
	video     sourceVideoScan
	audio     map[int]audioPacketSpan
	subtitles map[int]subtitlePackets
}

// scanFilePackets reads a file's packets once, without decoding them, and
// measures the requested streams: the video's timeline, frames and demuxer
// damage, each audio stream's span (mediainspect.AudioPacketAccumulator, the
// promotion's measure) and each subtitle stream's packets. Healthy files of
// every container the library holds read with no demuxer error at all; the
// errors of a damaged one name where the damage sits. Errors of the decoders
// ffprobe opens to find stream parameters are not the container's and are not
// counted.
func scanFilePackets(ctx context.Context, ffprobePath, path string, req packetScanRequest) (filePackets, error) {
	out := filePackets{video: sourceVideoScan{FirstErrorByte: -1}}
	scan := &out.video
	audio := &mediainspect.AudioPacketAccumulator{}
	if len(req.subtitles) > 0 {
		out.subtitles = map[int]subtitlePackets{}
	}
	videoOnly := len(req.audio) == 0 && len(req.subtitles) == 0
	first := true
	var formatName string
	var parseErr error
	stdout := &lineWriter{line: func(line string) {
		parts := strings.Split(line, "|")
		switch parts[0] {
		case "packet":
			if parseErr != nil {
				return
			}
			fields := make(map[string]string, len(parts)-1)
			for _, f := range parts[1:] {
				name, value, _ := strings.Cut(f, "=")
				fields[name] = value
			}
			index, err := strconv.Atoi(fields["stream_index"])
			switch {
			case err == nil:
			case fields["stream_index"] == "" && videoOnly:
				// Only the video's packets are printed.
				index = req.video
			default:
				parseErr = fmt.Errorf("unexpected ffprobe packet output %q", truncateForLog(line, 120))
				return
			}
			switch {
			case index == req.video:
				addVideoPacket(scan, fields, &first)
			case req.audio[index]:
				if err := audio.Add(fields); err != nil {
					parseErr = fmt.Errorf("audio stream %d: %w", index, err)
				}
			case req.subtitles[index]:
				size, err := strconv.ParseInt(fields["size"], 10, 64)
				if err != nil || size < 0 {
					parseErr = fmt.Errorf("unexpected ffprobe subtitle packet output %q", truncateForLog(line, 120))
					return
				}
				c := out.subtitles[index]
				c.packets++
				if size > movTextEmptySampleBytes {
					c.text++
				}
				out.subtitles[index] = c
			}
		case "format":
			for _, f := range parts[1:] {
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
	// readFaults are the demuxer's failed reads: the scan stopped where the
	// read did, so it judged nothing about the file.
	readFaults := map[string]*tagErrors{}
	// A failed read is the file's no matter which context logged it, or none.
	var untaggedFaults tagErrors
	var stderrTail []string
	stderr := &lineWriter{line: func(line string) {
		tag, text, ok := demuxerTag(line)
		if !ok {
			if mediainspect.ReadFaultPattern.MatchString(line) {
				if untaggedFaults.count++; untaggedFaults.count == 1 {
					untaggedFaults.first = line
				}
			}
			// The last lines: ffprobe ends with the reason it stopped.
			if stderrTail = append(stderrTail, line); len(stderrTail) > 5 {
				stderrTail = stderrTail[1:]
			}
			return
		}
		target := byTag
		if mediainspect.ReadFaultPattern.MatchString(text) {
			target = readFaults
		} else if !packetDamagePattern.MatchString(text) {
			return
		}
		e := target[tag]
		if e == nil {
			e = &tagErrors{first: "[" + tag + "] " + text}
			target[tag] = e
		}
		e.count++
	}}
	args := []string{"-v", "error"}
	if videoOnly {
		// The video alone: the other streams' packets are not printed.
		args = append(args, "-select_streams", strconv.Itoa(req.video))
	}
	args = append(args, "-show_entries", "packet=stream_index,pts_time,dts_time,duration_time,size,flags:format=format_name", "-of", "compact", path)
	cmd := exec.CommandContext(ctx, ffprobePath, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := runCharged(ctx, cmd)
	stdout.flush()
	stderr.flush()
	if err != nil {
		return out, fmt.Errorf("ffprobe packet scan failed: %w (stderr: %s)", err, strings.Join(stderrTail, "; "))
	}
	if parseErr != nil {
		return out, parseErr
	}
	if formatName == "" {
		return out, errors.New("ffprobe packet scan printed no format name")
	}
	faults := untaggedFaults
	if e := readFaults[formatName]; e != nil {
		if faults.count == 0 {
			faults.first = e.first
		}
		faults.count += e.count
	}
	if faults.count > 0 {
		// Not the file's damage, so not the permanent class: the read of it
		// failed, and a retry reads it again.
		return out, fmt.Errorf("i/o error: the demuxer's read of the file failed %d time(s) before its packets were fully scanned (%s); the scan judged nothing about the file", faults.count, truncateForLog(faults.first, 200))
	}
	if e := byTag[formatName]; e != nil {
		scan.DemuxErrors, scan.FirstError = e.count, e.first
		if m := errorBytePattern.FindStringSubmatch(e.first); m != nil {
			scan.FirstErrorByte, _ = strconv.ParseInt(m[1], 10, 64)
		}
	}
	if len(req.audio) > 0 {
		spans, err := audio.Spans()
		if err != nil {
			return out, fmt.Errorf("audio packets: %w", err)
		}
		out.audio = make(map[int]audioPacketSpan, len(spans))
		for index, s := range spans {
			out.audio[index] = audioPacketSpan{start: s.Start, end: s.End, bytes: s.Bytes, packets: s.Packets, discarded: s.Discarded}
		}
	}
	return out, nil
}

// addVideoPacket adds one programme video packet to the scan. first is set
// until the first packet is added.
func addVideoPacket(scan *sourceVideoScan, fields map[string]string, first *bool) {
	var pts, dts, dur float64
	hasPTS, hasDTS := false, false
	for name, value := range fields {
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			continue // N/A, or not a time
		}
		switch name {
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
	if !*first && pts < scan.End-timestampResetSeconds {
		scan.Discontinuous = true
	}
	if *first || pts < scan.Start {
		scan.Start = pts
	}
	if *first || pts+dur > scan.End {
		scan.End, scan.LastFrame = pts+dur, dur
	}
	*first = false
	scan.Packets++
	if scan.durations == nil {
		scan.durations = map[int64]int{}
	}
	scan.durations[int64(math.Round(dur*1e6))]++
	key, discard := strings.Contains(fields["flags"], "K"), strings.Contains(fields["flags"], "D")
	leading := !key && !scan.sawKey
	if key {
		scan.sawKey = true
		if scan.keys++; scan.keys == 1 {
			scan.firstKeyPTS = pts
		}
	} else if leading {
		scan.LeadingNonKey++
	}
	if discard {
		scan.Discarded++
		if leading {
			scan.leadDiscarded++
		}
	}
	if scan.keys < 2 {
		scan.headPackets++
		if discard {
			scan.headDiscarded++
		}
		if scan.keys == 1 && !key && hasPTS && pts < scan.firstKeyPTS {
			scan.leadingPictures++
		}
	}
}

// maxHeadDecodePackets bounds the head decodeVideoHead decodes: a source whose
// first picture group runs longer (one keyframe for minutes) is not decoded,
// and only the packets before its first keyframe may then be missing.
const maxHeadDecodePackets = 1200

// decodeVideoHead decodes the head of the programme video (the packets before
// its second keyframe) and records how many pictures the decoder shows from
// them. An open picture group's first keyframe can be followed by leading
// pictures that refer to pictures before it (an HEVC CRA's RASL pictures, an
// H.264 recovery point's leading B-frames): in a file cut from a longer one
// those pictures are gone, the decoder drops them, and the encode has none of
// them. The packets alone cannot tell them from leading pictures that do
// decode (RADL), so the head is decoded: a few dozen packets, never the whole
// file.
func decodeVideoHead(ctx context.Context, ffprobePath, path string, streamIndex int, scan *sourceVideoScan) error {
	if !scan.sawKey || scan.headPackets == 0 || scan.headPackets > maxHeadDecodePackets {
		return nil
	}
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-read_intervals", "%+#"+strconv.Itoa(scan.headPackets), "-count_frames", "-count_packets",
		"-show_entries", "stream=nb_read_frames,nb_read_packets", "-of", "default=noprint_wrappers=1", path)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runCharged(ctx, cmd); err != nil {
		return fmt.Errorf("ffprobe decode of the video's first picture group failed: %w (stderr: %s)", err, truncateForLog(strings.TrimSpace(stderr.String()), 200))
	}
	frames, packets := -1, -1
	for _, line := range strings.Split(stdout.String(), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		n, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		switch key {
		case "nb_read_frames":
			frames = n
		case "nb_read_packets":
			packets = n
		}
	}
	if frames < 0 || packets != scan.headPackets {
		return fmt.Errorf("ffprobe decode of the video's first picture group read %d packets and %d pictures, the packet scan %d packets (%s)", packets, frames, scan.headPackets, truncateForLog(strings.TrimSpace(stdout.String()), 200))
	}
	scan.headFrames, scan.headDecoded = frames, true
	return nil
}

// startDropped is how many of the source's packets a decoder shows no picture
// for at the video's start, and how many of those are also Discarded packets
// (a packet skipped for two reasons is one packet).
//
// With the head decoded it is the head's packets less the pictures shown,
// held to the packets that can be dropped there: those before the first
// keyframe, the first keyframe's leading pictures and the discarded ones. A
// field-coded head shows one picture for two packets, so for it the bound is
// what counts. Without the decode (a head too long, a test that injects the
// scan) it is the packets before the first keyframe, as before.
//
// A decoder also drops pictures after the head when an HEVC BLA picture or an
// end of sequence sits mid-file. Those are not counted: such a source is
// refused, fail closed; it is rare.
func (s sourceVideoScan) startDropped() (dropped, discarded int) {
	if !s.headDecoded {
		return s.LeadingNonKey, s.leadDiscarded
	}
	bound := s.LeadingNonKey + s.leadingPictures + s.headDiscarded
	dropped = max(0, min(s.headPackets-s.headFrames, bound))
	return dropped, min(s.headDiscarded, dropped)
}

// demuxerTag splits an ffmpeg log line "[matroska,webm @ 0x7f..] text" into
// its context name and text, dropping the context's address.
func demuxerTag(line string) (tag, text string, ok bool) {
	return mediainspect.DemuxerTag(line)
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
// which the final gate only found out after the whole encode. Only the grid
// windows are held to it: a fresh window past the video's end is left out of
// the verdict at the final gate, never a reason to refuse the job.
func finalWindowsFitVideo(plan *transcode.Plan, scan sourceVideoScan) error {
	if plan == nil || plan.QualityValidation == nil || scan.Packets == 0 || scan.Discontinuous {
		return nil
	}
	video := scan.Seconds()
	for _, window := range plan.QualityValidation.Samples {
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
	if programme, _ := splitProgrammeVideo(streams); len(programme) > 0 {
		return programme[0].Index
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
