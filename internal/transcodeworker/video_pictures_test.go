package transcodeworker

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// vpkt is one video packet as ffprobe prints it.
type vpkt struct {
	pts, dur float64
	flags    string
	untimed  bool
}

// videoScanOf scans the packets as the packet scan does.
func videoScanOf(pkts ...vpkt) sourceVideoScan {
	s := sourceVideoScan{FirstErrorByte: -1}
	for _, p := range pkts {
		f := map[string]string{"pts_time": strconv.FormatFloat(p.pts, 'f', 6, 64), "dts_time": "N/A", "duration_time": strconv.FormatFloat(p.dur, 'f', 6, 64), "flags": p.flags}
		if p.untimed {
			f["pts_time"] = "N/A"
		}
		addVideoPacket(&s, f)
	}
	return s
}

// run is n packets of one length laid end to end from first; flags gives
// each its flags (nil: the first a keyframe).
func run(n int, first, length float64, flags func(i int) string) []vpkt {
	out := make([]vpkt, n)
	for i := range out {
		out[i] = vpkt{pts: first + float64(i)*length, dur: length}
		if flags != nil {
			out[i].flags = flags(i)
		} else if i == 0 {
			out[i].flags = "K"
		}
	}
	return out
}

// frames is a frame-coded scan of n pictures from first.
func frames(n int, first, length float64) sourceVideoScan {
	return videoScanOf(run(n, first, length, nil)...)
}

// decoded is s with its head decoded to the given pictures.
func decoded(s sourceVideoScan, pictures int) sourceVideoScan {
	n, _ := s.headLength()
	s.headPackets, s.headFrames, s.headDecoded = n, pictures, true
	return s
}

func cat(runs ...[]vpkt) []vpkt {
	var out []vpkt
	for _, r := range runs {
		out = append(out, r...)
	}
	return out
}

func checkVideo(t *testing.T, src, cand sourceVideoScan, codec string, fps float64, fail string) {
	t.Helper()
	err := videoCompleteness(src, cand, codec, fps)
	if fail == "" {
		if err != nil {
			t.Fatalf("rejected: %v", err)
		}
		return
	}
	if err == nil || !errors.Is(err, ErrContentNotPreserved) || !strings.Contains(err.Error(), fail) {
		t.Fatalf("error %v, want a preservation failure saying %q", err, fail)
	}
}

