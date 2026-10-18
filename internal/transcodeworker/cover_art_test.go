package transcodeworker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func coverArtPlan() *transcode.Plan {
	p := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 28, Preset: "ultrafast", AudioMode: "copy", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

func coverArtStreams() []SourceStream {
	return []SourceStream{
		{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8},
		{Index: 1, TypeIndex: 0, Kind: "audio", Codec: "aac", Channels: 2},
		{Index: 2, TypeIndex: 0, Kind: "attachment", Codec: "ttf", Filename: "font.ttf", MimeType: "application/x-truetype-font"},
		{Index: 3, TypeIndex: 1, Kind: "video", Codec: "mjpeg", Width: 600, Height: 900, Disposition: map[string]int{"attached_pic": 1}, Filename: "cover.jpg", MimeType: "image/jpeg"},
	}
}

func TestBuildExecutionPlanKeepsCoverArtOutOfVideoEncode(t *testing.T) {
	ep, err := BuildExecutionPlan(coverArtPlan(), coverArtStreams(), 60)
	if err != nil {
		t.Fatal(err)
	}
	covers := ep.CoverArt()
	if len(covers) != 1 || !reflect.DeepEqual(covers[0], StreamAction{Kind: "cover", SourceIndex: 3, TypeIndex: 1, Codec: "mjpeg", TargetCodec: "copy", Filename: "cover.jpg", MimeType: "image/jpeg"}) {
		t.Fatalf("cover action: %+v", covers)
	}
	if ep.SourceAttachments != 1 {
		t.Fatalf("source attachments = %d", ep.SourceAttachments)
	}
	for _, s := range ep.Streams {
		if s.Kind == "video" && s.SourceIndex != 0 {
			t.Fatalf("cover planned as encoded video: %+v", s)
		}
	}

	// Two-pass x265 counts programme video only.
	twoPass := coverArtPlan()
	twoPass.Quality = 0
	twoPass.X265 = &transcode.X265Settings{BitrateKbps: 2000, TwoPass: true, Lookahead: 20, BFrames: 4, RefFrames: 3, AQMode: 2, AQStrength: 1, PsyRD: 1, PsyRDOQ: 1}
	twoPass.PlanDigest, _ = transcode.DigestPlan(twoPass)
	if _, err := BuildExecutionPlan(twoPass, coverArtStreams(), 60); err != nil {
		t.Fatalf("two-pass rejected a source with cover art: %v", err)
	}
	second := append(coverArtStreams(), SourceStream{Index: 4, TypeIndex: 2, Kind: "video", Codec: "h264"})
	if _, err := BuildExecutionPlan(twoPass, second, 60); err == nil {
		t.Fatal("two-pass accepted two programme video streams")
	}
}

func TestCoverArtActionTypeAndName(t *testing.T) {
	for _, tc := range []struct {
		stream       SourceStream
		wantName     string
		wantMimeType string
	}{
		{SourceStream{Index: 3, TypeIndex: 1, Kind: "video", Codec: "png", Disposition: map[string]int{"attached_pic": 1}}, "cover_1.png", "image/png"},
		{SourceStream{Index: 3, TypeIndex: 1, Kind: "video", Codec: "mjpeg", Disposition: map[string]int{"attached_pic": 1}, Filename: "../cover.jpg"}, "cover_1.jpg", "image/jpeg"},
		{SourceStream{Index: 3, TypeIndex: 1, Kind: "video", Codec: "mjpeg", Disposition: map[string]int{"attached_pic": 1}, Filename: "folder.jpg", MimeType: "image/jpeg"}, "folder.jpg", "image/jpeg"},
	} {
		a, err := coverArtAction(tc.stream)
		if err != nil || a.Filename != tc.wantName || a.MimeType != tc.wantMimeType {
			t.Fatalf("%+v: got %+v, %v", tc.stream, a, err)
		}
	}
	for _, bad := range []SourceStream{
		{Index: 3, Kind: "video", Codec: "h264", Disposition: map[string]int{"attached_pic": 1}},
		{Index: 3, Kind: "video", Codec: "png", Disposition: map[string]int{"attached_pic": 1}, MimeType: "text/html"},
	} {
		if _, err := BuildExecutionPlan(coverArtPlan(), []SourceStream{{Index: 0, Kind: "video", Codec: "h264"}, bad}, 60); err == nil {
			t.Fatalf("accepted unsupported cover %+v", bad)
		}
	}
}

func TestBuildFFmpegArgsReattachesCoverArt(t *testing.T) {
	ep, err := BuildExecutionPlan(coverArtPlan(), coverArtStreams(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildFFmpegArgs(ep, "/in.mkv", "/out.mkv", "/progress"); err == nil {
		t.Fatal("built args without the extracted cover")
	}
	if _, err := BuildFFmpegArgsWithCoverArt(ep, "/in.mkv", "/out.mkv", "/progress", map[int]string{3: "cover.jpg"}); err == nil {
		t.Fatal("accepted a relative cover path")
	}
	args, err := BuildFFmpegArgsWithCoverArt(ep, "/in.mkv", "/out.mkv", "/progress", map[int]string{3: "/scratch/cover-3.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-map 0:V? -map 0:a? -map 0:s? -map 0:t?",
		"-c:v libx265",
		"-c:t copy -attach /scratch/cover-3.jpg -metadata:s:t:1 mimetype=image/jpeg -metadata:s:t:1 filename=cover.jpg /out.mkv",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "0:v?") {
		t.Fatalf("cover art still mapped as encoded video:\n%s", joined)
	}

	plain, err := BuildExecutionPlan(coverArtPlan(), coverArtStreams()[:3], 60)
	if err != nil {
		t.Fatal(err)
	}
	args, err = BuildFFmpegArgs(plain, "/in.mkv", "/out.mkv", "/progress")
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-map 0:v? ") || strings.Contains(joined, "-attach") {
		t.Fatalf("plain source args changed:\n%s", joined)
	}
}

func TestCandidateVideoCountsSeparateCoverArt(t *testing.T) {
	streams := coverArtStreams()
	if countMainVideo(streams) != 1 || countCoverArt(streams) != 1 {
		t.Fatalf("main=%d covers=%d", countMainVideo(streams), countCoverArt(streams))
	}
	coverFirst := []SourceStream{streams[3], streams[0]}
	if v := firstMainVideo(coverFirst); v == nil || v.Index != 0 {
		t.Fatalf("first main video = %+v", v)
	}
}

// TestCoverArtRealEncode runs the production encode path on a synthetic MKV
// whose cover is an image attachment (ffprobe: png video, attached_pic=1),
// then the pre-publish structural and sampled quality validation.
func TestCoverArtRealEncode(t *testing.T) {
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
	cover := filepath.Join(dir, "cover.png")
	font := filepath.Join(dir, "font.ttf")
	source := filepath.Join(dir, "source.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x240:rate=1", "-frames:v", "1", cover).CombinedOutput(); err != nil {
		t.Fatalf("cover fixture: %v %s", err, out)
	}
	if err := os.WriteFile(font, []byte("synthetic font payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=4", "-map", "0:v", "-map", "1:a", "-c:v", "ffv1", "-pix_fmt", "yuv420p", "-c:a", "flac",
		"-attach", font, "-metadata:s:t:0", "mimetype=application/x-truetype-font",
		"-attach", cover, "-metadata:s:t:1", "mimetype=image/png", "-metadata:s:t:1", "filename=cover.png", source).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	coverBytes, err := os.ReadFile(cover)
	if err != nil {
		t.Fatal(err)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	if countMainVideo(srcProbe.Streams) != 1 || countCoverArt(srcProbe.Streams) != 1 || countStreams(srcProbe.Streams, "attachment") != 1 {
		t.Fatalf("fixture does not have the production cover-art shape: %+v", srcProbe.Streams)
	}
	twoPass := coverArtPlan()
	twoPass.Quality = 0
	twoPass.X265 = &transcode.X265Settings{BitrateKbps: 300, TwoPass: true, Lookahead: 10, BFrames: 2, RefFrames: 2, AQMode: 2, AQStrength: 1, PsyRD: 1, PsyRDOQ: 1}
	for name, plan := range map[string]*transcode.Plan{"crf": coverArtPlan(), "two_pass": twoPass} {
		t.Run(name, func(t *testing.T) {
			minimum, _ := optimization.ModeMinimumSavings("size", "")
			plan.Mode, plan.PolicyDigest, plan.SizePolicy = "size", "sha256:"+strings.Repeat("0", 64), &transcode.SizePolicy{MinSavingsPercent: float64(minimum)}
			plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}, {Index: 1, StartSeconds: 2.5, DurationSeconds: 1}}, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
			if err != nil {
				t.Fatal(err)
			}
			jobDir := filepath.Join(dir, name)
			if err := os.MkdirAll(jobDir, 0o700); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(jobDir, "candidate.mkv")
			if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
				log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
				t.Fatalf("encode: %v\n%s", err, log)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(jobDir, ".cover-art-*")); len(leftovers) != 0 {
				t.Fatalf("cover scratch left behind: %v", leftovers)
			}
			candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
			if err != nil {
				t.Fatal(err)
			}
			main := firstMainVideo(candProbe.Streams)
			if main == nil || main.Codec != "hevc" || main.Width != 320 || main.Height != 180 {
				t.Fatalf("main video not HEVC: %+v", candProbe.Streams)
			}
			var covers []SourceStream
			for _, s := range candProbe.Streams {
				if s.IsAttachedPicture() {
					covers = append(covers, s)
				}
			}
			if len(covers) != 1 || covers[0].Codec != "png" || covers[0].Filename != "cover.png" || covers[0].MimeType != "image/png" || countStreams(candProbe.Streams, "attachment") != 1 {
				t.Fatalf("cover not preserved as an attachment: %+v", candProbe.Streams)
			}
			extracted, err := ExtractCoverArt(ctx, ffmpeg, candidate, t.TempDir(), []StreamAction{{Kind: "cover", SourceIndex: covers[0].Index, MimeType: "image/png"}})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(extracted[covers[0].Index]); err != nil || !bytes.Equal(got, coverBytes) {
				t.Fatalf("cover bytes changed (%d vs %d bytes): %v", len(got), len(coverBytes), err)
			}

			w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
			// The mode preflight gets past the source-class gate; it then stops
			// only because this SSIM plan has no frozen VMAF policy.
			if err := w.validateModeSourceQuality(ctx, source, plan); err == nil || !strings.Contains(err.Error(), "quality_model_unavailable") {
				t.Fatalf("mode preflight: %v", err)
			}
			if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, srcProbe, 0); err != nil {
				t.Fatalf("pre-publish validation: %v", err)
			}
			info, err := os.Stat(candidate)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := w.validateFinalPerceptual(ctx, jobDir, source, candidate, plan, "sha256:candidate", info.Size())
			if err != nil || evidence == nil || evidence.Verdict != "pass" || evidence.SSIMMean == nil {
				t.Fatalf("sampled quality validation: %+v %v", evidence, err)
			}

			// A candidate that dropped the cover must not pass.
			lost := filepath.Join(jobDir, "lost-cover.mkv")
			if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", candidate, "-map", "0:V", "-map", "0:a", "-map", "0:t", "-c", "copy", lost).CombinedOutput(); err != nil {
				t.Fatalf("lost-cover fixture: %v %s", err, out)
			}
			if _, err := w.validateEncodedCandidateFull(ctx, lost, ep, srcProbe, 0); err == nil || !strings.Contains(err.Error(), "cover art count") {
				t.Fatalf("accepted a candidate without its cover: %v", err)
			}
		})
	}
}

