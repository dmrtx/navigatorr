package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

func TestBuildVMAFModelArgsDecimatesBothInputsIdentically(t *testing.T) {
	model, _ := quality.Model("v1_1080p_3h")
	native, err := BuildVMAFModelArgs("c.mkv", "r.mkv", "v.json", model, 8, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filterOf(native), "select") {
		t.Fatalf("native-rate graph decimated: %s", filterOf(native))
	}
	args, err := BuildVMAFModelArgs("c.mkv", "r.mkv", "v.json", model, 8, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	graph := filterOf(args)
	branches := strings.Split(graph, ";")
	if len(branches) != 3 {
		t.Fatalf("unexpected graph shape: %s", graph)
	}
	want := `format=yuv420p10le,setpts=PTS-STARTPTS,select=not(mod(n\,2))`
	if branches[0] != "[0:v]"+want+"[d]" || branches[1] != "[1:v]"+want+"[r]" {
		t.Fatalf("reference and candidate must drop the same frame indices after PTS normalization: %s", graph)
	}
	if strings.Count(graph, "select=") != 2 || !strings.HasPrefix(branches[2], "[d][r]libvmaf=") || !strings.Contains(branches[2], ":shortest=1:repeatlast=0") {
		t.Fatalf("decimation leaked into the metric options: %s", graph)
	}
	for _, bad := range []int{0, 3} {
		if _, err := BuildVMAFModelArgs("c.mkv", "r.mkv", "v.json", model, 8, bad, 0); err == nil {
			t.Fatalf("decimation %d accepted", bad)
		}
	}
}

func filterOf(args []string) string {
	for i, arg := range args {
		if arg == "-filter_complex" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hfrTestModel returns the production model when this libvmaf can run it.
// Otherwise vmaf_v0.6.1 (same temporal feature family) exercises the exact
// decimated graph, so the alignment and score checks still run locally.
func hfrTestModel(t *testing.T, ctx context.Context, ffmpeg string) quality.VMAFModel {
	t.Helper()
	model, _ := quality.Model("v1_1080p_3h")
	if probeQualityCapabilities(ctx, ffmpeg, t.TempDir()).Models[model.ID].Available {
		return model
	}
	model.ID, model.LibvmafModel = "test_v0_6_1", "vmaf_v0.6.1"
	return model
}

func requireLibvmaf(t *testing.T) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	out, err := exec.Command(ffmpeg, "-hide_banner", "-filters").CombinedOutput()
	if err != nil || !strings.Contains(string(out), " libvmaf ") {
		t.Skip("libvmaf filter unavailable")
	}
	return ffmpeg
}

func TestProbeHFRDecimationExecutesDecimatedGraph(t *testing.T) {
	ffmpeg := requireLibvmaf(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	model := hfrTestModel(t, ctx, ffmpeg)
	if got := probeHFRDecimation(ctx, ffmpeg, t.TempDir(), model); got != quality.HFRDecimationFactor {
		t.Fatalf("decimated probe failed on a working libvmaf: %d", got)
	}
	model.LibvmafModel = "vmaf_model_that_does_not_exist"
	if got := probeHFRDecimation(ctx, ffmpeg, t.TempDir(), model); got != 0 {
		t.Fatalf("failed probe advertised HFR: %d", got)
	}
}

// A synthetic 50 fps source: an identical copy must score near the top, a
// downscaled, heavily quantized copy clearly lower, and a one-frame shift must not be
// hidden by decimation.
func TestHFRDecimatedVMAFRealScores(t *testing.T) {
	ffmpeg := requireLibvmaf(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	model := hfrTestModel(t, ctx, ffmpeg)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-y", "-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v: %s", args, err, out)
		}
	}
	ref := filepath.Join(dir, "reference.mkv")
	low := filepath.Join(dir, "compressed.mkv")
	shifted := filepath.Join(dir, "shifted.mkv")
	run("-f", "lavfi", "-i", "testsrc2=size=960x540:rate=50:duration=2", "-pix_fmt", "yuv420p", "-c:v", "ffv1", ref)
	run("-i", ref, "-vf", "scale=240:136,scale=960:540", "-c:v", "mpeg4", "-q:v", "31", "-pix_fmt", "yuv420p", low)
	run("-i", ref, "-vf", `select=gte(n\,1),setpts=PTS-STARTPTS`, "-c:v", "ffv1", shifted)

	measure := func(candidate string) quality.VMAFStats {
		t.Helper()
		logPath := filepath.Join(dir, "vmaf.json")
		args, err := BuildVMAFModelArgs(candidate, ref, logPath, model, 8, quality.HFRDecimationFactor, 0)
		if err != nil {
			t.Fatal(err)
		}
		run(args[1:]...) // drop -nostdin; run adds it
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 0, 0, 50, quality.HFRDecimationFactor, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	same := measure(ref)
	if same.FramesTotal != quality.DecimatedFrameCount(100, 2) || same.MeasuredFPS != 25 || same.TemporalDecimation != 2 {
		t.Fatalf("decimation did not keep every second frame: %+v", same)
	}
	if same.Mean < 95 || same.P5 < 90 {
		t.Fatalf("identical HFR copy scored too low: mean %.2f p5 %.2f", same.Mean, same.P5)
	}
	lowStats := measure(low)
	if lowStats.Mean > same.Mean-15 {
		t.Fatalf("heavily compressed copy not clearly lower: %.2f vs %.2f", lowStats.Mean, same.Mean)
	}
	shift := measure(shifted)
	if shift.Mean > same.Mean-15 {
		t.Fatalf("one-frame misalignment hidden by decimation: %.2f vs %.2f", shift.Mean, same.Mean)
	}
	t.Logf("model=%s identical=%.2f compressed=%.2f shifted=%.2f", model.LibvmafModel, same.Mean, lowStats.Mean, shift.Mean)
}