// A frame-coded source shows exactly as many pictures as its decoded head
// says plus every later packet but the discarded ones: the candidate has that
// many frames, no fewer and no more, with its first and last where the
// source's are.
func TestVideoCompleteness(t *testing.T) {
	// 100 pictures at 25 FPS, the first a keyframe, every one shown.
	src := decoded(frames(100, 0, 0.04), 100)
	// A variable frame rate: the last picture lasts two seconds.
	vfr := decoded(videoScanOf(cat(run(99, 0, 0.04, nil), []vpkt{{pts: 3.96, dur: 2}})...), 100)
	// Two packets before the first keyframe, which a decoder skips.
	lead := decoded(videoScanOf(cat([]vpkt{{pts: 0, dur: 0.04}, {pts: 0.04, dur: 0.04}}, run(100, 0.08, 0.04, nil))...), 100)
	// An edit list: three packets marked discard, the first the keyframe.
	editList := decoded(videoScanOf(cat(run(3, -0.12, 0.04, func(i int) string {
		if i == 0 {
			return "KD"
		}
		return "D"
	}), run(100, 0, 0.04, func(int) string { return "" }))...), 100)
	// An edit list that trims the end, past the decoded head.
	trimmedEnd := decoded(videoScanOf(cat(run(395, 0, 0.04, nil), run(5, 15.8, 0.04, func(int) string { return "D" }))...), 301)
	// No packet flagged a keyframe: the decoder showed nothing for the first
	// three until it could.
	noKey := decoded(videoScanOf(run(100, 0, 0.04, func(int) string { return "" })...), 97)
	restarted := decoded(videoScanOf(cat(run(50, 10, 0.04, nil), run(50, 0, 0.04, func(int) string { return "" }))...), 100)
	for _, tc := range []struct {
		name      string
		src, cand sourceVideoScan
		fail      string
	}{
		{"identical", src, frames(100, 0, 0.04), ""},
		{"the last second missing", src, frames(75, 0, 0.04), "75 frames, the source shows 100"},
		{"one frame missing at the end", src, frames(99, 0, 0.04), "99 frames"},
		{"a frame added", src, frames(101, 0, 0.04), "101 frames"},
		{"every frame but squeezed short", src, frames(100, 0, 0.035), "last frame shows 3.465s after its first, the source's 3.960s"},
		{"every frame but running long", src, frames(100, 0, 0.042), "shows 4.158s after its first"},
		{"every frame but starting late", src, frames(100, 0.2, 0.04), ""},
		{"no frames at all", src, sourceVideoScan{}, "has no frames"},
		{"a damaged encode", src, func() sourceVideoScan {
			c := frames(100, 0, 0.04)
			c.DemuxErrors, c.FirstError = 2, "invalid"
			return c
		}(), "is damaged"},
		{"timestamps rounded by a millisecond", src, frames(100, 0.0009, 0.04), ""},
		// The last picture's length is no frame's: only the times count.
		{"a long last picture", vfr, frames(100, 0, 0.04), ""},
		{"a long last picture, the last frame lost", vfr, frames(99, 0, 0.04), "99 frames"},
		{"leading non-keyframes skipped", lead, frames(100, 0.08, 0.04), ""},
		{"more than the leading non-keyframes missing", lead, frames(99, 0.08, 0.04), "99 frames, the source shows 100"},
		{"leading non-keyframes kept", lead, frames(102, 0, 0.04), "102 frames"},
		{"an edit list's discarded frames", editList, frames(100, 0, 0.04), ""},
		{"an edit list's discarded frames, one more missing", editList, frames(99, 0, 0.04), "99 frames"},
		{"an edit list trimming the end", trimmedEnd, frames(395, 0, 0.04), ""},
		{"an edit list trimming the end, its frames kept", trimmedEnd, frames(400, 0, 0.04), "400 frames, the source shows 395"},
		{"no keyframe flagged, the decoder's start skipped", noKey, frames(97, 0.12, 0.04), ""},
		{"no keyframe flagged, a frame more lost", noKey, frames(96, 0.12, 0.04), "96 frames"},
		{"no keyframe flagged, squeezed short", noKey, frames(97, 0.12, 0.039), "after its first"},
		{"a restarted timeline is held to its frames", restarted, frames(100, 0, 0.04), ""},
		{"a restarted timeline that lost frames", restarted, frames(90, 0, 0.04), "90 frames"},
		{"a source measured with no packets", sourceVideoScan{}, frames(1, 0, 0.04), "measured with no packets"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkVideo(t, tc.src, tc.cand, "mpeg4", 25, tc.fail) })
	}
}

// The decoded head decides which of a stream-copied clip's first packets
// show: an open picture group's leading pictures whose references were cut
// away (RASL) show nothing, those that decode (RADL) show and may start the
// candidate before the keyframe.
func TestSourcePicturesFromTheHead(t *testing.T) {
	// The keyframe at 0.08, then two leading pictures at 0 and 0.04, then
	// the rest of the 100 pictures.
	open := videoScanOf(cat([]vpkt{{pts: 0.08, dur: 0.04, flags: "K"}, {pts: 0, dur: 0.04}, {pts: 0.04, dur: 0.04}}, run(97, 0.12, 0.04, func(int) string { return "" }))...)
	rasl, radl := decoded(open, 98), decoded(open, 100)
	for _, tc := range []struct {
		name      string
		src, cand sourceVideoScan
		fail      string
	}{
		{"two leading pictures dropped", rasl, frames(98, 0.08, 0.04), ""},
		{"two dropped and one more lost", rasl, frames(97, 0.08, 0.04), "97 frames, the source shows 98"},
		{"leading pictures that decode", radl, frames(100, 0, 0.04), ""},
		// Leading pictures that decode excuse nothing.
		{"leading pictures that decode, dropped", radl, frames(98, 0.08, 0.04), "98 frames, the source shows 100"},
		{"leading pictures dropped, the end cut", rasl, func() sourceVideoScan { c := frames(98, 0.08, 0.04); c.Last -= 0.2; return c }(), "after its first"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkVideo(t, tc.src, tc.cand, "hevc", 25, tc.fail) })
	}
	// A test's scan, never decoded, bounds the leading pictures.
	if p := sourcePictures(open, "hevc", 25); p.least != 98 || p.most != 100 || p.fields {
		t.Fatalf("undecoded %+v, want 98 to 100", p)
	}
	// A head with more packets before its first keyframe than it holds is
	// not complete: the packets past it show nothing either.
	long := decoded(videoScanOf(cat(run(maxHeadDecodePackets+10, 0, 0.04, func(int) string { return "" }), run(10, 3010*0.04, 0.04, nil))...), 0)
	long.headFrames = 1
	if _, complete := long.headLength(); complete {
		t.Fatal("a head that never reached its keyframe counted as complete")
	}
	if p := sourcePictures(long, "hevc", 25); p.least > 10 || p.most < 10 {
		t.Fatalf("long lead %+v, want 10 within", p)
	}
}