// Cover first, as an MP4 covr or a remuxed file may order it, so the gate
// cannot pass by taking the first video stream.
const coverArtProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "mjpeg", "pix_fmt": "yuvj420p", "width": 600, "height": 900, "r_frame_rate": "90000/1", "avg_frame_rate": "0/0", "disposition": {"default": 0, "attached_pic": 1}, "tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}},
    {"index": 1, "codec_type": "video", "codec_name": "h264", "profile": "High", "pix_fmt": "yuv420p", "r_frame_rate": "24/1", "avg_frame_rate": "24/1", "width": 1920, "height": 1080, "bits_per_raw_sample": "8", "color_range": "tv", "color_space": "bt709", "color_primaries": "bt709", "color_transfer": "bt709", "disposition": {"default": 1, "attached_pic": 0}}
  ],
  "format": {"format_name": "matroska,webm", "duration": "120.000000", "size": "50000000"},
  "chapters": []
}`

func TestProductionBenchmarkRunnerMeasuresMainVideoBesideCoverArt(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	_ = os.WriteFile(sourceFile, []byte("fake video content"), 0644)
	mockFFmpeg, mockProbe, logFile := setupMockTools(t, dir, coverArtProbeJSON, "")
	worker := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: mockFFmpeg, FFprobe: mockProbe})
	record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-cover", Status: "running", Source: sourceFile, Metric: "ssim", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 20, DurationSeconds: 10}}, Candidates: []transcode.BenchmarkCandidate{{ID: "cand", Quality: 75}}, Attempt: 1}
	if err := (&ProductionBenchmarkRunner{}).RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("benchmark rejected a source with cover art: %v", err)
	}
	if record.Evidence == nil || record.Evidence.SourceBitDepth != 8 {
		t.Fatalf("evidence: %+v", record.Evidence)
	}
	log, _ := os.ReadFile(logFile)
	if !strings.Contains(string(log), "-map 0:1 -c:v ffv1") || strings.Contains(string(log), "-map 0:0 -c:v ffv1") {
		t.Fatalf("reference sample not taken from the main video:\n%s", log)
	}
}
