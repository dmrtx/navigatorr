package transcodeworker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// A candidate's programme video is held to the source's frames: the same
// number, ending where the source's ends. Only frames a decoder never shows
// may be missing.
func TestVideoCompleteness(t *testing.T) {
	// 100 frames at 25 FPS, the first a keyframe.
	src := sourceVideoScan{Packets: 100, Start: 0, End: 4, LastFrame: 0.04, sawKey: true}
	for _, tc := range []struct {
		name string
		src  sourceVideoScan
		cand sourceVideoScan
		fail string
	}{
		{"identical", src, src, ""},
		{"the last second missing", src, sourceVideoScan{Packets: 75, End: 3, LastFrame: 0.04, sawKey: true}, "75 frames, the source 100"},
		{"one frame missing at the end", src, sourceVideoScan{Packets: 99, End: 3.96, LastFrame: 0.04, sawKey: true}, "99 frames"},
		{"a frame added", src, sourceVideoScan{Packets: 101, End: 4.04, LastFrame: 0.04, sawKey: true}, "101 frames"},
		{"every frame but squeezed short", src, sourceVideoScan{Packets: 100, End: 3.5, LastFrame: 0.035, sawKey: true}, "ends 3.500s"},
		{"every frame but running long", src, sourceVideoScan{Packets: 100, End: 4.2, LastFrame: 0.04, sawKey: true}, "ends 4.200s"},
		{"no frames at all", src, sourceVideoScan{}, "has no frames"},
		{"a damaged encode", src, sourceVideoScan{Packets: 100, End: 4, LastFrame: 0.04, sawKey: true, DemuxErrors: 2, FirstError: "invalid"}, "is damaged"},
		{"timestamps rounded by a millisecond", src, sourceVideoScan{Packets: 100, Start: 0, End: 4.0009, LastFrame: 0.04, sawKey: true}, ""},
		// Two packets before the first keyframe: a decoder shows nothing
		// for them, so the encode may start two frames later.
		{"leading non-keyframes skipped", sourceVideoScan{Packets: 102, Start: 0, End: 4.08, LastFrame: 0.04, LeadingNonKey: 2, sawKey: true}, sourceVideoScan{Packets: 100, Start: 0, End: 4, LastFrame: 0.04, sawKey: true}, ""},
		{"more than the leading non-keyframes missing", sourceVideoScan{Packets: 102, Start: 0, End: 4.08, LastFrame: 0.04, LeadingNonKey: 2, sawKey: true}, sourceVideoScan{Packets: 99, Start: 0, End: 3.96, LastFrame: 0.04, sawKey: true}, "99 frames"},
		{"leading non-keyframes kept but the end cut", sourceVideoScan{Packets: 102, Start: 0, End: 4.08, LastFrame: 0.04, LeadingNonKey: 2, sawKey: true}, sourceVideoScan{Packets: 102, Start: 0, End: 3.9, LastFrame: 0.04, sawKey: true}, "ends 3.900s"},
		{"an edit list's discarded frames", sourceVideoScan{Packets: 103, Start: -0.12, End: 4, LastFrame: 0.04, Discarded: 3, sawKey: true}, sourceVideoScan{Packets: 100, Start: 0, End: 4, LastFrame: 0.04, sawKey: true}, ""},
		// No keyframe flag at all: nothing says which frames a decoder
		// skips, so none may be missing.
		{"no keyframe flagged", sourceVideoScan{Packets: 100, End: 4, LastFrame: 0.04, LeadingNonKey: 100}, sourceVideoScan{Packets: 98, End: 3.92, LastFrame: 0.04, sawKey: true}, "98 frames"},
		{"a restarted timeline is held to its frames", sourceVideoScan{Packets: 100, End: 2, LastFrame: 0.04, Discontinuous: true, sawKey: true}, sourceVideoScan{Packets: 100, End: 4, LastFrame: 0.04, sawKey: true}, ""},
		{"a restarted timeline that lost frames", sourceVideoScan{Packets: 100, End: 2, LastFrame: 0.04, Discontinuous: true, sawKey: true}, sourceVideoScan{Packets: 90, End: 3.6, LastFrame: 0.04, sawKey: true}, "90 frames"},
		{"source not scanned", sourceVideoScan{}, sourceVideoScan{Packets: 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := videoCompleteness(tc.src, tc.cand)
			if tc.fail == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrSourceInvalid) || !strings.Contains(err.Error(), tc.fail) {
				t.Fatalf("error %v, want a preservation failure saying %q", err, tc.fail)
			}
		})
	}
}

