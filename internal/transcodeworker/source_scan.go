package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
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
	// Packets counts every packet, those without a time included, as
	// ffprobe's own packet count does.
	Packets int
	// First and Last are the times of the first and the last picture a
	// decoder surely shows: the packets before the first keyframe, those the
	// container marks to be discarded and the first keyframe's leading
	// pictures are left out of them (timed is set once one is seen); a
	// video with no keyframe flagged is timed from its first packet. End is
	// where the last picture ends when the container states its length
	// (endKnown), and Last otherwise.
	First, Last, End float64
	timed, endKnown  bool
	// leadFirst is the time of the earliest leading picture (hasLead): one
	// that follows the first keyframe in decode order and shows before it.
	// A decoder may show it or not.
	leadFirst float64
	hasLead   bool
	// Discontinuous is set when the timestamps jump back by more than
	// timestampResetSeconds (a broadcast recording whose clock restarts):
	// First and Last then no longer measure the video's length. latest is
	// the latest packet end seen, which a restart falls behind.
	Discontinuous bool
	latest        float64
	anyTime       bool
	// LeadingNonKey counts the packets read before the first keyframe: a
	// decoder has nothing to show them with. Discarded counts the packets the
	// container marks to be dropped (an edit list that starts the video
	// later), which no decoder shows. leadDiscarded counts the packets that
	// are both.
	LeadingNonKey, Discarded, leadDiscarded int
	// untimed counts the packets with no time at all: a decoder may still
	// show them, at a time no packet states.
	untimed     int
	sawKey      bool
	firstKeyPTS float64
	// head holds the first packets as read, at most maxHeadDecodePackets of
	// them: decodeVideoHead decodes the first headPackets of them and
	// records in headFrames how many pictures a decoder shows (headDecoded).
	head        []headPacket
	headPackets int
	headFrames  int
	headDecoded bool
	// durations counts the packets by duration, in microseconds (0 when the
	// container states none).
	durations map[int64]int
	// steps counts the timed packets by their step: how far, in tenths of a
	// millisecond, a packet's time lies from the previous timed packet's in
	// decode order, paired with that previous packet's own step (-1 for none).
	// The two fields of a picture lie half a frame apart whatever length the
	// container states for them, and the step before says where a run of
	// them starts. A video whose steps take more than maxStepKinds pairs
	// stops counting new ones (stepsOverflow) and is judged by its durations.
	steps         map[[2]int64]int
	stepsOverflow bool
	prevPTS       float64
	prevStep      int64
	hasPrevPTS    bool
	// DemuxErrors counts the packet damage the container's demuxer reported
	// while reading. FirstError is the first of it, and FirstErrorByte the
	// byte offset it names (-1 when it names none).
	DemuxErrors    int
	FirstError     string
	FirstErrorByte int64
}

// headPacket is one of the first packets of the programme video.
type headPacket struct {
	key, discard, timed bool
	pts                 float64
}

// Seconds is the video's length from its first picture to the end of its
// last.
func (s sourceVideoScan) Seconds() float64 {
	if !s.timed {
		return 0
	}
	return s.End - s.First
}

// start is the earliest time a picture of the video may show: its first
// leading picture's, or its first picture's.
func (s sourceVideoScan) start() float64 {
	if s.hasLead && s.leadFirst < s.First {
		return s.leadFirst
	}
	return s.First
}

// noPicturesFrom reports whether the video, as the scan read it, has no
// picture at or after at, a time on the source's own timeline: every packet
// was timed, the clock never restarted and the last picture a decoder shows
// comes before at. A window cut from there extracts nothing, whatever the
// extract's exit status or messages say. A nil scan (none was read) knows
// nothing.
func (s *sourceVideoScan) noPicturesFrom(at float64) bool {
	return s != nil && s.timed && s.untimed == 0 && !s.Discontinuous && at > s.Last+0.001
}

// errorBytePattern finds the byte offset a demuxer error names ("at pos
// 27835247", "pos: 1234", "File ended prematurely at pos. 4021").
var errorBytePattern = regexp.MustCompile(`\bpos(?:ition)?\.?[: ]+(\d+)`)