// A field-coded source (PAFF in a transport stream) carries a field per
// packet; the encode one packet per picture. The head decode tells it: half
// its packets show no picture of their own.
func TestVideoCompletenessFieldCoded(t *testing.T) {
	// 100 pictures at 25 FPS as 200 field packets.
	fields := decoded(videoScanOf(run(200, 0, 0.02, nil)...), 100)
	// Frame-coded for its first 400 pictures, then 50 pictures as fields:
	// the head shows frames, the short packets show the fields.
	mixed := decoded(videoScanOf(cat(run(400, 0, 0.04, nil), run(100, 16, 0.02, func(int) string { return "" }))...), 301)
	// Field packets whose length the container does not state.
	unstated := decoded(videoScanOf(run(200, 0, 0.02, nil)...), 100)
	unstated.durations = map[int64]int{0: 200}
	for _, tc := range []struct {
		name      string
		src, cand sourceVideoScan
		codec     string
		fps       float64
		fail      string
	}{
		{"every picture of a field-coded source", fields, frames(100, 0, 0.04), "h264", 25, ""},
		{"a field-coded source's encode missing 10 frames", fields, frames(90, 0, 0.04), "h264", 25, "90 frames, the source shows at least 100"},
		// Two field packets may be one picture or two: the count cannot
		// tell a picture added, the times do.
		{"two pictures added", fields, frames(102, 0, 0.04), "h264", 25, "after its first"},
		{"the probe states the field rate", fields, frames(100, 0, 0.04), "h264", 50, ""},
		{"the probe states the field rate, 10 missing", fields, frames(90, 0, 0.04), "h264", 50, "90 frames"},
		{"no frame rate", fields, frames(100, 0, 0.04), "h264", 0, ""},
		{"fields past the head", mixed, frames(450, 0, 0.04), "h264", 25, ""},
		{"fields past the head, one missing", mixed, frames(449, 0, 0.04), "h264", 25, "449 frames, the source shows at least 450"},
		// A codec that never codes fields apart is held to every packet.
		{"short packets of a frame codec", mixed, frames(450, 0, 0.04), "mpeg4", 25, "450 frames, the source shows 500"},
		{"fields of unstated length", unstated, frames(100, 0, 0.04), "h264", 25, ""},
		{"fields of unstated length, half lost", unstated, frames(50, 0, 0.04), "h264", 25, "50 frames"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkVideo(t, tc.src, tc.cand, tc.codec, tc.fps, tc.fail) })
	}
}

// A packet without a time is still a picture: it is counted as ffprobe
// counts it, and left out of the timeline only.
func TestVideoScanCountsUntimedPackets(t *testing.T) {
	s := videoScanOf(cat(run(10, 0, 0.04, nil), []vpkt{{untimed: true}}, run(10, 0.44, 0.04, func(int) string { return "" }))...)
	if s.Packets != 21 || s.First != 0 || s.Last < 0.799 || s.Last > 0.801 {
		t.Fatalf("scan %+v, want 21 packets, 0 to 0.8 s", s)
	}
	checkVideo(t, decoded(s, 21), frames(21, 0, 0.04), "mpeg4", 25, "")
	checkVideo(t, decoded(s, 21), frames(20, 0, 0.04), "mpeg4", 25, "20 frames, the source shows 21")
}