// packetFixture is a source and a candidate described by injected probes and
// scans: a video, two copied audio tracks, a copied SubRip subtitle, a
// mov_text subtitle converted to SubRip, and a font.
type packetFixture struct {
	dir, source, candidate string
	src, cand              SourceProbe
	srcVideo, candVideo    sourceVideoScan
	srcAudio, candAudio    map[int]audioPacketSpan
	srcSubs, candSubs      map[int]subtitlePackets
	scans                  []string
}

func newPacketFixture(t *testing.T) *packetFixture {
	t.Helper()
	dir := t.TempDir()
	start := func(v float64) *float64 { return &v }
	streams := func() []SourceStream {
		return []SourceStream{
			{Kind: "video", Index: 0, TypeIndex: 0, Codec: "hevc", Width: 1920, Height: 1080, StartTime: start(0)},
			{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "eac3", Channels: 6, Layout: "5.1(side)", Language: "eng", Title: "English", Disposition: map[string]int{"default": 1}},
			{Kind: "audio", Index: 2, TypeIndex: 1, Codec: "ac3", Channels: 2, Layout: "stereo", Language: "eng", Title: "Commentary", Disposition: map[string]int{"comment": 1}},
			{Kind: "subtitle", Index: 3, TypeIndex: 0, Codec: "subrip", Language: "eng", Title: "SDH", Disposition: map[string]int{"hearing_impaired": 1}},
			{Kind: "subtitle", Index: 4, TypeIndex: 1, Codec: "mov_text", Language: "spa"},
			{Kind: "attachment", Index: 5, TypeIndex: 0, Codec: "ttf", Filename: "font.ttf", MimeType: "font/ttf", AttachmentBytes: 1234},
		}
	}
	f := &packetFixture{
		dir:       dir,
		source:    ioOptWriteFile(t, filepath.Join(dir, "source.mp4"), "source"),
		candidate: ioOptWriteFile(t, filepath.Join(dir, "candidate.mkv"), "candidate"),
		srcVideo:  sourceVideoScan{Packets: 250, End: 10, LastFrame: 0.04, sawKey: true},
		candVideo: sourceVideoScan{Packets: 250, End: 10, LastFrame: 0.04, sawKey: true},
		srcAudio:  map[int]audioPacketSpan{1: {start: 0, end: 10, bytes: 800000, packets: 313}, 2: {start: 0, end: 10, bytes: 240000, packets: 313}},
		candAudio: map[int]audioPacketSpan{1: {start: 0, end: 10, bytes: 800000, packets: 313}, 2: {start: 0, end: 10, bytes: 240000, packets: 313}},
		// The mov_text track holds 12 lines and 11 empty gap samples.
		srcSubs:  map[int]subtitlePackets{3: {packets: 40, text: 40}, 4: {packets: 23, text: 12}},
		candSubs: map[int]subtitlePackets{3: {packets: 40, text: 40}, 4: {packets: 12, text: 12}},
	}
	f.src = SourceProbe{Path: f.source, Streams: streams(), DurationSec: 10}
	f.cand = SourceProbe{Path: f.candidate, Streams: streams(), DurationSec: 10}
	f.cand.Streams[4].Codec = "subrip"
	return f
}