// errorStreamPattern finds the stream a demuxer error names ("Packet corrupt
// (stream = 2, dts = 1234)").
var errorStreamPattern = regexp.MustCompile(`\(stream = (\d+)\b`)

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

// wants reports whether the request measures stream index.
func (r packetScanRequest) wants(index int) bool {
	return (r.video >= 0 && index == r.video) || r.audio[index] || r.subtitles[index]
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
// counted, nor are errors that name a stream the request leaves out: a track
// the output drops cannot fail the job.
func scanFilePackets(ctx context.Context, ffprobePath, path string, req packetScanRequest) (filePackets, error) {
	out := filePackets{video: sourceVideoScan{FirstErrorByte: -1}}
	if !req.any() {
		return out, nil
	}
	scan := &out.video
	audio := &mediainspect.AudioPacketAccumulator{}
	if len(req.subtitles) > 0 {
		out.subtitles = map[int]subtitlePackets{}
	}
	videoOnly := req.video >= 0 && len(req.audio) == 0 && len(req.subtitles) == 0
	var formatName string
	var parseErr error
	stdout := &lineWriter{line: func(line string) {
		if parseErr != nil {
			return
		}
		rest, isPacket := strings.CutPrefix(line, "packet|")
		if !isPacket {
			if rest, ok := strings.CutPrefix(line, "format|"); ok {
				for _, f := range strings.Split(rest, "|") {
					if key, value, _ := strings.Cut(f, "="); key == "format_name" {
						formatName = value
					}
				}
				return
			}
			if strings.TrimSpace(line) != "" {
				parseErr = fmt.Errorf("unexpected ffprobe packet output %q", truncateForLog(line, 120))
			}
			return
		}
		index := req.video
		if !videoOnly {
			// The stream comes first: a packet of a stream left out is
			// skipped before its other fields are read.
			field, _, _ := strings.Cut(rest, "|")
			value, ok := strings.CutPrefix(field, "stream_index=")
			n, err := strconv.Atoi(value)
			if !ok || err != nil {
				parseErr = fmt.Errorf("unexpected ffprobe packet output %q", truncateForLog(line, 120))
				return
			}
			if !req.wants(n) {
				return
			}
			index = n
		}
		fields := make(map[string]string, 6)
		for _, f := range strings.Split(rest, "|") {
			name, value, _ := strings.Cut(f, "=")
			fields[name] = value
		}
		switch {
		case index == req.video:
			addVideoPacket(scan, fields)
		case req.audio[index]:
			if err := audio.Add(fields); err != nil {
				parseErr = fmt.Errorf("audio stream %d: %w", index, err)
			}
		case req.subtitles[index]:
			c := out.subtitles[index]
			c.add(fields)
			out.subtitles[index] = c
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
		} else if m := errorStreamPattern.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && !req.wants(n) {
				// Damage in a track the output leaves out.
				return
			}
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
			out.audio[index] = audioSpanOf(s)
		}
	}
	return out, nil
}

// addVideoPacket adds one programme video packet to the scan.
func addVideoPacket(scan *sourceVideoScan, fields map[string]string) {
	var pts, dts, dur float64
	hasPTS, hasDTS := false, false
	for name, value := range fields {
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue // N/A, or not a time
		}
		switch name {
		case "pts_time":
			pts, hasPTS = v, true
		case "dts_time":
			dts, hasDTS = v, true
		case "duration_time":
			dur = max(v, 0)
		}
	}
	if !hasPTS && hasDTS {
		// AVI and raw streams can carry decode times only.
		pts, hasPTS = dts, true
	}
	key, discard := strings.Contains(fields["flags"], "K"), strings.Contains(fields["flags"], "D")
	// A packet with no time still holds a picture: it is counted, and only
	// the timeline leaves it out.
	scan.Packets++
	if len(scan.head) < maxHeadDecodePackets {
		scan.head = append(scan.head, headPacket{key: key, discard: discard, timed: hasPTS, pts: pts})
	}
	leading := !key && !scan.sawKey
	if key && !scan.sawKey {
		// The timeline starts at the first keyframe: what came before it
		// stood for the timeline only while no keyframe was seen.
		scan.sawKey, scan.firstKeyPTS, scan.timed = true, pts, false
	} else if leading {
		scan.LeadingNonKey++
	}
	if discard {
		scan.Discarded++
		if leading {
			scan.leadDiscarded++
		}
	}
	if !hasPTS {
		scan.untimed++
		return
	}
	if scan.durations == nil {
		scan.durations = map[int64]int{}
	}
	scan.durations[int64(math.Round(dur*1e6))]++
	scan.addStep(pts)
	if scan.anyTime && pts < scan.latest-timestampResetSeconds {
		scan.Discontinuous = true
	}
	if !scan.anyTime || pts+dur > scan.latest {
		scan.latest, scan.anyTime = pts+dur, true
	}
	switch {
	case discard:
		// No decoder shows it.
		return
	case scan.sawKey && !key && pts < scan.firstKeyPTS:
		// One of the first keyframe's leading pictures.
		if !scan.hasLead || pts < scan.leadFirst {
			scan.leadFirst, scan.hasLead = pts, true
		}
		return
	}
	if !scan.timed || pts < scan.First {
		scan.First = pts
	}
	if !scan.timed || pts > scan.Last {
		scan.Last = pts
		scan.End, scan.endKnown = pts+dur, dur > 0
	}
	scan.timed = true
}

