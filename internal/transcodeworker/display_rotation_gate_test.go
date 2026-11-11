package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// displayMatrix is the video stream's raw display matrix (empty when none).
func displayMatrix(t *testing.T, ffprobe, path string) string {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream_side_data=displaymatrix", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// displayPSNR is the mean PSNR of two files' pictures as a player shows them:
// ffmpeg's default autorotate on both inputs, which is how the quality
// extracts read them. A mirrored or turned picture scores near 10 dB.
func displayPSNR(t *testing.T, ffmpeg, a, b string) float64 {
	t.Helper()
	v, ok := tryDisplayPSNR(ffmpeg, a, b)
	if !ok {
		t.Fatalf("psnr %s vs %s failed", a, b)
	}
	return v
}

// tryDisplayPSNR is displayPSNR, not ok when the displayed sizes differ.
func tryDisplayPSNR(ffmpeg, a, b string) (float64, bool) {
	out, err := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-i", a, "-i", b, "-filter_complex", "[0:v:0][1:v:0]psnr", "-f", "null", "-").CombinedOutput()
	m := regexp.MustCompile(`average:(\S+)`).FindStringSubmatch(string(out))
	if err != nil || m == nil {
		return 0, false
	}
	if m[1] == "inf" {
		return 100, true
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v, true
}

func rotationPlan(t *testing.T, probe SourceProbe, quality int) (*transcode.Plan, *ExecutionPlan) {
	t.Helper()
	plan := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: quality, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	return plan, ep
}

// A mirrored source (a flip in its display matrix, with or without a turn)
// keeps the flip as the encode's display matrix, so a player shows the same
// picture: source and candidate decoded the default way match, and the
// matrix is the source's.
func TestReencodeKeepsDisplayFlipAsMetadata(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"hflip", []string{"-display_hflip"}},
		{"vflip", []string{"-display_vflip"}},
		{"rotate90_hflip", []string{"-display_rotation", "90", "-display_hflip"}},
		{"rotate90_vflip", []string{"-display_rotation", "90", "-display_vflip"}},
		{"rotate270_hflip", []string{"-display_rotation", "270", "-display_hflip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base.mp4")
			scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=2", "-c:v", "libx264", "-crf", "10", "-pix_fmt", "yuv420p", base)
			source := filepath.Join(dir, "source.mp4")
			scanTestMedia(t, ffmpeg, append(append([]string{}, tc.args...), "-i", base, "-c", "copy", source)...)
			srcMatrix := displayMatrix(t, ffprobe, source)
			if srcMatrix == "" {
				t.Fatal("fixture has no display matrix")
			}
			probe, err := ProbeSourceDetails(context.Background(), ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			_, ep := rotationPlan(t, probe, 14)
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := RunFFmpegPaths(context.Background(), ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
				t.Fatalf("encode: %v", err)
			}
			candW, candH, _ := rotationFacts(t, ffprobe, candidate)
			if candW != 640 || candH != 360 {
				t.Errorf("candidate stores %dx%d, want the source's 640x360", candW, candH)
			}
			if got := displayMatrix(t, ffprobe, candidate); got != srcMatrix {
				t.Errorf("candidate display matrix %q, want the source's %q", got, srcMatrix)
			}
			psnr := displayPSNR(t, ffmpeg, source, candidate)
			t.Logf("%s: displayed PSNR %.2f dB", tc.name, psnr)
			if psnr < 25 {
				t.Errorf("displayed picture differs from the source's: PSNR %.2f dB", psnr)
			}
			// Control: the same encode without the flip shows another picture.
			plain := filepath.Join(dir, "plain.mkv")
			scanTestMedia(t, ffmpeg, "-display_rotation", "0", "-i", candidate, "-map", "0:v:0", "-c", "copy", plain)
			if wrong, ok := tryDisplayPSNR(ffmpeg, source, plain); ok && wrong > psnr-8 {
				t.Errorf("a candidate without the flip scores %.2f dB against %.2f: the comparison cannot tell them apart", wrong, psnr)
			} else {
				t.Logf("%s: without the matrix %.2f dB (same size: %v)", tc.name, wrong, ok)
			}
		})
	}
}

func rotationGateEnv(t *testing.T) (ctx context.Context, ffmpeg, ffprobe, dir string, w func(string) *Worker) {
	t.Helper()
	ffmpeg = requireLibvmaf(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	dir = t.TempDir()
	// Where the production VMAF model is not installed, the shim runs the
	// built-in one under the production model's name.
	shim := qualityShim(t, ffmpeg, hfrTestModel(t, ctx, ffmpeg), false)
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, shim, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Encoders["libx265"] || caps.Quality == nil || !caps.Quality.Models["v1_1080p_3h"].Available {
		t.Skip("executable VMAF v1/libx265 path unavailable")
	}
	w = func(name string) *Worker {
		return NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: shim, FFprobe: ffprobe, MaxParallelJobs: 1})
	}
	return
}

func rotationQuality() *transcode.QualityValidationPlan {
	return &transcode.QualityValidationPlan{
		Metric:                 "vmaf",
		Samples:                []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}},
		BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64),
		Quality: transcode.BenchmarkQualityConfig{
			VMAF:            &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 80, Target: 90, WorstWindowSeconds: 0.25},
			FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"},
		},
	}
}

