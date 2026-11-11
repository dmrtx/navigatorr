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
	// segments are the stretches of the video between the restarts of its
	// clock (the open one is seg, once segOpen), each from its
	// earliest packet time to the latest end of a packet: their lengths
	// add up to the video's real length (restartedSeconds). keys are the
	// keyframes with a byte position in the file (byteWindow). Both are
	// bounded, and overflow turns a lookup off.
	segments     []videoSegment
	seg          videoSegment
	segOpen      bool
	segOverflow  bool
	keys         []segmentKey
	keysOverflow bool
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
	// headFirstShown is the time of the first picture the head decode
	// showed (headShownTimed): a decoder that starts at a recovery point
	// (H.264 intra refresh, cut) shows nothing until the picture is
	// whole again, later than the keyframe.
	headFirstShown float64
	headShownTimed bool
	// durations counts the packets by duration, in microseconds (0 when the
	// container states none).
	durations map[int64]int
	// times holds every timed packet in decode order (see videoTime), at
	// most maxTimedPackets of them; a longer video sets timesOverflow, and
	// any of its packets may then be a field. timesBase is the first timed
	// packet's time, which the others are counted from.
	times         []videoTime
	timesBase     float64
	timesOverflow bool
	// earliest is the earliest time of any timed packet and firstTime the
	// first timed packet's (sawTime), kept whether or not times overflowed.
	// They differ when the clock restarted lower than it began.
	earliest, firstTime float64
	sawTime             bool
	// fieldSample is what decoding a sample of the packets that lie close
	// together found (sampleFieldPictures): whether two of them make one
	// picture (fields) or each its own (a higher frame rate).
	fieldSample fieldSampleResult
	// format is the container's name as the demuxer gives it.
	format string
	// DemuxErrors counts the packet damage the container's demuxer reported
	// while reading. FirstError is the first of it, and FirstErrorByte the
	// byte offset it names (-1 when it names none).
	DemuxErrors    int
	FirstError     string
	FirstErrorByte int64
}

// videoSegment is one stretch of the video between restarts of its clock:
// its earliest packet time, the latest end of a packet (a packet with no
// stated length ends where it begins), its latest packet time and how many
// packets it holds.
type videoSegment struct {
	min, max, maxPTS float64
	n                int
	// durSum is the sum of the packets' stated lengths, and durN how many
	// state one.
	durSum float64
	durN   int
}

// span is the stretch's length: the packets' stated lengths added up when
// every one states its own (a packet whose time the container leaves out
// still lasts its length, which the last time seen does not count), which is
// where ffmpeg puts the part that follows. A video that states none ends its
// last picture one frame after its time, the frame being the stretch's
// average.
func (g videoSegment) span() float64 {
	if g.durN == g.n && g.durSum > 0 {
		return g.durSum
	}
	end := g.max
	if g.n > 1 && g.max <= g.maxPTS {
		end = g.maxPTS + (g.maxPTS-g.min)/float64(g.n-1)
	}
	return end - g.min
}

