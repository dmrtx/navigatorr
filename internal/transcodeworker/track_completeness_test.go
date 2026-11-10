package transcodeworker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

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
		srcVideo:  decoded(frames(250, 0, 0.04), 250),
		candVideo: frames(250, 0, 0.04),
		srcAudio:  map[int]audioPacketSpan{1: {start: 0, end: 10, bytes: 800000, packets: 313, timed: 313}, 2: {start: 0, end: 10, bytes: 240000, packets: 313, timed: 313}},
		candAudio: map[int]audioPacketSpan{1: {start: 0, end: 10, bytes: 800000, packets: 313, timed: 313}, 2: {start: 0, end: 10, bytes: 240000, packets: 313, timed: 313}},
		// The mov_text track holds 12 lines and 11 empty gap samples: its
		// conversion, run before the encode, wrote 12.
		srcSubs:  map[int]subtitlePackets{3: {packets: 40}, 4: {packets: 23, events: 12, counted: true}},
		candSubs: map[int]subtitlePackets{3: {packets: 40}, 4: {packets: 12}},
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
	_, err = w.validateEncodedCandidateFull(context.Background(), f.candidate, ep, prescanSource(t, w, plan, f.src, f.source), 0)
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
	// Each refusal names its check in front of its numbers: the coordinator
	// shows the owner which one refused the encode.
	for _, tc := range []struct {
		name   string
		mutate func(*packetFixture)
		check  string
		fail   string
	}{
		{"video ends early behind full-length audio", func(f *packetFixture) {
			f.candVideo = frames(200, 0, 0.04)
		}, "video completeness", "200 frames, the source shows 250"},
		{"a copied audio track lost packets", func(f *packetFixture) {
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 230000, packets: 300, timed: 300}
		}, "audio packets", "copied audio stream 1 has 300 packets, the source track 313"},
		{"a copied audio track lost more packets than its discarded ones", func(f *packetFixture) {
			f.srcAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 240000, packets: 313, timed: 311, discarded: 2}
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 239000, packets: 310, timed: 310}
		}, "audio packets", "copied audio stream 1 has 310 packets, the source track 313"},
		{"a copied audio track gained packets", func(f *packetFixture) {
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 240800, packets: 314, timed: 314}
		}, "audio packets", "copied audio stream 1 has 314 packets"},
		{"E-AC-3 copied as AC-3", func(f *packetFixture) { f.cand.Streams[1].Codec = "ac3" }, "audio tracks", "audio codec \"ac3\""},
		{"a copied audio track cut short", func(f *packetFixture) {
			f.candAudio[2] = audioPacketSpan{start: 0, end: 9.5, bytes: 240000, packets: 313, timed: 313}
		}, "audio packets", "copied audio stream 1 lasts 9.500s, the source track 10.000s"},
		{"a copied audio track longer than its untimed packets", func(f *packetFixture) {
			f.srcAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 240000, packets: 313, timed: 311, untimed: 2}
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10.5, bytes: 240000, packets: 313, timed: 313}
		}, "audio packets", "copied audio stream 1 lasts 10.500s"},
		{"a copied subtitle lost lines", func(f *packetFixture) { f.candSubs[3] = subtitlePackets{packets: 39} }, "subtitle events", "copied subtitle stream 0 has 39 lines, the source track 40"},
		{"a copied subtitle lost more than its discarded packets", func(f *packetFixture) {
			f.srcSubs[3] = subtitlePackets{packets: 40, discarded: 2}
			f.candSubs[3] = subtitlePackets{packets: 37}
		}, "subtitle events", "copied subtitle stream 0 has 37 lines, the source track 38 to 40"},
		{"a converted subtitle lost a line", func(f *packetFixture) { f.candSubs[4] = subtitlePackets{packets: 11} }, "subtitle events", "converted subtitle stream 1 has 11 lines, the source track 12"},
		{"a converted subtitle kept the gap samples", func(f *packetFixture) { f.candSubs[4] = subtitlePackets{packets: 23} }, "subtitle events", "has 23 lines"},
		{"a converted subtitle no conversion counted", func(f *packetFixture) { f.srcSubs[4] = subtitlePackets{packets: 23} }, "subtitle events", "had its lines counted by no conversion"},
		{"a subtitle came out empty", func(f *packetFixture) { delete(f.candSubs, 3) }, "subtitle events", "came out empty"},
		{"a subtitle lost its language", func(f *packetFixture) { f.cand.Streams[4].Language = "" }, "subtitle tracks", "subtitle language mismatch"},
		{"a copied subtitle lost its name", func(f *packetFixture) { f.cand.Streams[3].Title = "" }, "subtitle title", "subtitle stream 0 is named \"\", the source track \"SDH\""},
		{"a copied subtitle lost hearing_impaired", func(f *packetFixture) { f.cand.Streams[3].Disposition = nil }, "subtitle disposition", "subtitle disposition hearing_impaired"},
		{"a copied subtitle became forced", func(f *packetFixture) {
			f.cand.Streams[3].Disposition = map[string]int{"hearing_impaired": 1, "forced": 1}
		}, "subtitle disposition", "subtitle disposition forced"},
		{"a copied audio track lost its name", func(f *packetFixture) { f.cand.Streams[2].Title = "" }, "audio title", "audio stream 1 is named \"\", the plan names it \"Commentary\""},
		{"a copied audio track lost comment", func(f *packetFixture) { f.cand.Streams[2].Disposition = nil }, "audio disposition", "audio disposition comment"},
		{"a font replaced by another of the same count", func(f *packetFixture) {
			f.cand.Streams[5].Filename = "other.ttf"
		}, "attachments", "Attachment \"font.ttf\" (font/ttf, 1234 bytes) is missing"},
		{"a font truncated", func(f *packetFixture) { f.cand.Streams[5].AttachmentBytes = 1000 }, "attachments", "Attachment \"font.ttf\" (font/ttf, 1234 bytes) is missing"},
		{"a font lost", func(f *packetFixture) { f.cand.Streams = f.cand.Streams[:5] }, "attachments", "the source has 1, the output 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPacketFixture(t)
			tc.mutate(f)
			err := f.validate(t)
			if err == nil || !strings.Contains(err.Error(), tc.fail) || !errors.Is(err, ErrSourceInvalid) {
				t.Fatalf("error %v, want a preservation failure saying %q", err, tc.fail)
			}
			if !strings.HasPrefix(err.Error(), tc.check+" check failed: ") {
				t.Fatalf("error %q does not name the %s check", err, tc.check)
			}
		})
	}

	// What the copy really kept passes.
	for _, tc := range []struct {
		name   string
		mutate func(*packetFixture)
	}{
		// A copied track keeps the source's timestamps: its start is not
		// re-timed against the video, only its packets and length count.
		{"a copied audio track that starts where the source's does, late", func(f *packetFixture) {
			f.srcAudio[2] = audioPacketSpan{start: 0.5, end: 10.5, bytes: 240000, packets: 313, timed: 313}
			f.candAudio[2] = audioPacketSpan{start: 0.5, end: 10.5, bytes: 240000, packets: 313, timed: 313}
		}},
		{"a copied audio track shifted as a whole", func(f *packetFixture) {
			f.candAudio[2] = audioPacketSpan{start: 0.5, end: 10.5, bytes: 240000, packets: 313, timed: 313}
		}},
		// Packets the source marks discard a decoder never plays: a copy
		// may leave them out.
		{"a copied audio track without its discarded packets", func(f *packetFixture) {
			f.srcAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 240000, packets: 313, timed: 311, discarded: 2}
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 239500, packets: 311, timed: 311}
		}},
		// A packet with no time is copied and timed: the copy runs longer
		// by it.
		{"a copied audio track with untimed packets", func(f *packetFixture) {
			f.srcAudio[2] = audioPacketSpan{start: 0, end: 10, bytes: 240000, packets: 313, timed: 311, untimed: 2}
			f.candAudio[2] = audioPacketSpan{start: 0, end: 10.064, bytes: 240000, packets: 313, timed: 313}
		}},
		// Whatever packets a copy carries, a track with none timed on
		// either side is held to its count alone.
		{"a copied audio track with no timed packet", func(f *packetFixture) {
			f.srcAudio[2] = audioPacketSpan{packets: 313, untimed: 313}
			f.candAudio[2] = audioPacketSpan{packets: 313, untimed: 313}
		}},
		{"a copied subtitle without its discarded packets", func(f *packetFixture) {
			f.srcSubs[3] = subtitlePackets{packets: 40, discarded: 2}
			f.candSubs[3] = subtitlePackets{packets: 38}
		}},
		// A mov_text track of empty samples only converts to no line: the
		// track comes out empty, as the conversion run before the encode did.
		{"a converted subtitle of empty samples only", func(f *packetFixture) {
			f.srcSubs[4] = subtitlePackets{packets: 5, events: 0, counted: true}
			delete(f.candSubs, 4)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPacketFixture(t)
			tc.mutate(f)
			if err := f.validate(t); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

// ffmpeg marks the first of two or more subtitles default on its own when
// none of them is, but only when the command sets no track's flags. The
// worker's command sets every audio track's flags it knows, so with them no
// default appears by itself; without them that flag is no change of the track.
func TestCopiedSubtitleAutomaticDefault(t *testing.T) {
	f := newPacketFixture(t)
	f.cand.Streams[3].Disposition = map[string]int{"hearing_impaired": 1, "default": 1}
	if err := f.validate(t); err == nil || !strings.Contains(err.Error(), "subtitle disposition default changed at stream 0") {
		t.Fatalf("a default ffmpeg could not have set passed: %v", err)
	}
	f = newPacketFixture(t)
	for _, streams := range [][]SourceStream{f.src.Streams, f.cand.Streams} {
		streams[1].Disposition, streams[2].Disposition = nil, nil
	}
	f.cand.Streams[3].Disposition = map[string]int{"hearing_impaired": 1, "default": 1}
	if err := f.validate(t); err != nil {
		t.Fatalf("ffmpeg's automatic default rejected: %v", err)
	}
	// On the second subtitle it is not ffmpeg's doing.
	f = newPacketFixture(t)
	f.src.Streams[4].Codec, f.cand.Streams[4].Codec = "subrip", "subrip"
	f.srcSubs[4] = subtitlePackets{packets: 12}
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