// maxStepKinds bounds the pairs of steps a scan counts (sourceVideoScan.steps).
const maxStepKinds = 4096

// addStep counts a timed packet's step (sourceVideoScan.steps).
func (s *sourceVideoScan) addStep(pts float64) {
	step, before := int64(-1), int64(-1)
	if s.hasPrevPTS {
		step, before = int64(math.Round(math.Abs(pts-s.prevPTS)*1e4)), s.prevStep
	}
	s.prevPTS, s.prevStep, s.hasPrevPTS = pts, step, true
	key := [2]int64{before, step}
	if s.steps == nil {
		s.steps = map[[2]int64]int{}
	}
	if _, ok := s.steps[key]; !ok && len(s.steps) >= maxStepKinds {
		s.stepsOverflow = true
		return
	}
	s.steps[key]++
}

// fieldSteps is how many timed packets lie less than fieldPacketShare of a
// frame from a neighbour in decode order: each run of such steps holds one
// packet more than it has steps. Those packets may be fields however long the
// container says they are. It is 0 when the frame is unknown or the steps
// were not all counted.
func (s sourceVideoScan) fieldSteps(frame float64) int {
	if frame <= 0 || s.stepsOverflow {
		return 0
	}
	short := func(step int64) bool { return step >= 0 && float64(step)/1e4 < frame*fieldPacketShare }
	n := 0
	for k, c := range s.steps {
		if short(k[1]) {
			n += c
			if !short(k[0]) {
				n += c
			}
		}
	}
	return n
}

// maxHeadDecodePackets bounds the head decodeVideoHead decodes, and
// headAfterKey is how many packets after the first keyframe it reaches: the
// keyframe's leading pictures follow it closely in decode order, and a few
// seconds of pictures tell a field-coded video from a frame-coded one.
// noKeyHeadPackets is the head of a video with no packet flagged a keyframe.
const (
	maxHeadDecodePackets = 3000
	headAfterKey         = 300
	noKeyHeadPackets     = 600
)

// headLength is how many of the first packets decodeVideoHead decodes, and
// whether they reach headAfterKey packets past the first keyframe (or the end
// of the video): when they do not, more packets before the first keyframe
// than maxHeadDecodePackets lie outside the decode.
func (s sourceVideoScan) headLength() (n int, complete bool) {
	if !s.sawKey {
		return min(noKeyHeadPackets, s.Packets, len(s.head)), true
	}
	for i, p := range s.head {
		if p.key {
			n = min(i+1+headAfterKey, len(s.head))
			return n, n == s.Packets || n == i+1+headAfterKey
		}
	}
	return len(s.head), false
}

