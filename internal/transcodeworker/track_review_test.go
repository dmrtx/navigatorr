package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// A source whose audio tracks carry no default flag gets every kept track's
// flags written as they are ("0"): an explicit flag stops ffmpeg from marking
// the first audio track default on its own, so the copy keeps the source's
// flags and the candidate check holds it to them.
func TestNoDefaultAudioSourceWritesEveryFlag(t *testing.T) {
	none := func() map[string]int { return map[string]int{"default": 0, "forced": 0, "comment": 0} }
	streams := []SourceStream{
		{Kind: "video", Index: 0, TypeIndex: 0, Codec: "hevc", Width: 1920, Height: 1080},
		{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "eac3", Channels: 6, Disposition: none()},
		{Kind: "audio", Index: 2, TypeIndex: 1, Codec: "ac3", Channels: 2, Disposition: none()},
	}
	ep, err := BuildExecutionPlan(ioOptPlan(t), streams, 10)
	if err != nil {
		t.Fatal(err)
	}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" -disposition:a:0 0 ", " -disposition:a:1 0 "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

// ffmpeg marks the first audio track default only when the command wrote no
// flags for it, which happens only when the probe reported none (a kept
// track's planned flags are nil). A track whose flags were written must come
// out with them.
func TestInferredAudioDefault(t *testing.T) {
	src := []SourceStream{
		{Kind: "video", Index: 0, TypeIndex: 0, Codec: "hevc"},
		{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "eac3", Channels: 6},
		{Kind: "audio", Index: 2, TypeIndex: 1, Codec: "ac3", Channels: 2},
	}
	cand := func(first map[string]int) []SourceStream {
		out := append([]SourceStream(nil), src...)
		out[1].Disposition = first
		return out
	}
	plan := &transcode.Plan{}
	// No flags reported, none written: ffmpeg's default on the first track
	// is its own.
	if err := validateAudioPreservation(plan, src, cand(map[string]int{"default": 1})); err != nil {
		t.Fatalf("ffmpeg's inferred default rejected: %v", err)
	}
	// On the second track it is not ffmpeg's doing.
	second := append([]SourceStream(nil), src...)
	second[2].Disposition = map[string]int{"default": 1}
	if err := validateAudioPreservation(plan, src, second); err == nil || !strings.Contains(err.Error(), "audio disposition default changed at stream 1") {
		t.Fatalf("a default on the second audio track passed: %v", err)
	}
	// Flags reported (and so written as "0"): a default is a change.
	flagged := append([]SourceStream(nil), src...)
	flagged[1].Disposition = map[string]int{"default": 0}
	flagged[2].Disposition = map[string]int{"default": 0}
	err := validateAudioPreservation(plan, flagged, cand(map[string]int{"default": 1}))
	if err == nil || !errors.Is(err, ErrSourceInvalid) || !strings.HasPrefix(err.Error(), "audio disposition check failed: ") {
		t.Fatalf("a default the command did not write passed: %v", err)
	}
}

// A copied track's codec is the source's, exactly; a converted one's is the
// codec its encoder writes.
func TestCodecMatches(t *testing.T) {
	for _, tc := range []struct {
		expected, got string
		want          bool
	}{
		{"libx265", "hevc", true},
		{"hevc_videotoolbox", "hevc", true},
		{"libx264", "h264", true},
		{"aac_at", "aac", true},
		{"aac", "aac", true},
		{"libopus", "opus", true},
		{"srt", "subrip", true},
		{"subrip", "subrip", true},
		{"libx265", "h264", false},
		{"aac_at", "ac3", false},
		{"libopus", "vorbis", false},
	} {
		if got := codecMatches(tc.expected, tc.got); got != tc.want {
			t.Errorf("codecMatches(%q, %q) = %v", tc.expected, tc.got, got)
		}
	}
	for _, tc := range []struct {
		source, got string
		want        bool
	}{
		{"eac3", "eac3", true},
		{"EAC3", "eac3", true},
		{"eac3", "ac3", false},
		{"ac3", "eac3", false},
		{"dts", "", false},
		{"truehd", "truehd", true},
	} {
		if got := copiedCodecMatches(tc.source, tc.got); got != tc.want {
			t.Errorf("copiedCodecMatches(%q, %q) = %v", tc.source, tc.got, got)
		}
	}
}

// Attachments are held to the source's files. A cover the encode re-attaches
// is the cover art check's, not an extra file, and a source without
// attachments has none to lose.
func TestAttachmentsPreserved(t *testing.T) {
	font := func(name string, size int64) SourceStream {
		return SourceStream{Kind: "attachment", Filename: name, MimeType: "font/ttf", AttachmentBytes: size}
	}
	cover := SourceStream{Kind: "attachment", Filename: "cover.jpg", MimeType: "image/jpeg", AttachmentBytes: 5000}
	covers := []StreamAction{{Kind: "cover", Filename: "cover.jpg", MimeType: "IMAGE/JPEG"}}
	src := []SourceStream{font("a.ttf", 10), font("b.ttf", 20)}
	for _, tc := range []struct {
		name   string
		covers []StreamAction
		src    []SourceStream
		cand   []SourceStream
		fail   string
	}{
		{"the same fonts", nil, src, []SourceStream{font("b.ttf", 20), font("a.ttf", 10)}, ""},
		{"a re-attached cover beside them", covers, src, []SourceStream{font("a.ttf", 10), cover, font("b.ttf", 20)}, ""},
		{"a source without attachments", covers, nil, []SourceStream{cover}, ""},
		{"a source without attachments, the output with one", nil, nil, []SourceStream{font("x.ttf", 1)}, ""},
		{"a font missing", nil, src, []SourceStream{font("a.ttf", 10)}, "attachment count 1 != source 2"},
		{"a font missing behind a cover", covers, src, []SourceStream{font("a.ttf", 10), cover}, "attachment count 1 != source 2"},
		{"a font replaced", nil, src, []SourceStream{font("a.ttf", 10), font("c.ttf", 20)}, "attachment \"b.ttf\" (font/ttf, 20 bytes) is missing"},
		{"a font truncated", nil, src, []SourceStream{font("a.ttf", 10), font("b.ttf", 19)}, "attachment \"b.ttf\" (font/ttf, 20 bytes) is missing"},
		{"an extra file in place of the cover", covers, src, []SourceStream{font("a.ttf", 10), font("b.ttf", 20), font("x.ttf", 1)}, "attachment count 3 != source 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := attachmentsPreserved(tc.covers, tc.src, tc.cand)
			if tc.fail == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrSourceInvalid) || !strings.Contains(err.Error(), tc.fail) {
				t.Fatalf("error %v, want %q", err, tc.fail)
			}
		})
	}
}