func (f *packetFixture) plan(t *testing.T) *transcode.Plan {
	t.Helper()
	p := ioOptPlan(t)
	p.SubtitleActions = []transcode.SubtitleAction{
		{TypeIndex: 0, SourceStreamIndex: 3, SourceCodec: "subrip", Operation: "copy", Codec: "copy"},
		{TypeIndex: 1, SourceStreamIndex: 4, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip"},
	}
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

func (f *packetFixture) validate(t *testing.T) error {
	t.Helper()
	w := NewWorker(pr6b2Config(f.dir, nil))
	w.probeDetails = func(ctx context.Context, path string) (SourceProbe, error) {
		if path != f.candidate {
			t.Fatalf("probed %s", path)
		}
		return f.cand, nil
	}
	pick := func(path string) bool {
		f.scans = append(f.scans, filepath.Base(path))
		return path == f.candidate
	}
	w.scanVideo = func(ctx context.Context, path string, index int) (sourceVideoScan, error) {
		if pick(path) {
			return f.candVideo, nil
		}
		return f.srcVideo, nil
	}
	w.scanAudio = func(ctx context.Context, path string) (map[int]audioPacketSpan, error) {
		if pick(path) {
			return f.candAudio, nil
		}
		return f.srcAudio, nil
	}
	w.scanSubtitles = func(ctx context.Context, path string) (map[int]subtitlePackets, error) {
		if pick(path) {
			return f.candSubs, nil
		}
		return f.srcSubs, nil
	}
	plan := f.plan(t)
	ep, err := BuildExecutionPlan(plan, f.src.Streams, f.src.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.validateEncodedCandidateFull(context.Background(), f.candidate, ep, f.src, 0)
	return err
}

// Each kept track is measured from its packets on a plan that copies its
// audio, which used to skip the packet check: a candidate that lost packets
// of any track fails as a preservation failure, whatever its probe says.
func TestCandidateTrackPackets(t *testing.T) {
	good := newPacketFixture(t)
	if err := good.validate(t); err != nil {
		t.Fatalf("the faithful candidate was rejected: %v", err)
	}
	for _, kind := range []string{"source.mp4", "candidate.mkv"} {
		n := 0
		for _, s := range good.scans {
			if s == kind {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("%s scanned %d times, want video, audio and subtitles once each: %v", kind, n, good.scans)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*packetFixture)
		fail   string
	}{
		{"video ends early behind full-length audio", func(f *packetFixture) {
			f.candVideo = sourceVideoScan{Packets: 200, End: 8, LastFrame: 0.04, sawKey: true}
		}, "200 frames, the source 250"},
		{"a copied audio track lost packets", func(f *packetFixture) {
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 230000, packets: 300}
		}, "copied audio stream 1 has 300 packets, the source track 313"},
		{"a copied audio track starts late", func(f *packetFixture) {
			f.candAudio[2] = audioPacketSpan{start: 0.5, end: 10.5, bytes: 240000, packets: 313}
		}, "A/V offset changed"},
		{"E-AC-3 copied as AC-3", func(f *packetFixture) { f.cand.Streams[1].Codec = "ac3" }, "audio codec \"ac3\""},
		{"a copied subtitle lost lines", func(f *packetFixture) { f.candSubs[3] = subtitlePackets{packets: 39, text: 39} }, "copied subtitle stream 0 has 39 lines, the source track 40"},
		{"a converted subtitle lost a line", func(f *packetFixture) { f.candSubs[4] = subtitlePackets{packets: 11, text: 11} }, "converted subtitle stream 1 has 11 lines, the source track 12"},
		{"a converted subtitle kept the gap samples", func(f *packetFixture) { f.candSubs[4] = subtitlePackets{packets: 23, text: 12} }, "has 23 lines"},
		{"a subtitle came out empty", func(f *packetFixture) { delete(f.candSubs, 3) }, "came out empty"},
		{"a subtitle lost its language", func(f *packetFixture) { f.cand.Streams[4].Language = "" }, "subtitle language mismatch"},
		{"a copied subtitle lost its name", func(f *packetFixture) { f.cand.Streams[3].Title = "" }, "copied subtitle stream 0 is named \"\""},
		{"a copied subtitle lost hearing_impaired", func(f *packetFixture) { f.cand.Streams[3].Disposition = nil }, "subtitle disposition hearing_impaired"},
		{"a copied subtitle became forced", func(f *packetFixture) {
			f.cand.Streams[3].Disposition = map[string]int{"hearing_impaired": 1, "forced": 1}
		}, "subtitle disposition forced"},
		{"a copied audio track lost its name", func(f *packetFixture) { f.cand.Streams[2].Title = "" }, "copied audio stream 1 is named"},
		{"a copied audio track lost comment", func(f *packetFixture) { f.cand.Streams[2].Disposition = nil }, "audio disposition comment"},
		{"a font replaced by another of the same count", func(f *packetFixture) {
			f.cand.Streams[5].Filename = "other.ttf"
		}, "attachment \"font.ttf\""},
		{"a font truncated", func(f *packetFixture) { f.cand.Streams[5].AttachmentBytes = 1000 }, "attachment \"font.ttf\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPacketFixture(t)
			tc.mutate(f)
			err := f.validate(t)
			if err == nil || !strings.Contains(err.Error(), tc.fail) {
				t.Fatalf("error %v, want one saying %q", err, tc.fail)
			}
			// The A/V offset check predates this one and keeps its plain
			// error; every new check is a preservation failure.
			if tc.fail != "A/V offset changed" && !errors.Is(err, ErrSourceInvalid) {
				t.Fatalf("not a preservation failure: %v", err)
			}
		})
	}
}

// ffmpeg marks the first subtitle default on its own when none of the
// output's is, and the worker's command leaves subtitle flags alone unless
// the plan moved the default. That flag is no change of the track.
func TestCopiedSubtitleAutomaticDefault(t *testing.T) {
	f := newPacketFixture(t)
	f.cand.Streams[3].Disposition = map[string]int{"hearing_impaired": 1, "default": 1}
	if err := f.validate(t); err != nil {
		t.Fatalf("ffmpeg's automatic default rejected: %v", err)
	}
	// On the second subtitle it is not ffmpeg's doing.
	f = newPacketFixture(t)
	f.src.Streams[4].Codec, f.cand.Streams[4].Codec = "subrip", "subrip"
	f.srcSubs[4] = subtitlePackets{packets: 12, text: 12}
	f.cand.Streams[4].Disposition = map[string]int{"default": 1}
	p := f.plan(t)
	p.SubtitleActions[1] = transcode.SubtitleAction{TypeIndex: 1, SourceStreamIndex: 4, SourceCodec: "subrip", Operation: "copy", Codec: "copy"}
	if err := subtitleTrackChecks(p, f); err == nil || !strings.Contains(err.Error(), "subtitle disposition default changed at stream 1") {
		t.Fatalf("a default on the second subtitle passed: %v", err)
	}
}

func subtitleTrackChecks(p *transcode.Plan, f *packetFixture) error {
	return validateSubtitlePreservation(p, f.src.Streams, f.cand.Streams)
}

// A plan whose subtitle has no action, which the build step refuses, never
// passes the candidate check either.
func TestSubtitleWithoutAction(t *testing.T) {
	f := newPacketFixture(t)
	p := f.plan(t)
	p.SubtitleActions = p.SubtitleActions[:1]
	// The legacy shim derives a missing action; a recipe's plan never does.
	p.RecipeVersion = "recipe-v1"
	if err := subtitleTrackChecks(p, f); err == nil || !errors.Is(err, ErrSourceInvalid) || !strings.Contains(err.Error(), "subtitle stream 1 has no planned action") {
		t.Fatalf("a subtitle without an action passed: %v", err)
	}
	if err := subtitleEvents(p, f.src.Streams, f.cand.Streams, f.srcSubs, f.candSubs); err == nil || !strings.Contains(err.Error(), "no planned action") {
		t.Fatalf("the packet check passed a subtitle without an action: %v", err)
	}
	// A legacy worker-shim plan derives its actions, as its build does.
	legacy := ioOptPlan(t)
	legacy.SubtitleActions = nil
	if legacy.RecipeVersion != "legacy-worker-shim" {
		t.Skipf("hevc-vt plan is %q", legacy.RecipeVersion)
	}
	if err := subtitleTrackChecks(legacy, f); err != nil {
		t.Fatalf("legacy shim plan rejected: %v", err)
	}
}

// mov_text writes an empty two-byte sample into every gap between lines, and
// converting the track to SubRip drops them: the scan counts the samples that
// hold text, which is what the converted track has, on files ffmpeg made.
func TestRealSubtitlePacketScan(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "lines.srt")
	body := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:04,000 --> 00:00:05,000\nWorld\n\n3\n00:00:07,000 --> 00:00:08,500\nAgain\n"
	if err := os.WriteFile(srt, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	mp4, mkv := filepath.Join(dir, "source.mp4"), filepath.Join(dir, "converted.mkv")
	run("-f", "lavfi", "-i", "testsrc=d=10:s=64x48:r=10", "-i", srt, "-map", "0", "-map", "1", "-c:v", "mpeg4", "-c:s", "mov_text", mp4)
	run("-i", mp4, "-map", "0", "-c:v", "copy", "-c:s", "subrip", mkv)
	src, err := scanSubtitlePackets(context.Background(), ffprobe, mp4)
	if err != nil {
		t.Fatal(err)
	}
	cand, err := scanSubtitlePackets(context.Background(), ffprobe, mkv)
	if err != nil {
		t.Fatal(err)
	}
	if src[1].packets <= src[1].text || src[1].text != 3 {
		t.Fatalf("mov_text source scan %+v: want 3 lines and gap samples", src[1])
	}
	if cand[1].packets != 3 {
		t.Fatalf("converted track scan %+v: want 3 lines", cand[1])
	}
	plan := &transcode.Plan{SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 1, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip"}}}
	srcStreams := []SourceStream{{Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "mov_text"}}
	candStreams := []SourceStream{{Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "subrip"}}
	if err := subtitleEvents(plan, srcStreams, candStreams, src, cand); err != nil {
		t.Fatalf("a faithful conversion was rejected: %v", err)
	}
}
