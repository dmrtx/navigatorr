package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func lossless51Source(rate int) []SourceStream {
	return []SourceStream{
		{Kind: "video", Index: 0, TypeIndex: 0, Codec: "h264", Width: 1920, Height: 1080},
		{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "flac", Channels: 6, Layout: "5.1", Language: "jpn", BitrateKbps: 3000, SampleRate: rate},
	}
}

func audioArgs(t *testing.T, mode string, source []SourceStream) string {
	t.Helper()
	ep, err := BuildExecutionPlan(sizeV5TestPlan(mode), source, 1800)
	if err != nil {
		t.Fatal(err)
	}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	return " " + strings.Join(args, " ") + " "
}

// AudioToolbox's AAC encoder cannot open on 88.2 kHz and higher input (a
// 96 kHz 5.1 FLAC failed as "Could not open encoder before EOF", -22). Such a
// track is resampled to 48 kHz for aac_at; nothing else is touched.
func TestAudioToolboxTracksAbove48kHzAreResampled(t *testing.T) {
	const resample = " -ar:a:0 48000 "
	for _, tc := range []struct {
		name string
		mode string
		rate int
		want bool
	}{
		{"96 kHz through aac_at", transcode.AudioModeSizeAAC, 96000, true},
		{"192 kHz through aac_at", transcode.AudioModeSizeAAC, 192000, true},
		{"96 kHz HE-AAC through aac_at", transcode.AudioModeSizeHEAAC, 96000, true},
		{"88.2 kHz through aac_at", transcode.AudioModeSizeAAC, 88200, true},
		{"48 kHz is left alone", transcode.AudioModeSizeAAC, 48000, false},
		{"44.1 kHz is left alone", transcode.AudioModeSizeAAC, 44100, false},
		{"rate not stated is left alone", transcode.AudioModeSizeAAC, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := audioArgs(t, tc.mode, lossless51Source(tc.rate))
			if !strings.Contains(joined, " -c:a:0 aac_at ") {
				t.Fatalf("not an aac_at track: %s", joined)
			}
			if got := strings.Contains(joined, resample); got != tc.want {
				t.Fatalf("resample = %v, want %v: %s", got, tc.want, joined)
			}
		})
	}
	// The native encoder opens at any rate and keeps it.
	native := lossless51Source(96000)
	native[1].Channels, native[1].Layout = 8, "7.1"
	joined := audioArgs(t, transcode.AudioModeSizeAAC, native)
	if !strings.Contains(joined, " -c:a:0 aac ") || strings.Contains(joined, " -ar:a:0 ") {
		t.Fatalf("native encoder track was resampled: %s", joined)
	}
}

// The failing file end to end with the real encoder: a 96 kHz 5.1 lossless
// track converted at the size goal's level opens, keeps six channels and the
// 5.1 layout, and comes out as 48 kHz AAC-LC.
func TestHighSampleRateLosslessConvertsWithAudioToolbox(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] || !available["libx264"] || !available["aac_at"] || !available["flac"] {
		t.Skip("libx265/libx264/AudioToolbox AAC/FLAC unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	gen := []string{"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=330:sample_rate=96000:duration=4,aformat=channel_layouts=5.1",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "flac", "-metadata:s:a:0", "language=jpn", source}
	if out, err := exec.CommandContext(ctx, ffmpeg, gen...).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	audio := streamsOfKind(srcProbe.Streams, "audio")
	if len(audio) != 1 || audio[0].SampleRate != 96000 {
		t.Fatalf("the probe did not read the sample rate: %+v", audio)
	}
	plan := sizeV5TestPlan(transcode.AudioModeSizeAAC)
	plan.Preset = "ultrafast"
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.mkv")
	if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.log"))
		t.Fatalf("encode: %v\n%s", err, log)
	}
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,profile,channels,channel_layout,sample_rate", "-of", "compact=p=0", candidate).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(out))
	for _, want := range []string{"codec_name=aac", "profile=LC", "channels=6", "channel_layout=5.1", "sample_rate=48000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("candidate audio %q lacks %s", got, want)
		}
	}
}