// The line count of a converted subtitle is exact for mov_text to SubRip
// alone, the default recipe's one subtitle conversion. Any other conversion a
// recipe adds is held to being non-empty, not to a count no rule gives.
func TestConvertedSubtitleEvents(t *testing.T) {
	snap, err := recipe.Parse(recipe.EmbeddedBytes())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range snap.Bundle.Containers {
		for from, conv := range c.SubtitleConversions {
			if from != "mov_text" || conv.TargetCodec != "subrip" {
				t.Fatalf("container %s converts %s to %s: give subtitleEvents its count rule", name, from, conv.TargetCodec)
			}
		}
	}
	plan := &transcode.Plan{SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 1, SourceCodec: "ass", Operation: "transcode", Codec: "subrip"}}}
	srcStreams := []SourceStream{{Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "ass"}}
	candStreams := []SourceStream{{Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "subrip"}}
	src := map[int]subtitlePackets{1: {packets: 40, text: 40}}
	if err := subtitleEvents(plan, srcStreams, candStreams, src, map[int]subtitlePackets{1: {packets: 36, text: 36}}); err != nil {
		t.Fatalf("an ASS conversion that merged events was refused: %v", err)
	}
	if err := subtitleEvents(plan, srcStreams, candStreams, src, map[int]subtitlePackets{}); err == nil || !strings.Contains(err.Error(), "came out empty") {
		t.Fatalf("an empty converted track passed: %v", err)
	}
}

