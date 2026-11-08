package transcodeworker

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestParseChromaPSNR(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		u, v float64
	}{
		"v inf":             {"[Parsed_psnr_4 @ 0x1] PSNR y:47.36 u:49.41 v:inf average:48.01 min:46.9 max:inf\n", 49.41, 100},
		"both inf":          {"[Parsed_psnr_0 @ 0xab] PSNR y:inf u:inf v:inf average:inf min:inf max:inf\n", 100, 100},
		"integers":          {"[Parsed_psnr_12 @ 0x1] PSNR y:40 u:41 v:42 average:41 min:40 max:42\n", 41, 42},
		"above 100":         {"[Parsed_psnr_1 @ 0x1] PSNR y:101.5 u:104.25 v:120 average:110 min:100 max:120\n", 104.25, 120},
		"noise first":       {"Input #0, matroska ...\nframe=  48 fps=0.0 q=-0.0 size=N/A\n[Parsed_psnr_2 @ 0x1] PSNR y:47 u:46.5 v:45.5 average:46 min:45 max:48\n", 46.5, 45.5},
		"last wins":         {"[Parsed_psnr_2 @ 0x1] PSNR y:1 u:1 v:1 average:1 min:1 max:1\n[Parsed_psnr_2 @ 0x1] PSNR y:47 u:46.5 v:45.5 average:46 min:45 max:48\n", 46.5, 45.5},
		"lookalike ignored": {"title : PSNR y:1 u:1 v:1 \n[Parsed_psnr_2 @ 0x1] PSNR y:47 u:46.5 v:45.5 average:46 min:45 max:48\n", 46.5, 45.5},
	} {
		u, v, err := parseChromaPSNR(tc.out)
		if err != nil || u != tc.u || v != tc.v {
			t.Errorf("%s: u=%v v=%v err=%v", name, u, v, err)
		}
	}
	for _, out := range []string{"[Parsed_psnr_4 @ 0x1] PSNR average:48.01\n", "metadata: PSNR y:1 u:2 v:3 \n", ""} {
		if _, _, err := parseChromaPSNR(out); err == nil {
			t.Errorf("accepted %q", out)
		}
	}
}

// A 4:4:4 source has its colour planes checked: a sound 4:2:0 encode passes
// with its chroma PSNR reported, the same encode with U and V swapped fails
// on chroma although its luma (all VMAF reads) is identical, and a 4:2:0
// source is not checked at all.
func TestRealFinalValidationChecksChromaOfSourcesBeyond420(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, ffmpeg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Encoders["libx265"] {
		t.Skip("libx265 unavailable")
	}
	// Chroma is checked whatever the luma metric; VMAF is used where its
	// model is, SSIM elsewhere.
	metric := "ssim"
	quality := transcode.BenchmarkQualityConfig{SSIM: &transcode.BenchmarkQualityThresholds{Minimum: 0.9}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
	if caps.Quality != nil && caps.Quality.Models["v1_1080p_3h"].Available {
		metric = "vmaf"
		quality = transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", WorstWindowSeconds: 0.25}, FinalValidation: quality.FinalValidation}
	}
	path := func(name string) string { return filepath.Join(dir, name) }
	// Both colour planes moved 4 pixels left, luma untouched.
	const shiftChroma = "extractplanes=y+u+v[y][u][v];[u]crop=iw-4:ih:4:0,pad=iw+4:ih:0:0[u2];[v]crop=iw-4:ih:4:0,pad=iw+4:ih:0:0[v2];[y][u2][v2]mergeplanes=0x001020:yuv444p"
	x265 := []string{"-c:v", "libx265", "-preset", "ultrafast", "-crf", "22", "-pix_fmt", "yuv420p", "-x265-params", "log-level=error"}
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=3", "-c:v", "ffv1", "-pix_fmt", "yuv444p", path("source444.mkv")},
		{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=3", "-c:v", "ffv1", "-pix_fmt", "yuv420p", path("source420.mkv")},
		append([]string{"-i", path("source444.mkv")}, append(x265, path("good.mkv"))...),
		append([]string{"-i", path("source444.mkv"), "-vf", "shuffleplanes=0:2:1"}, append(x265, path("swapped.mkv"))...),
		append([]string{"-i", path("source444.mkv"), "-vf", shiftChroma}, append(x265, path("shifted.mkv"))...),
		append([]string{"-i", path("source420.mkv")}, append(x265, path("good420.mkv"))...),
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("fixture %v: %v %s", args, err, out)
		}
	}
	plan := &transcode.Plan{PixelFormat: "yuv420p", QualityValidation: &transcode.QualityValidationPlan{
		Metric:                 metric,
		Samples:                transcode.PlanFinalValidationWindows(3, 0.5, 1, 2, 2),
		BenchmarkRequestDigest: "sha256:" + strings.Repeat("3", 64),
		Quality:                quality,
	}}
	validate := func(name, source, candidate string) (*transcode.FinalQualityEvidence, error) {
		w := NewWorker(&WorkerConfig{StateDir: path("state-" + name), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1})
		return w.validateFinalPerceptual(ctx, dir, path(source), path(candidate), plan, "sha256:candidate", 1)
	}

	e, err := validate("good", "source444.mkv", "good.mkv")
	if err != nil || e.Verdict != "pass" || e.ChromaPSNR == nil {
		t.Fatalf("good: %+v %v", e, err)
	}
	t.Logf("good chroma %+v", *e.ChromaPSNR)
	if c := e.ChromaPSNR; c.MinimumDB != chromaPSNRMinimumDB || c.WorstUDB < chromaPSNRMinimumDB || c.WorstVDB < chromaPSNRMinimumDB {
		t.Fatalf("good chroma %+v", c)
	}

	e, err = validate("swapped", "source444.mkv", "swapped.mkv")
	if err == nil || e.Verdict == "pass" || !slices.Contains(e.ReasonCodes, "quality_chroma_psnr_below_minimum") {
		t.Fatalf("swapped: %+v %v", e, err)
	}
	if e.ChromaPSNR == nil || e.ChromaPSNR.WorstUDB >= chromaPSNRMinimumDB {
		t.Fatalf("swapped chroma %+v", e.ChromaPSNR)
	}
	if slices.ContainsFunc(e.ReasonCodes, func(c string) bool { return strings.HasPrefix(c, "quality_vmaf") }) {
		t.Fatalf("swapped planes failed VMAF too: %v", e.ReasonCodes)
	}

	// Misplaced colour lands between a sound encode and swapped planes, so
	// it holds the minimum where it is.
	e, err = validate("shifted", "source444.mkv", "shifted.mkv")
	if err == nil || !slices.Contains(e.ReasonCodes, "quality_chroma_psnr_below_minimum") {
		t.Fatalf("shifted: %+v %v", e, err)
	}
	t.Logf("shifted chroma %+v", *e.ChromaPSNR)

	e, err = validate("plain", "source420.mkv", "good420.mkv")
	if err != nil || e.Verdict != "pass" || e.ChromaPSNR != nil {
		t.Fatalf("4:2:0 source: %+v %v", e, err)
	}
}
