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
}

// Two-pass x265 analyses the first video stream that is not cover art, so a
// preview track after the programme video is left out like in any other plan,
// and one before it is refused: it would be analysed in the programme's place.
func TestTwoPassPlanWithPreviewTrack(t *testing.T) {
	twoPass := coverArtPlan()
	twoPass.Quality = 0
	twoPass.X265 = &transcode.X265Settings{BitrateKbps: 2000, TwoPass: true, Lookahead: 20, BFrames: 4, RefFrames: 3, AQMode: 2, AQStrength: 1, PsyRD: 1, PsyRDOQ: 1}
	twoPass.PlanDigest, _ = transcode.DigestPlan(twoPass)

	ep, err := BuildExecutionPlan(twoPass, previewTrackStreams(), 60)
	if err != nil {
		t.Fatalf("two-pass refused a preview track after the programme video: %v", err)
	}
	if !reflect.DeepEqual(ep.Thumbnails, []int{2}) {
		t.Fatalf("thumbnails = %v", ep.Thumbnails)
	}
	if _, err := BuildExecutionPlan(twoPass, previewTrackStreams()[:2], 60); err != nil {
		t.Fatalf("two-pass rejected a source without a preview track: %v", err)
	}

	streams := previewTrackStreams()
	previewFirst := []SourceStream{streams[2], streams[0], streams[1]}
	previewFirst[0].Index, previewFirst[1].Index, previewFirst[2].Index = 0, 1, 2
	if _, err := BuildExecutionPlan(twoPass, previewFirst, 60); err == nil || !strings.Contains(err.Error(), "preview image track") {
		t.Fatalf("two-pass accepted a preview track before the programme video: %v", err)
	}

	// Cover art before the programme is not analysed: 0:V skips it.
	coverFirst := []SourceStream{coverArtStreams()[3], streams[0], streams[1], streams[2]}
	for i := range coverFirst {
		coverFirst[i].Index = i
	}
	if _, err := BuildExecutionPlan(twoPass, coverFirst, 60); err != nil {
		t.Fatalf("two-pass refused cover art before the programme video: %v", err)
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

// Tracks picked by hand map audio and subtitles one by one; the preview track
// is still unmapped, and only the picked audio stays.
func TestBuildFFmpegArgsUnmapsPreviewTrackWithPickedTracks(t *testing.T) {
	plan := sizeV5TestPlan(transcode.AudioModeSizeAACBed)
	plan.ModePolicyVersion = transcode.ModePolicyV9
	plan.SizePolicy.MinSavingsPercent = transcode.SizeSmallerSavingPercent
	plan.Tracks = &transcode.TrackSelection{KeepAudio: []int{1}}
	ep, err := BuildExecutionPlan(resign(plan), previewTrackStreams(), 60)
	if err != nil {
		t.Fatal(err)
	}
	args, err := BuildFFmpegArgs(ep, "/in.mp4", "/out.mkv", "/progress")
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-map 0:v? -map -0:2 -map 0:1 -map 0:t?") {
		t.Fatalf("picked tracks lost the preview unmap:\n%s", joined)
	}
}

// The audio-only goal copies the video and proves the copy against the source.
// With the preview track listed first, the source's first video stream is not
// the programme, and the proof must follow the programme's own index in each
// file instead of "the first video".
func TestVideoCopyProofSkipsPreviewTrackListedFirst(t *testing.T) {
	ffmpeg, ffprobe, ctx := videoCopyTools(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=0.25:duration=12",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=12",
		"-f", "lavfi", "-i", "aevalsrc=sin(440*2*PI*t)|sin(550*2*PI*t):s=48000:c=stereo:d=12",
		"-map", "0:v", "-map", "1:v", "-map", "2:a",
		"-c:v:0", "mjpeg", "-pix_fmt:v:0", "yuvj420p",
		"-c:v:1", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "-pix_fmt:v:1", "yuv420p",
		"-c:a", "pcm_s24le", source).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	programme, previews := splitProgrammeVideo(srcProbe.Streams)
	if len(programme) != 1 || len(previews) != 1 || previews[0].Index >= programme[0].Index {
		t.Fatalf("fixture does not list the preview track before the programme: %+v", srcProbe.Streams)
	}
	plan := sizeAudioOnlyTestPlan(t, 8)
	ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.mkv")
	if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.log"))
		t.Fatalf("remux: %v\n%s", err, log)
	}
	candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if countStreams(candProbe.Streams, "video") != 1 {
		t.Fatalf("candidate kept the preview track: %+v", candProbe.Streams)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	evidence, err := w.validateVideoCopy(ctx, source, candidate, plan, "sha256:c", mustSize(t, candidate))
	if err != nil || evidence.Verdict != "pass" {
		t.Fatalf("a correct copy was refused: %+v %v", evidence, err)
	}

	// A candidate left with only the preview track has no programme video to
	// copy: its lone track counts as the programme, and the proof refuses it.
	onlyPreview := filepath.Join(dir, "only-preview.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", source,
		"-map", "0:v:0", "-map", "0:a", "-c", "copy", onlyPreview).CombinedOutput(); err != nil {
		t.Fatalf("only-preview fixture: %v %s", err, out)
	}
	if evidence, err := w.validateVideoCopy(ctx, source, onlyPreview, plan, "sha256:p", mustSize(t, onlyPreview)); err == nil || evidence.Verdict == "pass" {
		t.Fatalf("accepted a candidate that kept only the preview track: %+v %v", evidence, err)
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

// previewTrackFixture builds a synthetic MP4 whose second video track is a few
// mjpeg pictures at 0.25 fps, the shape chapter thumbnails have in a real file.
// It skips the test when the tools are missing.
func previewTrackFixture(t *testing.T) (ctx context.Context, ffmpeg, dir, source string, probe SourceProbe) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] {
		t.Skip("libx265 unavailable")
	}
	dir = t.TempDir()
	source = filepath.Join(dir, "source.mp4")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=12",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=0.25:duration=12",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=12",
		"-map", "0:v", "-map", "1:v", "-map", "2:a",
		"-c:v:0", "mpeg4", "-pix_fmt:v:0", "yuv420p", "-c:v:1", "mjpeg", "-pix_fmt:v:1", "yuvj420p", "-c:a", "aac", "-t", "12", source).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	probe, err = ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, previews := splitProgrammeVideo(probe.Streams); countStreams(probe.Streams, "video") != 2 || len(previews) != 1 {
		t.Fatalf("fixture does not have the production preview-track shape: %+v", probe.Streams)
	}
	return ctx, ffmpeg, dir, source, probe
}

