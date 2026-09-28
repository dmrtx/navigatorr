package transcodeworker

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestCompactAudioArgumentsAndPreservation(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "libx265", Quality: 23, VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact"}
	ep := &ExecutionPlan{Plan: plan, Streams: []StreamAction{
		{Kind: "audio", TypeIndex: 0, TargetCodec: "aac", BitrateKbps: 192},
		{Kind: "audio", TypeIndex: 1, TargetCodec: "copy"},
	}}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-pix_fmt yuv420p10le", "-c:a copy", "-c:a:0 aac", "-b:a:0 192k", "-metadata:s:a:0 BPS="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s: %s", want, joined)
		}
	}
	if strings.Contains(joined, "-ac ") || strings.Contains(joined, "-c:a:1 aac") {
		t.Fatal("downmix or lossy re-encode requested")
	}
	src := []SourceStream{{Kind: "audio", Codec: "dts", Channels: 2, Language: "eng"}, {Kind: "audio", Codec: "aac", Channels: 2, Language: "jpn"}}
	cand := append([]SourceStream(nil), src...)
	cand[0].Codec = "aac"
	if err := validateAudioPreservation(plan, src, cand); err != nil {
		t.Fatal(err)
	}
	cand[0].Language = ""
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("known language lost")
	}
	cand[0].Language = "eng"
	cand[0].Codec = "dts"
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("ignored compact mode accepted")
	}
	cand[0].Codec = "aac"
	cand[1].Codec = "mp3"
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("copied track changed codec")
	}
}

func TestCompactAudioMain10RealEncode(t *testing.T) {
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
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "nullsrc=size=320x180:rate=24:duration=1,format=yuv420p10le,geq=lum='64+mod(X+3*Y,877)':cb=512:cr=512", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000:duration=1", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "ffv1", "-c:a", "flac", "-ac", "2", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn", source}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	probe, err := ProbeSourceDetails(context.Background(), ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 23, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	args, err = BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
	if err != nil {
		t.Fatal(err)
	}
	args = append([]string{"-nostdin", "-v", "error"}, args...)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v %s", err, out)
	}
	result, err := ProbeSourceDetails(context.Background(), ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	video := firstStreamOfKind(result.Streams, "video")
	if video == nil || video.Codec != "hevc" || video.BitDepth != 10 {
		t.Fatalf("video not Main10: %+v", video)
	}
	if err := validateAudioPreservation(plan, probe.Streams, result.Streams); err != nil {
		t.Fatal(err)
	}
	audio := streamsOfKind(result.Streams, "audio")
	if len(audio) != 2 || audio[0].Codec != "aac" || audio[1].Codec != "aac" || audio[0].Language != "eng" || audio[1].Language != "jpn" {
		t.Fatalf("audio policy mismatch: %+v", audio)
	}
	if len(ep.Conversions) != 2 {
		t.Fatalf("missing conversion evidence: %+v", ep.Conversions)
	}
}

func TestNative10BitRealQualityProbe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	filters, err := exec.Command(ffmpeg, "-hide_banner", "-filters").CombinedOutput()
	if err != nil || !strings.Contains(string(filters), "libvmaf") {
		t.Skip("libvmaf unavailable")
	}
	caps := probeQualityCapabilities(context.Background(), ffmpeg, t.TempDir())
	if !caps.Models["v1_1080p_3h"].Available && caps.ProbeError == "" {
		t.Skip("installed FFmpeg does not support VMAF v1")
	}
	if !caps.Native10Bit || !caps.CAMBIFullRef || caps.ProbeError != "" {
		t.Fatalf("native 10-bit executable probe failed: %+v", caps)
	}
}