// A clip cut with a stream copy can start on an open picture group: its first
// keyframe's leading pictures refer to pictures that were cut away, and a
// decoder drops them. The decode of the head says how many; the packets alone
// bound it.
func TestStartDropped(t *testing.T) {
	// 100 packets at 25 FPS from 0.04 (the leading pictures show before the
	// keyframe at 0.08), the first group 24 packets long.
	src := sourceVideoScan{Packets: 100, Start: 0.04, End: 4.04, LastFrame: 0.04, sawKey: true, keys: 5, headPackets: 24, leadingPictures: 2, headDecoded: true}
	cand := func(packets int) sourceVideoScan {
		return sourceVideoScan{Packets: packets, Start: 0.08, End: 0.08 + float64(packets)*0.04, LastFrame: 0.04, sawKey: true}
	}
	for _, tc := range []struct {
		name    string
		frames  int // the pictures the head decodes to
		lead    int
		discard int
		cand    sourceVideoScan
		dropped int
		fail    string
	}{
		{"two leading pictures dropped (RASL)", 22, 0, 0, cand(98), 2, ""},
		{"two dropped and one more lost", 22, 0, 0, cand(97), 2, "97 frames, the source 100"},
		// Leading pictures that decode (RADL) excuse nothing.
		{"leading pictures that decode", 24, 0, 0, cand(98), 0, "98 frames, the source 100"},
		{"a closed group", 24, 0, 0, cand(100), 0, ""},
		// A field-coded head shows a picture per two packets: the packets
		// that can be dropped bound it.
		{"a field-coded head", 12, 0, 0, cand(98), 2, ""},
		{"a field-coded head with packets before its keyframe", 12, 1, 0, cand(97), 3, ""},
		// A discarded packet the decoder dropped is counted once.
		{"a discarded leading picture", 22, 0, 1, cand(98), 2, ""},
		{"a discarded leading picture, one more lost", 22, 0, 1, cand(97), 2, "97 frames"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := src
			s.headFrames, s.LeadingNonKey, s.Discarded, s.headDiscarded = tc.frames, tc.lead, tc.discard, tc.discard
			if dropped, _ := s.startDropped(); dropped != tc.dropped {
				t.Fatalf("dropped %d, want %d", dropped, tc.dropped)
			}
			err := videoCompleteness(s, tc.cand, 25)
			if tc.fail == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrContentNotPreserved) || !strings.Contains(err.Error(), tc.fail) {
				t.Fatalf("error %v, want %q", err, tc.fail)
			}
		})
	}
	// Without the decode the packets before the first keyframe are the
	// allowance, as before.
	undecoded := src
	undecoded.headDecoded = false
	if dropped, _ := undecoded.startDropped(); dropped != 0 {
		t.Fatalf("undecoded head dropped %d", dropped)
	}
}

// The preservation sentinel says what happened and still counts as the
// source sentinel the callers turn into preservation_not_met.
func TestContentNotPreservedError(t *testing.T) {
	err := checkInvalid("video completeness", fmt.Errorf("%w: the converted video has 3660 frames", ErrContentNotPreserved))
	if !errors.Is(err, ErrSourceInvalid) || !errors.Is(err, ErrContentNotPreserved) {
		t.Fatalf("%v is not a preservation failure", err)
	}
	if got, want := err.Error(), "video completeness check failed: the converted file lost content: the converted video has 3660 frames"; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
	if errors.Is(ErrSourceInvalid, ErrContentNotPreserved) {
		t.Fatal("every invalid source counts as lost content")
	}
}

// A real open-GOP clip: x264 with open picture groups, cut on a recovery
// point with a stream copy, as the acceptance clips are. The head decode finds
// the one leading B-frame the decoder drops; an encode of every picture the
// source shows passes, and one more frame lost fails.
func TestRealOpenGOPClip(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	if out, _ := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libx264") {
		t.Skip("libx264 unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	full, clip := filepath.Join(dir, "full.mkv"), filepath.Join(dir, "clip.mkv")
	run("-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-t", "6", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-x264-params", "open-gop=1:keyint=24:min-keyint=24:bframes=3:scenecut=0", full)
	run("-ss", "2.05", "-i", full, "-t", "3", "-c", "copy", "-avoid_negative_ts", "make_zero", clip)

	ctx := context.Background()
	scan, err := scanFilePackets(ctx, ffprobe, clip, packetScanRequest{video: 0})
	if err != nil {
		t.Fatal(err)
	}
	src := scan.video
	if err := decodeVideoHead(ctx, ffprobe, clip, 0, &src); err != nil {
		t.Fatal(err)
	}
	if !src.headDecoded || src.LeadingNonKey != 0 {
		t.Fatalf("clip scan %+v: want a keyframe first, its head decoded", src)
	}
	if src.leadingPictures == 0 {
		// Which leading pictures a stream copy keeps is the ffmpeg
		// build's choice; this one kept none.
		t.Skipf("the cut kept no leading pictures: %+v", src)
	}
	dropped, _ := src.startDropped()
	if dropped == 0 || dropped > src.leadingPictures {
		t.Fatalf("dropped %d, leading pictures %d", dropped, src.leadingPictures)
	}
	encode := func(name string, extra ...string) sourceVideoScan {
		t.Helper()
		out := filepath.Join(dir, name)
		args := append([]string{"-i", clip, "-map", "0:v:0"}, extra...)
		args = append(args, sourceTimestampArgs...)
		run(append(args, "-c:v", "libx264", "-preset", "ultrafast", out)...)
		got, err := scanFilePackets(ctx, ffprobe, out, packetScanRequest{video: 0})
		if err != nil {
			t.Fatal(err)
		}
		return got.video
	}
	if err := videoCompleteness(src, encode("whole.mkv"), 24); err != nil {
		t.Fatalf("an encode of every picture the clip shows was refused: %v", err)
	}
	lost := encode("lost.mkv", "-vf", "select=not(eq(n\\,30))")
	if err := videoCompleteness(src, lost, 24); err == nil || !strings.Contains(err.Error(), "frames") {
		t.Fatalf("an encode that lost a frame passed: %v", err)
	}
}
