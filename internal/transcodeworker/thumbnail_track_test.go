package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// previewTrackStreams is a source whose chapter thumbnails are a second mjpeg
// video track at well under one frame per second.
func previewTrackStreams() []SourceStream {
	return []SourceStream{
		{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 804, BitDepth: 8, FPS: 23.976},
		{Index: 1, TypeIndex: 0, Kind: "audio", Codec: "aac", Channels: 2},
		{Index: 2, TypeIndex: 1, Kind: "video", Codec: "mjpeg", Width: 640, Height: 268, BitDepth: 8, FPS: 0.0056},
	}
}

func TestBuildExecutionPlanLeavesPreviewTrackOut(t *testing.T) {
	ep, err := BuildExecutionPlan(coverArtPlan(), previewTrackStreams(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ep.Thumbnails, []int{2}) {
		t.Fatalf("thumbnails = %v", ep.Thumbnails)
	}
	var video []int
	for _, s := range ep.Streams {
		if s.SourceIndex == 2 {
			t.Fatalf("preview track planned: %+v", s)
		}
		if s.Kind == "video" {
			video = append(video, s.SourceIndex)
		}
	}
	if !reflect.DeepEqual(video, []int{0}) {
		t.Fatalf("video actions = %v", video)
	}

	// Two-pass x265 maps the first video stream, so it still refuses a preview
	// track beside the programme video.
	twoPass := coverArtPlan()
	twoPass.Quality = 0
	twoPass.X265 = &transcode.X265Settings{BitrateKbps: 2000, TwoPass: true, Lookahead: 20, BFrames: 4, RefFrames: 3, AQMode: 2, AQStrength: 1, PsyRD: 1, PsyRDOQ: 1}
	twoPass.PlanDigest, _ = transcode.DigestPlan(twoPass)
	if _, err := BuildExecutionPlan(twoPass, previewTrackStreams(), 60); err == nil {
		t.Fatal("two-pass accepted a preview track beside the programme video")
	}
	if _, err := BuildExecutionPlan(twoPass, previewTrackStreams()[:2], 60); err != nil {
		t.Fatalf("two-pass rejected a source without a preview track: %v", err)
	}
}

func TestBuildFFmpegArgsUnmapsPreviewTrack(t *testing.T) {
	ep, err := BuildExecutionPlan(coverArtPlan(), previewTrackStreams(), 60)
	if err != nil {
		t.Fatal(err)
	}
	args, err := BuildFFmpegArgs(ep, "/in.mp4", "/out.mkv", "/progress")
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-map 0:v? -map -0:2 -map 0:a? -map 0:s? -map 0:t?") {
		t.Fatalf("preview track still mapped:\n%s", joined)
	}

	// Beside cover art the programme map is 0:V?; the preview track matches it.
	withCover := append(previewTrackStreams(), coverArtStreams()[3])
	ep, err = BuildExecutionPlan(coverArtPlan(), withCover, 60)
	if err != nil {
		t.Fatal(err)
	}
	args, err = BuildFFmpegArgsWithCoverArt(ep, "/in.mp4", "/out.mkv", "/progress", map[int]string{3: "/scratch/cover-3.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-map 0:V? -map -0:2 -map 0:a?") {
		t.Fatalf("preview track still mapped beside cover art:\n%s", joined)
	}

	plain, err := BuildExecutionPlan(coverArtPlan(), previewTrackStreams()[:2], 60)
	if err != nil {
		t.Fatal(err)
	}
	args, err = BuildFFmpegArgs(plain, "/in.mp4", "/out.mkv", "/progress")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "-map -0:") {
		t.Fatalf("plain source args changed:\n%s", strings.Join(args, " "))
	}
}

func TestProgrammeVideoSkipsPreviewTrack(t *testing.T) {
	streams := previewTrackStreams()
	if countMainVideo(streams) != 1 {
		t.Fatalf("programme video count = %d", countMainVideo(streams))
	}
	// A preview track listed first is never taken as the programme.
	previewFirst := []SourceStream{streams[2], streams[0]}
	previewFirst[0].Index, previewFirst[1].Index = 0, 1
	if v := firstMainVideo(previewFirst); v == nil || v.Index != 1 || v.Codec != "h264" {
		t.Fatalf("first main video = %+v", v)
	}
	if i := programmeVideoIndex(previewFirst); i != 1 {
		t.Fatalf("programme video index = %d", i)
	}
	// With nothing else, the track is the programme.
	if alone := streams[2:]; countMainVideo(alone) != 1 || programmeVideoIndex(alone) != 2 {
		t.Fatalf("a lone preview-like track was dropped: %d", countMainVideo(alone))
	}
}

// TestPreviewTrackRealEncode runs the production encode path on a synthetic MP4
// whose second video track is a few mjpeg pictures at 0.25 fps and checks the
// output carries one video stream, not two.
func TestPreviewTrackRealEncode(t *testing.T) {
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
	if err != nil || !available["libx265"] {
		t.Skip("libx265 unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=12",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=0.25:duration=12",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=12",
		"-map", "0:v", "-map", "1:v", "-map", "2:a",
		"-c:v:0", "mpeg4", "-pix_fmt:v:0", "yuv420p", "-c:v:1", "mjpeg", "-pix_fmt:v:1", "yuvj420p", "-c:a", "aac", "-t", "12", source).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, previews := splitProgrammeVideo(srcProbe.Streams); countStreams(srcProbe.Streams, "video") != 2 || len(previews) != 1 {
		t.Fatalf("fixture does not have the production preview-track shape: %+v", srcProbe.Streams)
	}
	ep, err := BuildExecutionPlan(coverArtPlan(), srcProbe.Streams, srcProbe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.mkv")
	if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.log"))
		t.Fatalf("encode: %v\n%s", err, log)
	}
	candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := countStreams(candProbe.Streams, "video"); got != 1 {
		t.Fatalf("candidate has %d video streams: %+v", got, candProbe.Streams)
	}
	if main := firstMainVideo(candProbe.Streams); main == nil || main.Codec != "hevc" || main.Width != 320 || main.Height != 180 {
		t.Fatalf("main video not HEVC: %+v", candProbe.Streams)
	}
	if countStreams(candProbe.Streams, "audio") != 1 {
		t.Fatalf("audio lost: %+v", candProbe.Streams)
	}
	if countMainVideo(candProbe.Streams) != countMainVideo(srcProbe.Streams) {
		t.Fatalf("programme video count changed: source %d, candidate %d", countMainVideo(srcProbe.Streams), countMainVideo(candProbe.Streams))
	}
}
