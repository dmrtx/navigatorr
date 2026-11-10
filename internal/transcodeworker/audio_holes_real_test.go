package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// holeSource is a real source for the audio hole checks: ten-odd seconds of
// noise in one codec and container, with, when asked, a stretch of its audio
// packets overwritten with noise (corrupt, so the decoder skips them) or
// moved apart in time (a pts gap the source itself carries).
type holeSource struct {
	codec, container string
	channels         int
	corruptFrom, to  int // packet numbers whose bytes are replaced
	gapAt, gapLen    float64
}

type holeTools struct{ ffmpeg, ffprobe string }

func holeToolsOrSkip(t *testing.T, codecs ...string) holeTools {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	available, err := ProbeAvailableEncoders(context.Background(), ffmpeg)
	if err != nil || !available["libx265"] || !available["aac"] {
		t.Skip("libx265/AAC unavailable")
	}
	for _, c := range codecs {
		if !available[c] {
			t.Skipf("%s encoder unavailable", c)
		}
	}
	return holeTools{ffmpeg, ffprobe}
}

func (h holeTools) run(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(h.ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// build writes the source into dir and returns its path.
func (h holeTools) build(t *testing.T, dir string, s holeSource, seconds int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("source-%s-%d.%s", s.codec, s.channels, s.container))
	audio := []string{"-c:a", s.codec}
	switch s.codec {
	case "truehd", "dca":
		audio = append(audio, "-strict", "-2")
	case "ac3", "eac3":
		audio = append(audio, "-b:a", "384k")
	}
	if s.corruptFrom < s.to {
		audio = append(audio, "-bsf:a", fmt.Sprintf("noise=amount='if(between(n,%d,%d),1,0)'", s.corruptFrom, s.to))
	}
	whole := filepath.Join(dir, "whole-"+filepath.Base(path))
	if s.gapLen == 0 {
		args := []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=24:duration=%d", seconds), "-f", "lavfi", "-i", layoutNoise(s.channels, seconds), "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast"}
		args = append(args, audio...)
		h.run(t, append(args, path)...)
		return path
	}
	// A gap: the audio in two pieces, the second one starting gapLen later
	// (the concat demuxer places a piece where the durations before it end).
	pieces := [2]string{filepath.Join(dir, "piece1.mka"), filepath.Join(dir, "piece2.mka")}
	h.run(t, append([]string{"-f", "lavfi", "-i", layoutNoise(s.channels, seconds)}, append(audio, pieces[0])...)...)
	h.run(t, "-ss", strconv.FormatFloat(s.gapAt, 'f', 3, 64), "-i", pieces[0], "-c", "copy", pieces[1])
	h.run(t, "-t", strconv.FormatFloat(s.gapAt, 'f', 3, 64), "-i", pieces[0], "-c", "copy", whole+".1.mka")
	list := filepath.Join(dir, "list.txt")
	body := fmt.Sprintf("file '%s'\nduration %.3f\nfile '%s'\n", whole+".1.mka", s.gapAt+s.gapLen, pieces[1])
	if err := os.WriteFile(list, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(t, "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=24:duration=%d", seconds), "-f", "concat", "-safe", "0", "-i", list, "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "copy", path)
	return path
}

// convert runs the worker's own plan and argument builder on source, with
// every audio track made AAC at 192 kb/s, and returns the candidate.
func (h holeTools) convert(t *testing.T, source string) (string, *transcode.Plan, SourceProbe, SourceProbe) {
	t.Helper()
	dir := filepath.Dir(source)
	probe, err := ProbeSourceDetails(context.Background(), h.ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 23, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ep.Streams {
		if s := &ep.Streams[i]; s.Kind == "audio" && s.TargetCodec == "copy" {
			s.TargetCodec, s.BitrateKbps = "aac", 192
		}
	}
	candidate := filepath.Join(dir, "candidate-"+strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))+".mkv")
	args, err := BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(h.ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("encode exited with an error: %v\n%s", err, out)
	}
	result, err := ProbeSourceDetails(context.Background(), h.ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	return candidate, plan, probe, result
}

// A conversion that skips a stretch of undecodable packets exits 0 and keeps
// the track's first and last times, so only the silence it opens inside the
// track shows it. A gap the source itself carries stays in the conversion and
// is not sound lost. Every case is a real source converted by the worker's own
// argument builder and read back by its own packet scan.
func TestConvertedAudioHolesRealConversions(t *testing.T) {
	holeToolsOrSkip(t)
	cases := []struct {
		holeSource
		wantLost bool
	}{
		{holeSource: holeSource{codec: "ac3", container: "mkv", channels: 6}},
		{holeSource: holeSource{codec: "ac3", container: "ts", channels: 6}},
		{holeSource: holeSource{codec: "eac3", container: "mkv", channels: 6}},
		{holeSource: holeSource{codec: "dca", container: "mkv", channels: 6}},
		{holeSource: holeSource{codec: "truehd", container: "mkv", channels: 2}},
		{holeSource: holeSource{codec: "flac", container: "mkv", channels: 2}},
		{holeSource: holeSource{codec: "aac", container: "mkv", channels: 2}},
		{holeSource: holeSource{codec: "aac", container: "ts", channels: 2}},
		// A gap in the source's own timestamps is kept as it is.
		{holeSource: holeSource{codec: "ac3", container: "mkv", channels: 6, gapAt: 5, gapLen: 3}},
		{holeSource: holeSource{codec: "dca", container: "mkv", channels: 6, gapAt: 5, gapLen: 3}},
		// Corrupt packets mid-stream: ffmpeg skips them and exits 0.
		{holeSource: holeSource{codec: "ac3", container: "mkv", channels: 6, corruptFrom: 150, to: 250}, wantLost: true},
		{holeSource: holeSource{codec: "flac", container: "mkv", channels: 2, corruptFrom: 100, to: 130}, wantLost: true},
		{holeSource: holeSource{codec: "dca", container: "mkv", channels: 6, corruptFrom: 300, to: 600}, wantLost: true},
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s-%s-corrupt%d-gap%.0f", c.codec, c.container, c.to-c.corruptFrom, c.gapLen)
		t.Run(name, func(t *testing.T) {
			h := holeToolsOrSkip(t, c.codec)
			dir := t.TempDir()
			source := h.build(t, dir, c.holeSource, 12)
			candidate, plan, probe, result := h.convert(t, source)
			src, err := audioPacketSpans(context.Background(), h.ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			cand, err := audioPacketSpans(context.Background(), h.ffprobe, candidate)
			if err != nil {
				t.Fatal(err)
			}
			x, y := src[1], cand[1]
			if math.Abs(x.seconds()-y.seconds()) > audioLengthSlack {
				t.Fatalf("the length moved (%.3fs against %.3fs): the case no longer shows what only the holes see", y.seconds(), x.seconds())
			}
			probe.audioSpans = src
			if c.wantLost && y.holes-x.holes < 0.5 {
				t.Fatalf("the conversion opened %.3fs of silence, want the skipped stretch", y.holes-x.holes)
			}
			if !c.wantLost && math.Abs(y.holes-x.holes) > 0.01 {
				t.Fatalf("a good conversion moved the silence by %.4fs (source %.3fs, candidate %.3fs)", y.holes-x.holes, x.holes, y.holes)
			}
			// Under the compact mode DTS, TrueHD and FLAC convert themselves;
			// the others are forced to AAC, so the converted-track check is
			// called as the validation calls it.
			err = convertedAudioPackets(0, 384, x, y, probe, result)
			if c.codec == "dca" || c.codec == "flac" || c.codec == "truehd" {
				err = validateAudioPackets(plan, probe, result, cand)
			}
			if c.wantLost {
				if err == nil || !errors.Is(err, ErrContentNotPreserved) || !strings.Contains(err.Error(), "silence between its packets") {
					t.Fatalf("a conversion that dropped sound was accepted or refused for another reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a good conversion was refused: %v", err)
			}
		})
	}
}

// The silence between packets is held to the source's: more than the slack is
// sound the conversion dropped, a gap the source has is not.
func TestConvertedAudioHolesAgainstSource(t *testing.T) {
	v := func(start float64) SourceProbe {
		return SourceProbe{Streams: []SourceStream{{Index: 0, Kind: "video", Codec: "h264", StartTime: &start}}}
	}
	span := func(holes float64) audioPacketSpan {
		return audioPacketSpan{start: 0, end: 600, holes: holes, bytes: 600 * 20000, packets: 28000, timed: 28000}
	}
	for _, c := range []struct {
		name       string
		src, cand  float64
		wantFailed bool
	}{
		{"no silence either side", 0, 0, false},
		{"the source's gap is kept", 3, 3.001, false},
		{"within the slack", 0, 0.05, false},
		{"a skipped stretch", 0, 2.6, true},
		{"one more packet than the slack", 0, audioHoleSlack + 0.01, true},
		{"a skipped stretch next to a gap", 3, 5.6, true},
		{"a closed gap is for the length check", 3, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := convertedAudioPackets(0, 384, span(c.src), span(c.cand), v(0), v(0))
			if c.wantFailed != (err != nil) {
				t.Fatalf("source %.3fs, candidate %.3fs of silence: error %v, want failure %v", c.src, c.cand, err, c.wantFailed)
			}
			if err != nil && !errors.Is(err, ErrContentNotPreserved) {
				t.Fatalf("refused as %v, want content not preserved", err)
			}
		})
	}
}
