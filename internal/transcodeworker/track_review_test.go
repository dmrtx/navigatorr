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
	// Flags never probed have nothing to compare: the copy carries the
	// source's own, whichever track they mark.
	second := append([]SourceStream(nil), src...)
	second[2].Disposition = map[string]int{"default": 1}
	if err := validateAudioPreservation(plan, src, second); err != nil {
		t.Fatalf("a default on a track whose source flags are unknown was refused: %v", err)
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
// codec its encoder writes. A converted subtitle's is held by
// transcode.SameConvertedSubtitleCodec, the coordinator's rule too.
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
		{"", "", false},
		{"", "aac", false},
		{"truehd", "truehd", true},
		// ffmpeg writes a plain-text track as S_TEXT/UTF8, read back as
		// subrip, and SSA as ASS.
		{"text", "subrip", true},
		{"ssa", "ass", true},
		{"subrip", "ass", false},
		{"text", "ass", false},
	} {
		if got := copiedCodecMatches(tc.source, tc.got); got != tc.want {
			t.Errorf("copiedCodecMatches(%q, %q) = %v", tc.source, tc.got, got)
		}
	}
}

// Attachments are held to the source's files by the coordinator's rule: as
// many, each by name, type and size. A file the source has not is an extra
// one, whether or not the source has attachments.
func TestAttachmentsPreserved(t *testing.T) {
	font := func(name string, size int64) SourceStream {
		return SourceStream{Kind: "attachment", Filename: name, MimeType: "font/ttf", AttachmentBytes: size}
	}
	src := []SourceStream{font("a.ttf", 10), font("b.ttf", 20)}
	for _, tc := range []struct {
		name string
		src  []SourceStream
		cand []SourceStream
		fail string
	}{
		{"the same fonts", src, []SourceStream{font("b.ttf", 20), font("a.ttf", 10)}, ""},
		{"no attachments", nil, nil, ""},
		{"a source without attachments, the output with one", nil, []SourceStream{font("x.ttf", 1)}, "the source has 0, the output 1"},
		{"a font missing", src, []SourceStream{font("a.ttf", 10)}, "the source has 2, the output 1"},
		{"a font replaced", src, []SourceStream{font("a.ttf", 10), font("c.ttf", 20)}, "Attachment \"b.ttf\" (font/ttf, 20 bytes) is missing"},
		{"a font truncated", src, []SourceStream{font("a.ttf", 10), font("b.ttf", 19)}, "Attachment \"b.ttf\" (font/ttf, 20 bytes) is missing"},
		{"an extra file", src, []SourceStream{font("a.ttf", 10), font("b.ttf", 20), font("x.ttf", 1)}, "the source has 2, the output 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := attachmentsPreserved(tc.src, tc.cand)
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
}

// Every converted subtitle is held to the exact lines its conversion wrote
// when it was run before the encode, whatever the formats: an ASS track
// whose events merged passes at the merged count, and one line kept of
// many never passes for being non-empty.
func TestConvertedSubtitleEvents(t *testing.T) {
	plan := &transcode.Plan{SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 1, SourceCodec: "ass", Operation: "transcode", Codec: "subrip"}}}
	srcStreams := []SourceStream{{Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "ass"}}
	candStreams := []SourceStream{{Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "subrip"}}
	src := map[int]subtitlePackets{1: {packets: 1500, events: 1436, counted: true}}
	for _, tc := range []struct {
		name string
		cand subtitlePackets
		fail string
	}{
		{"the counted lines", subtitlePackets{packets: 1436}, ""},
		{"one line of many", subtitlePackets{packets: 1}, "converted subtitle stream 0 has 1 lines, the source track 1436"},
		{"one line more", subtitlePackets{packets: 1437}, "has 1437 lines"},
		{"empty", subtitlePackets{}, "came out empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := subtitleEvents(plan, srcStreams, candStreams, src, map[int]subtitlePackets{1: tc.cand})
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
	uncounted := map[int]subtitlePackets{1: {packets: 1500}}
	if err := subtitleEvents(plan, srcStreams, candStreams, uncounted, map[int]subtitlePackets{1: {packets: 1500}}); err == nil || !strings.Contains(err.Error(), "counted by no conversion") {
		t.Fatalf("a converted track no conversion counted passed: %v", err)
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
	_, leading, _ := src.headStats(src.headPackets)
	if leading == 0 {
		// Which leading pictures a stream copy keeps is the ffmpeg
		// build's choice; this one kept none.
		t.Skipf("the cut kept no leading pictures: %+v", src)
	}
	// The decoder dropped some of the leading pictures: the source shows
	// exactly that many fewer than its packets.
	p := sourcePictures(src, "h264", 24)
	if dropped := src.Packets - p.most; p.least != p.most || p.fields || dropped == 0 || dropped > leading {
		t.Fatalf("pictures %+v of %d packets, %d leading", p, src.Packets, leading)
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
	if err := videoCompleteness(src, encode("whole.mkv"), "h264", 24); err != nil {
		t.Fatalf("an encode of every picture the clip shows was refused: %v", err)
	}
	lost := encode("lost.mkv", "-vf", "select=not(eq(n\\,30))")
	if err := videoCompleteness(src, lost, "h264", 24); err == nil || !strings.Contains(err.Error(), "frames") {
		t.Fatalf("an encode that lost a frame passed: %v", err)
	}
}