// The final quality gate on a source turned 90 degrees measures the encode and
// the source in the same orientation: a good encode scores high, and an encode
// that lost the rotation (the stored pictures without the display matrix) is
// not passed.
func TestRealFinalGateMeasuresRotatedSourceInOneOrientation(t *testing.T) {
	ctx, ffmpeg, ffprobe, dir, worker := rotationGateEnv(t)
	base := filepath.Join(dir, "base.mp4")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=3", "-c:v", "libx264", "-crf", "10", "-pix_fmt", "yuv420p", base)
	source := filepath.Join(dir, "source.mp4")
	scanTestMedia(t, ffmpeg, "-display_rotation", "90", "-i", base, "-c", "copy", source)
	probe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan, ep := rotationPlan(t, probe, 20)
	plan.QualityValidation = rotationQuality()
	good := filepath.Join(dir, "good.mkv")
	if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, good, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
		t.Fatal(err)
	}
	// The same pictures with no display matrix: shown sideways.
	lost := filepath.Join(dir, "lost.mkv")
	scanTestMedia(t, ffmpeg, "-display_rotation", "0", "-i", good, "-map", "0:v:0", "-c", "copy", lost)
	if m := displayMatrix(t, ffprobe, lost); m != "" {
		t.Fatalf("fixture kept a display matrix: %q", m)
	}

	jobDir := filepath.Join(dir, "gate")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := worker("good").validateFinalPerceptual(ctx, jobDir, source, good, plan, "sha256:candidate", 1)
	if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil {
		t.Fatalf("a good encode of a rotated source was refused: %+v %v", e, err)
	}
	t.Logf("good encode: VMAF mean %.2f min %.2f", e.VMAF.Mean, e.VMAF.Min)
	if e.VMAF.Mean < 90 {
		t.Errorf("good encode scored %.2f", e.VMAF.Mean)
	}

	e, err = worker("lost").validateFinalPerceptual(ctx, jobDir, source, lost, plan, "sha256:candidate", 1)
	if e != nil && e.VMAF != nil {
		t.Logf("rotation lost: verdict %s VMAF mean %.2f", e.Verdict, e.VMAF.Mean)
	}
	t.Logf("rotation lost: err %v", err)
	if err == nil && e != nil && e.Verdict == "pass" {
		t.Fatalf("an encode that lost its rotation passed the gate")
	}
}

// A compact plan on a source stored 1920x1080 and shown turned 90 degrees:
// the plan sizes the encode in stored pixels (the filter runs on them), and
// the output check, the final gate and the quality windows agree with it.
func TestRealCompactOnRotatedSource(t *testing.T) {
	ctx, ffmpeg, ffprobe, dir, worker := rotationGateEnv(t)
	useTempCompactLUTDir(t)
	if err := probeCompactChain(ctx, ffmpeg); err != nil {
		t.Skipf("compact chain not executable here: %v", err)
	}
	base := filepath.Join(dir, "base.mp4")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=2",
		"-vf", "format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv",
		"-c:v", "libx264", "-crf", "12", "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv", base)
	source := filepath.Join(dir, "source.mp4")
	scanTestMedia(t, ffmpeg, "-display_rotation", "90", "-i", base, "-c", "copy", source)

	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
	if err != nil {
		t.Fatal(err)
	}
	c, err := transcode.NewCompactPlan(d, rep.MainVideo()[0], 720)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("source stored %dx%d, compact target %dx%d (max height 720)", rep.MainVideo()[0].Width, rep.MainVideo()[0].Height, c.TargetWidth, c.TargetHeight)
	probe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := compactModePlanFor(t, c, 8)
	plan.Quality = 16
	plan.QualityValidation = rotationQuality()
	plan.PlanDigest = ""
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.mkv")
	args, err := BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("compact encode: %v %s", err, out)
	}
	w, h, rot := rotationFacts(t, ffprobe, candidate)
	t.Logf("candidate stored %dx%d rotation %d", w, h, rot)
	if w != c.TargetWidth || h != c.TargetHeight || rot != 90 {
		t.Errorf("candidate %dx%d rotation %d, want %dx%d rotation 90", w, h, rot, c.TargetWidth, c.TargetHeight)
	}
	jobDir := filepath.Join(dir, "gate")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := worker("compact").validateFinalPerceptual(ctx, jobDir, source, candidate, plan, "sha256:candidate", 1)
	if e != nil && e.VMAF != nil {
		t.Logf("compact rotated: verdict %s VMAF mean %.2f min %.2f", e.Verdict, e.VMAF.Mean, e.VMAF.Min)
	}
	if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil {
		t.Fatalf("compact final gate on a rotated source: %+v %v", e, err)
	}
	if e.VMAF.Mean < 85 {
		t.Errorf("compact VMAF %.2f", e.VMAF.Mean)
	}

	// The same encode without its rotation is refused, though the stored
	// pictures the windows read are identical.
	lost := filepath.Join(dir, "lost.mkv")
	scanTestMedia(t, ffmpeg, "-display_rotation", "0", "-i", candidate, "-map", "0:v:0", "-c", "copy", lost)
	e, err = worker("compact-lost").validateFinalPerceptual(ctx, jobDir, source, lost, plan, "sha256:candidate", 1)
	if err == nil || e == nil || e.Verdict == "pass" || !strings.Contains(err.Error(), "quality_final_orientation_changed") {
		t.Fatalf("a compact encode that lost its rotation was not refused for it: %+v %v", e, err)
	}
}