// previewTrackEncode is the outcome of runPreviewTrackEncode.
type previewTrackEncode struct {
	ctx                    context.Context
	ffmpeg, ffprobe        string
	dir, source, candidate string
	srcProbe               SourceProbe
	execPlan               *ExecutionPlan
}

// runPreviewTrackEncode runs the production encode path for plan on the fixture
// and checks the candidate carries one video stream, not two.
func runPreviewTrackEncode(t *testing.T, plan *transcode.Plan) previewTrackEncode {
	t.Helper()
	ctx, ffmpeg, dir, source, srcProbe := previewTrackFixture(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
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
	return previewTrackEncode{ctx: ctx, ffmpeg: ffmpeg, ffprobe: ffprobe, dir: dir, source: source, candidate: candidate, srcProbe: srcProbe, execPlan: ep}
}

// TestPreviewTrackRealEncode runs the production encode path on a synthetic MP4
// whose second video track is a few mjpeg pictures at 0.25 fps and checks the
// output carries one video stream, not two.
func TestPreviewTrackRealEncode(t *testing.T) {
	runPreviewTrackEncode(t, coverArtPlan())
}

// The plan unmaps the preview track, so a candidate that still carries one
// means the map did not take; the programme count does not see it, and the
// candidate must not be published.
func TestCandidateKeepingPreviewTrackFailsValidation(t *testing.T) {
	enc := runPreviewTrackEncode(t, coverArtPlan())
	w := NewWorker(&WorkerConfig{StateDir: enc.dir, FFmpeg: enc.ffmpeg, FFprobe: enc.ffprobe})
	if _, err := w.validateEncodedCandidateFull(enc.ctx, enc.candidate, enc.execPlan, prescanSource(t, w, enc.execPlan.Plan, enc.srcProbe, enc.source), 0); err != nil {
		t.Fatalf("clean candidate rejected: %v", err)
	}
	kept := filepath.Join(enc.dir, "kept-preview.mkv")
	if out, err := exec.CommandContext(enc.ctx, enc.ffmpeg, "-nostdin", "-v", "error", "-i", enc.candidate, "-i", enc.source,
		"-map", "0", "-map", "1:v:1", "-c", "copy", kept).CombinedOutput(); err != nil {
		t.Fatalf("kept-preview fixture: %v %s", err, out)
	}
	keptProbe, err := ProbeSourceDetails(enc.ctx, enc.ffprobe, kept)
	if err != nil {
		t.Fatal(err)
	}
	if _, previews := splitProgrammeVideo(keptProbe.Streams); len(previews) != 1 || countMainVideo(keptProbe.Streams) != 1 {
		t.Fatalf("fixture does not carry the preview track beside one programme video: %+v", keptProbe.Streams)
	}
	if _, err := w.validateEncodedCandidateFull(enc.ctx, kept, enc.execPlan, prescanSource(t, w, enc.execPlan.Plan, enc.srcProbe, enc.source), 0); err == nil || !strings.Contains(err.Error(), "preview image track") {
		t.Fatalf("accepted a candidate that kept the preview track: %v", err)
	}
}

// The two-pass path analyses 0:V:0 and encodes with the same unmap, so a
// preview track after the programme video ends up out of the output too.
func TestPreviewTrackRealTwoPassEncode(t *testing.T) {
	plan := coverArtPlan()
	plan.Quality = 0
	plan.X265 = &transcode.X265Settings{BitrateKbps: 400, TwoPass: true, Lookahead: 20, BFrames: 4, RefFrames: 3, AQMode: 2, AQStrength: 1, PsyRD: 1, PsyRDOQ: 1}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	runPreviewTrackEncode(t, plan)
}