// headStats counts, over the first n packets, those before the first
// keyframe, the keyframe's leading pictures and the discarded ones.
func (s sourceVideoScan) headStats(n int) (nonKey, leading, discarded int) {
	seenKey := false
	keyPTS := 0.0
	for _, p := range s.head[:min(n, len(s.head))] {
		switch {
		case !seenKey && p.key:
			seenKey, keyPTS = true, p.pts
		case !seenKey:
			nonKey++
		case !p.key && p.timed && p.pts < keyPTS:
			leading++
		}
		if p.discard {
			discarded++
		}
	}
	return nonKey, leading, discarded
}

// decodeVideoHead decodes the head of the programme video (see headLength)
// and records how many pictures the decoder shows from it. An open picture
// group's first keyframe can be followed by leading pictures that refer to
// pictures before it (an HEVC CRA's RASL pictures, an H.264 recovery point's
// leading B-frames): in a file cut from a longer one those pictures are gone,
// the decoder drops them, and the encode has none of them. The packets alone
// cannot tell them from leading pictures that do decode (RADL), nor a
// field-coded picture (two packets, one picture) from a frame, so the head is
// decoded: a few seconds of packets, never the whole file.
func decodeVideoHead(ctx context.Context, ffprobePath, path string, streamIndex int, scan *sourceVideoScan) error {
	n, _ := scan.headLength()
	if n == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-read_intervals", "%+#"+strconv.Itoa(n), "-count_frames", "-count_packets",
		"-show_entries", "stream=nb_read_frames,nb_read_packets", "-of", "default=noprint_wrappers=1", path)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runCharged(ctx, cmd); err != nil {
		return fmt.Errorf("ffprobe decode of the video's first %d packets failed: %w (stderr: %s)", n, err, truncateForLog(strings.TrimSpace(stderr.String()), 200))
	}
	frames, packets := -1, -1
	for _, line := range strings.Split(stdout.String(), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		v, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		switch key {
		case "nb_read_frames":
			frames = v
		case "nb_read_packets":
			packets = v
		}
	}
	if frames < 0 || packets != n {
		return fmt.Errorf("ffprobe decode of the video's first packets read %d packets and %d pictures, the packet scan %d packets (%s)", packets, frames, n, truncateForLog(strings.TrimSpace(stdout.String()), 200))
	}
	if frames == 0 {
		return sourceDamaged(fmt.Errorf("%w: the source's video shows no picture in its first %d packets; nothing can say which of its pictures an encode must keep", ErrSourceInvalid, n))
	}
	scan.headPackets, scan.headFrames, scan.headDecoded = n, frames, true
	return nil
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
// which the final gate only found out after the whole encode. Windows are
// cut from the first decoded picture (probeFirstFrameTime, then
// BuildReferenceExtractionArgs seeks firstFrame+StartSeconds), so the video's
// end is measured from that picture, whatever stream the container starts
// with. A last picture whose length the container does not state ends
// anywhere up to the container's end: only a window that starts after it is
// refused then. Only the grid windows are held to it: a fresh window past the
// video's end is left out of the verdict at the final gate, never a reason to
// refuse the job.
func finalWindowsFitVideo(plan *transcode.Plan, scan sourceVideoScan) error {
	if plan == nil || plan.QualityValidation == nil || !scan.timed || scan.Discontinuous {
		return nil
	}
	origin := scan.start()
	for _, window := range plan.QualityValidation.Samples {
		// Half a second of slack: a window a frame past the end is the
		// final gate's to judge, as before.
		end, limit, what := window.StartSeconds+window.DurationSeconds, scan.End-origin, "ends"
		if !scan.endKnown {
			end, limit, what = window.StartSeconds, scan.Last-origin, "starts"
		}
		if end > limit+0.5 {
			return &classedError{class: resilience.InternalInvariant, err: fmt.Errorf("quality_final_window_beyond_video: final window %d %s at %.1fs on the container's timeline, but the video ends at %.1fs while the container runs on; the windows were planned on the container's length",
				window.Index, what, end, limit)}
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