// segmentKey is a keyframe of the video: its time on its segment's clock,
// the byte offset of its packet and the segment (index into the closed
// segments, then the open one) it lies in.
type segmentKey struct {
	pts float64
	pos int64
	seg int32
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
// nothing. One packet with no time is enough to know nothing: the decoder
// shows its picture at a time the reader guesses (after the picture before
// it, by a length nothing states), so no bound from the timed packets holds,
// and a window called empty is left out of the verdict. Such a video is left
// to the extract, whose empty output says it.
func (s *sourceVideoScan) noPicturesFrom(at float64) bool {
	return s != nil && s.timed && s.untimed == 0 && !s.Discontinuous && at > s.Last+0.001
}

// picturesIn reports whether the scan read a picture a decoder shows inside
// [from, to), times on the source's own timeline, as surely as
// noPicturesFrom reads none: every packet timed, and a clock that never
// restarted. It looks for a packet timed inside the window, between the
// first and the last picture a decoder shows and not marked to be
// discarded: a video with a gap longer than the window (a still slide, a
// frozen screen capture) has pictures after the window but none in it, and
// an extract of it is rightly empty. Without the packets' times (more than
// the scan keeps) it can only tell pictures after from: that is all it
// claims then.
func (s *sourceVideoScan) picturesIn(from, to float64) bool {
	if s == nil || !s.timed || s.untimed > 0 || s.Discontinuous {
		return false
	}
	if s.timesOverflow || len(s.times) == 0 {
		return from < s.Last-0.001
	}
	lo, hi := max(from, s.First-0.001), min(to, s.Last+0.001)
	for _, t := range s.times {
		if at := s.timesBase + float64(t.at)/1e4; !t.discard && at >= lo && at < hi {
			return true
		}
	}
	return false
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
	args = append(args, "-show_entries", "packet=stream_index,pts_time,dts_time,duration_time,size,pos,flags:format=format_name", "-of", "compact", path)
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
	scan.format = formatName
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
	var pos int64
	hasPTS, hasDTS, hasPos := false, false, false
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
		case "pos":
			if v >= 0 && v < math.MaxInt64/2 {
				pos, hasPos = int64(v), true
			}
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
	scan.addTime(pts, dur, key, discard)
	scan.addSegment(pts, dur, key && !discard, pos, hasPos)
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

// videoTime is one timed packet of the programme video: its time from the
// scan's first, in tenths of a millisecond, the length the container states
// for it in microseconds (0 for none), whether it is a keyframe and whether
// the container marks it to be discarded.
type videoTime struct {
	at, dur      int32
	key, discard bool
}

// maxTimedPackets bounds the packets a scan keeps the times of: twelve bytes
// each, nine hours at 60 packets a second.
const maxTimedPackets = 1 << 21

// addTime keeps a timed packet's time (sourceVideoScan.times).
func (s *sourceVideoScan) addTime(pts, dur float64, key, discard bool) {
	if !s.sawTime {
		s.firstTime = pts
	}
	if !s.sawTime || pts < s.earliest {
		s.earliest, s.sawTime = pts, true
	}
	if s.timesOverflow {
		return
	}
	if len(s.times) == 0 {
		s.timesBase = pts
	}
	at := math.Round((pts - s.timesBase) * 1e4)
	if len(s.times) >= maxTimedPackets || math.Abs(at) > math.MaxInt32 {
		s.times, s.timesOverflow = nil, true
		return
	}
	s.times = append(s.times, videoTime{at: int32(at), dur: int32(min(math.Round(dur*1e6), math.MaxInt32)), key: key, discard: discard})
}

func newVideoSegment(pts, dur float64) videoSegment {
	g := videoSegment{min: pts, max: pts + dur, maxPTS: pts, n: 1}
	if dur > 0 {
		g.durSum, g.durN = dur, 1
	}
	return g
}

// maxSegments and maxSegmentKeys bound what a scan keeps of the restarts of
// the clock and of the keyframes' byte offsets: a keyframe every half second
// for nine hours.
const (
	maxSegments    = 1 << 12
	maxSegmentKeys = 1 << 16
)

// addSegment places a timed packet in the stretch of video between restarts
// of the clock: one whose time falls more than timestampResetSeconds behind
// the stretch's latest end starts the next.
func (s *sourceVideoScan) addSegment(pts, dur float64, key bool, pos int64, hasPos bool) {
	switch {
	case !s.segOpen:
		s.seg, s.segOpen = newVideoSegment(pts, dur), true
	case pts < s.seg.max-timestampResetSeconds:
		if len(s.segments) >= maxSegments {
			s.segOverflow = true
		} else {
			s.segments = append(s.segments, s.seg)
		}
		s.seg = newVideoSegment(pts, dur)
	default:
		g := &s.seg
		g.min, g.max, g.maxPTS, g.n = min(g.min, pts), max(g.max, pts+dur), max(g.maxPTS, pts), g.n+1
		if dur > 0 {
			g.durSum, g.durN = g.durSum+dur, g.durN+1
		}
	}
	if !key || !hasPos || s.keysOverflow {
		return
	}
	if len(s.keys) >= maxSegmentKeys {
		s.keys, s.keysOverflow = nil, true
		return
	}
	s.keys = append(s.keys, segmentKey{pts: pts, pos: pos, seg: int32(len(s.segments))})
}

// allSegments are the closed segments and the open one.
func (s *sourceVideoScan) allSegments() []videoSegment {
	out := append([]videoSegment(nil), s.segments...)
	if s.segOpen {
		out = append(out, s.seg)
	}
	return out
}

// restartedSeconds is the length of a video whose clock restarted: the sum
// of its segments' lengths, which is the length of the timeline ffmpeg makes
// of it (each part follows the one before), where First to End would span the
// gap or the overlap between them. It is 0 when the scan lost count of the
// segments.
func (s *sourceVideoScan) restartedSeconds() float64 {
	if s.segOverflow {
		return 0
	}
	total := 0.0
	for _, g := range s.allSegments() {
		total += g.span()
	}
	return total
}

// byteWindow maps a window on the continuous timeline (from and to seconds
// after the first picture, which is at origin on the source's clock) to the
// segment holding it, for a container a read can start in the middle of (an
// MPEG program or transport stream, which resync on their own): the byte
// offset of the last keyframe at least fieldSampleSeekBack before the window
// and the window's bounds on that segment's own clock. ok is false when the
// scan cannot say (a window across a restart, no such keyframe, counts lost).
func (s *sourceVideoScan) byteWindow(origin, from, to float64) (skip int64, lo, hi float64, ok bool) {
	if s.format != "mpegts" && s.format != "mpeg" || s.segOverflow || s.keysOverflow || !s.segOpen {
		return 0, 0, 0, false
	}
	start := 0.0
	for i, g := range s.allSegments() {
		if i == 0 {
			start = g.min - origin
		}
		span := g.span()
		if from >= start && to <= start+span {
			lo, hi = g.min+from-start, g.min+to-start
			best := -1
			for j, k := range s.keys {
				if int(k.seg) == i && k.pts <= lo-fieldSampleSeekBack && (best < 0 || k.pts > s.keys[best].pts) {
					best = j
				}
			}
			if best < 0 {
				return 0, 0, 0, false
			}
			return s.keys[best].pos, lo, hi, true
		}
		start += span
	}
	return 0, 0, 0, false
}

// closePackets marks the timed packets that may be fields: those the
// container states less than fieldPacketShare of a frame long, and those
// whose time lies that close to the previous or the next packet's in decode
// order, whatever length the container states for them (the two fields of a
// picture lie half a frame apart). A packet of a video at a higher frame rate
// than its head's is marked too: only decoding it tells (sampleFieldPictures).
// It is nil when the frame is unknown or no packet is close.
func (s sourceVideoScan) closePackets(frame float64) []bool {
	if frame <= 0 {
		return nil
	}
	limit := frame * fieldPacketShare
	var marks []bool
	mark := func(i int) {
		if marks == nil {
			marks = make([]bool, len(s.times))
		}
		marks[i] = true
	}
	for i, p := range s.times {
		if p.dur > 0 && float64(p.dur)/1e6 < limit {
			mark(i)
		}
		if i > 0 && math.Abs(float64(p.at)-float64(s.times[i-1].at))/1e4 < limit {
			mark(i - 1)
			mark(i)
		}
	}
	return marks
}

// possibleFields is how many timed packets may be fields (closePackets).
func (s sourceVideoScan) possibleFields(frame float64) int {
	n := 0
	for _, near := range s.closePackets(frame) {
		if near {
			n++
		}
	}
	return n
}

// maxHeadDecodePackets bounds the head decodeVideoHead decodes, and
// headAfterKey is how many packets after the first keyframe it reaches, not
// counting those the container marks to be discarded (an MP4 cut by an edit
// list inside a long group of pictures carries hundreds of them, which show
// nothing): the keyframe's leading pictures follow it closely in decode
// order, and a few seconds of pictures tell a field-coded video from a
// frame-coded one. noKeyHeadPackets is the head of a video with no packet
// flagged a keyframe.
const (
	maxHeadDecodePackets = 3000
	headAfterKey         = 300
	noKeyHeadPackets     = 600
)

// headLength is how many of the first packets decodeVideoHead decodes, and
// whether they reach headAfterKey packets past the first keyframe that are
// not discarded (or the end of the video): when they do not, more packets
// before the first keyframe, or discarded after it, than
// maxHeadDecodePackets lie outside the decode.
func (s sourceVideoScan) headLength() (n int, complete bool) {
	if !s.sawKey {
		return min(noKeyHeadPackets, s.Packets, len(s.head)), true
	}
	for i, p := range s.head {
		if !p.key {
			continue
		}
		after := 0
		for j := i + 1; j < len(s.head); j++ {
			if s.head[j].discard {
				continue
			}
			if after++; after == headAfterKey {
				return j + 1, true
			}
		}
		return len(s.head), len(s.head) == s.Packets
	}
	return len(s.head), false
}

// headStats counts, over the first n packets, those before the first
// keyframe, the keyframe's leading pictures and the discarded ones. The
// pictures from the keyframe on that the head decode showed none of, being
// timed before its first picture (headFirstShown), count as leading too:
// the decoder started there and the encode has none of them (a recovery
// point it shows only once the picture is whole again).
func (s sourceVideoScan) headStats(n int) (nonKey, leading, discarded int) {
	seenKey := false
	keyPTS := 0.0
	late := func(p headPacket) bool {
		return s.headDecoded && s.headShownTimed && !p.discard && p.timed && p.pts >= keyPTS && p.pts < s.headFirstShown-0.0005
	}
	for _, p := range s.head[:min(n, len(s.head))] {
		switch {
		case !seenKey && p.key:
			seenKey, keyPTS = true, p.pts
			if late(p) {
				leading++
			}
		case !seenKey:
			nonKey++
		case !p.key && p.timed && p.pts < keyPTS:
			leading++
		case late(p):
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
// decoded: a few seconds of packets, never the whole file. The time of the
// first picture it shows is kept too: a decoder that starts at a recovery
// point drops the pictures after it until the picture is whole again
// (headStats), which are not fields.
func decodeVideoHead(ctx context.Context, ffprobePath, path string, streamIndex int, scan *sourceVideoScan) error {
	n, _ := scan.headLength()
	if n == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-read_intervals", "%+#"+strconv.Itoa(n), "-count_frames", "-count_packets",
		"-show_entries", "stream=nb_read_frames,nb_read_packets:frame=best_effort_timestamp_time", "-of", "compact", path)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runCharged(ctx, cmd); err != nil {
		return fmt.Errorf("ffprobe decode of the video's first %d packets failed: %w (stderr: %s)", n, err, truncateForLog(strings.TrimSpace(stderr.String()), 200))
	}
	frames, packets := -1, -1
	firstShown, shownTimed, shownUntimed := 0.0, false, false
	for _, line := range strings.Split(stdout.String(), "\n") {
		section, rest, _ := strings.Cut(strings.TrimSpace(line), "|")
		for _, field := range strings.Split(rest, "|") {
			key, value, _ := strings.Cut(field, "=")
			if section == "frame" && key == "best_effort_timestamp_time" {
				t, err := strconv.ParseFloat(value, 64)
				switch {
				case err != nil || math.IsNaN(t) || math.IsInf(t, 0):
					shownUntimed = true
				case !shownTimed || t < firstShown:
					firstShown, shownTimed = t, true
				}
				continue
			}
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
	}
	if frames < 0 || packets != n {
		return fmt.Errorf("ffprobe decode of the video's first packets read %d packets and %d pictures, the packet scan %d packets (%s)", packets, frames, n, truncateForLog(strings.TrimSpace(stdout.String()), 200))
	}
	if frames == 0 {
		return sourceDamaged(fmt.Errorf("%w: the source's video shows no picture in its first %d packets; nothing can say which of its pictures an encode must keep", ErrSourceInvalid, n))
	}
	scan.headPackets, scan.headFrames, scan.headDecoded = n, frames, true
	// A picture shown at no time says nothing of where the decoder started.
	scan.headFirstShown, scan.headShownTimed = firstShown, shownTimed && !shownUntimed
	return nil
}

// fieldSampleResult is what sampleFieldPictures found.
type fieldSampleResult int

const (
	// fieldsNotSampled: nothing was decoded (no packet lies close to
	// another, the head already shows fields, or no sample could be read).
	fieldsNotSampled fieldSampleResult = iota
	// fieldsSampledFrames: every sample read showed one picture per packet.
	fieldsSampledFrames
	// fieldsSampledFields: a sample showed two packets making one picture.
	fieldsSampledFields
)

// fieldSamples is how many places among the close packets are decoded,
// fieldSampleSeconds how long each one is, and maxFieldSampleLead how many
// packets a sample may decode before it, from the keyframe it starts at: a
// sample further from its keyframe is skipped. A container with an index
// (Matroska, MP4) seeks to the keyframe at or before the time asked, so its
// read aims at the keyframe itself; a transport stream seeks to a packet near
// the time, so its read starts fieldSampleSeekBack before the keyframe. When
// a read does not start at the keyframe the other way is tried.
const (
	fieldSamples        = 5
	fieldSampleSeconds  = 1.0
	maxFieldSampleLead  = 1500
	fieldSampleSeekBack = 1.0
)

// sampleFieldPictures tells, for a source of a field codec whose head is
// frame-coded, whether its later packets that lie close together
// (closePackets) are fields or the pictures of a higher frame rate: a
// section at 60 frames a second after a 30 or 24 frame head looks like fields
// by its timing alone, and would let a conversion lose half of it unseen. Up
// to fieldSamples places spread over those packets are decoded, each
// fieldSampleSeconds long from the keyframe before it: the second field of a
// picture shows no picture of its own, so a sample of fields shows about half
// as many pictures as it has close packets, a sample of frames one per packet.
// One sample of fields marks the source fields; every sample read showing
// frames marks it frame-coded, counted exactly. A few seconds of packets are
// decoded at most per sample, never the whole file.
func sampleFieldPictures(ctx context.Context, ffprobePath, path string, streamIndex int, scan *sourceVideoScan, codec string, fps float64) error {
	if !fieldCodecs[strings.ToLower(codec)] || !scan.headDecoded || scan.timesOverflow {
		return nil
	}
	if scan.headShowsFields(scan.headStats(scan.headPackets)) {
		return nil
	}
	frame := scan.headFrame()
	if frame <= 0 && fps > 0 {
		frame = 1 / fps
	}
	marks := scan.closePackets(frame)
	var near []int
	for i, m := range marks {
		if m {
			near = append(near, i)
		}
	}
	if len(near) == 0 {
		return nil
	}
	result := fieldsNotSampled
	sampledTo := math.Inf(-1)
	for j := 0; j < fieldSamples; j++ {
		i := near[(len(near)-1)*j/(fieldSamples-1)]
		from := float64(scan.times[i].at) / 1e4
		if from < sampledTo {
			continue // inside the previous sample
		}
		key := -1
		for k := i; k >= 0 && i-k <= maxFieldSampleLead; k-- {
			if scan.times[k].key && scan.times[k].at <= scan.times[i].at {
				key = k
				break
			}
		}
		if key < 0 {
			continue
		}
		sampledTo = from + fieldSampleSeconds
		fields, read, err := decodeFieldSample(ctx, ffprobePath, path, streamIndex, scan, marks, key, from, sampledTo)
		if err != nil {
			return err
		}
		switch {
		case !read:
		case fields:
			scan.fieldSample = fieldsSampledFields
			return nil
		default:
			result = fieldsSampledFrames
		}
	}
	scan.fieldSample = result
	return nil
}

// decodeFieldSample decodes the source's video from the keyframe key (an
// index into scan.times) to past to, and compares, between from and to
// (seconds from scan.timesBase), the packets with the pictures shown and the
// close packets (marks). read is false when the decode did not start at the
// keyframe (a transport stream's seek lands where it can), the window holds
// no close packet or ffprobe failed to read it: the sample then says nothing,
// and an unsampled source keeps its range. Damage in the packets themselves
// is the packet scan's to report (sourceVideoScan.damage), not a sample's.
// Only a cancelled job ends the sampling with an error.
func decodeFieldSample(ctx context.Context, ffprobePath, path string, streamIndex int, scan *sourceVideoScan, marks []bool, key int, from, to float64) (fields, read bool, err error) {
	keyAt := scan.timesBase + float64(scan.times[key].at)/1e4
	seeks := []float64{keyAt + 0.0005, keyAt - fieldSampleSeekBack}
	if !indexedContainer(scan.format) {
		seeks[0], seeks[1] = seeks[1], seeks[0]
	}
	for _, seek := range seeks {
		fields, read, err = decodeFieldWindow(ctx, ffprobePath, path, streamIndex, scan, marks, keyAt, seek, from, to)
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		if err == nil && read {
			return fields, true, nil
		}
	}
	return false, false, nil
}

// windowSeek is where an extract of the source seeks to decode the pictures
// from at (a time on the source's own timeline), and whether that seek
// reaches the keyframe they are decoded from. A container with a keyframe
// index (Matroska, MP4) seeks to the keyframe at or before the time asked by
// itself. A transport stream seeks to a packet near the time, inside a group
// of pictures, and a decoder shows nothing until the next keyframe: the
// extract would start late, or hold nothing. Its seek goes
// fieldSampleSeekBack before the last keyframe at or before at that the scan
// read. With no keyframe times (more packets than the scan keeps, a clock
// that restarted, no keyframe before at) it goes windowSeekFallback before
// at, and is not known to reach a keyframe. The extract trims the window by
// its times, so a seek earlier than needed keeps the same pictures and only
// decodes more. A seek that would land before zero, or at or near the time
// of the first packet or of the earliest one the scan read, is not made
// (math.Inf(-1)): the extract decodes from the file's start, which reaches
// the first keyframe (an AVI cannot seek before its first packet, and its
// extract would then start wherever its probe stopped reading). That holds
// for a clock that restarted too: a seek before the first packet's time
// aims at no packet before the window, and where the clock restarted lower
// than it began, it finds the restarted part instead (an MPEG program
// stream's seek lands there, and decodes nothing of the window; the final
// gate does not use this seek for a clock that restarted, see
// sourceExtractions). Decoding from the start is keyed: the
// decoder shows every picture from the first keyframe on, and a picture
// before it is shown by no decoder (picturesIn reads none there).
func (s *sourceVideoScan) windowSeek(at float64) (float64, bool) {
	if indexedContainer(s.format) {
		return at, true
	}
	seek, keyed := at-windowSeekFallback, false
	if !s.timesOverflow && !s.Discontinuous {
		keyAt := math.Inf(-1)
		for _, t := range s.times {
			if k := s.timesBase + float64(t.at)/1e4; t.key && k <= at && k > keyAt {
				keyAt = k
			}
		}
		if !math.IsInf(keyAt, -1) {
			seek, keyed = keyAt-fieldSampleSeekBack, true
		}
	}
	if seek < 0 || s.sawTime && seek < max(s.earliest, s.firstTime)+windowSeekStartMargin {
		return math.Inf(-1), true
	}
	return seek, keyed
}

// windowSeekFallback is how far before a window an extract seeks when the
// scan holds no keyframe time before it: several groups of pictures of a
// broadcast stream. windowSeekStartMargin is how close to the video's first
// packet a seek may land before none is made: ffmpeg moves a seek back a
// little for a video with B-pictures.
const (
	windowSeekFallback    = 20.0
	windowSeekStartMargin = 0.5
)

// indexedContainer reports whether a demuxer seeks by an index of keyframes,
// to the one at or before the time asked: Matroska and MP4 do.
func indexedContainer(format string) bool {
	for _, name := range strings.Split(format, ",") {
		if name == "matroska" || name == "mov" {
			return true
		}
	}
	return false
}

// decodeFieldWindow is one read of decodeFieldSample, seeking to seek.
func decodeFieldWindow(ctx context.Context, ffprobePath, path string, streamIndex int, scan *sourceVideoScan, marks []bool, keyAt, seek, from, to float64) (fields, read bool, err error) {
	start, end := scan.timesBase+from, scan.timesBase+to
	interval := strconv.FormatFloat(seek, 'f', 6, 64) + "%" + strconv.FormatFloat(end+0.5, 'f', 6, 64)
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-read_intervals", interval, "-show_entries", "packet=pts_time,dts_time,flags:frame=best_effort_timestamp_time", "-of", "compact", path)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runCharged(ctx, cmd); err != nil {
		return false, false, fmt.Errorf("ffprobe decode of the video from %.3fs to %.3fs failed: %w (stderr: %s)", keyAt, end, err, truncateForLog(strings.TrimSpace(stderr.String()), 200))
	}
	sawKey, packets, pictures, ok := countFieldWindow(stdout.String(), keyAt, start, end)
	if !ok {
		return false, false, nil // a picture shown at no time: the sample says nothing
	}
	near := 0
	for i, p := range scan.times {
		if at := float64(p.at) / 1e4; marks[i] && at >= from && at < to {
			near++
		}
	}
	if !sawKey || packets == 0 || near == 0 {
		return false, false, nil
	}
	return fieldWindowVerdict(packets, pictures, near), true, nil
}

// fieldWindowVerdict reads one decoded sample: packets read and pictures
// shown between its bounds, near of those packets close to another. A sample
// of fields shows about one picture for every two close packets, so at least
// a quarter of them must show none. The close packets must be at least a
// quarter of the sample too: a frame-coded sample with one or two close
// packets (a timing glitch) and one picture a decoder dropped is not fields,
// and taking it for them would halve what a conversion must keep.
func fieldWindowVerdict(packets, pictures, near int) bool {
	missing := packets - pictures
	return missing > 0 && 4*missing >= near && 4*near >= packets
}

// countFieldWindow reads decodeFieldWindow's ffprobe output: whether the
// keyframe at keyAt was read, and how many packets and pictures lie between
// start and end. A packet is timed as the scan times it (addVideoPacket): by
// its presentation time, or its decode time when it has none (AVI). ok is
// false when a picture was shown at no time.
func countFieldWindow(output string, keyAt, start, end float64) (sawKey bool, packets, pictures int, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(line), "|")
		var t, dts float64
		timed, hasDTS, keyFlag := false, false, false
		for _, f := range strings.Split(rest, "|") {
			name, value, _ := strings.Cut(f, "=")
			v, err := strconv.ParseFloat(value, 64)
			bad := err != nil || math.IsNaN(v) || math.IsInf(v, 0)
			switch name {
			case "pts_time", "best_effort_timestamp_time":
				if !bad {
					t, timed = v, true
				}
			case "dts_time":
				if !bad {
					dts, hasDTS = v, true
				}
			case "flags":
				keyFlag = strings.Contains(value, "K")
			}
		}
		switch kind {
		case "packet":
			if !timed && hasDTS {
				t, timed = dts, true
			}
			if keyFlag && timed && math.Abs(t-keyAt) < 0.0005 {
				sawKey = true
			}
			if timed && t >= start && t < end {
				packets++
			}
		case "frame":
			if !timed {
				return false, 0, 0, false
			}
			if t >= start && t < end {
				pictures++
			}
		}
	}
	return sawKey, packets, pictures, true
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
	if plan == nil || plan.QualityValidation == nil || !scan.timed || scan.Discontinuous && scan.restartedSeconds() <= 0 {
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
		if scan.Discontinuous {
			// The restarts' gaps and overlaps are no video: the encode's
			// timeline is the parts one after the other.
			limit = scan.restartedSeconds()
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
