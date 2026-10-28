package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

func TestBuildVMAFModelArgsDecimatesBothInputsIdentically(t *testing.T) {
	model, _ := quality.Model("v1_1080p_3h")
	native, err := BuildVMAFModelArgs("c.mkv", "r.mkv", "v.json", model, 8, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if graph := filterOf(native); strings.Contains(graph, "select") || !strings.HasPrefix(graph, "[0:v]format=yuv420p10le,settb=AVTB,setpts=N[d];[1:v]format=yuv420p10le,settb=AVTB,setpts=N[r];[d][r]libvmaf=") {
		t.Fatalf("native-rate graph must pair by frame index without decimating: %s", graph)
	}
	cambi := filterOf(BuildCAMBIArgs("c.mkv", "r.mkv", "c.json", 8, 0))
	if strings.Contains(cambi, "select") || strings.Count(cambi, "settb=AVTB,setpts=N") != 2 {
		t.Fatalf("CAMBI must pair every frame by index: %s", cambi)
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
	want := `format=yuv420p10le,select=not(mod(n\,2)),settb=AVTB,setpts=N`
	if branches[0] != "[0:v]"+want+"[d]" || branches[1] != "[1:v]"+want+"[r]" {
		t.Fatalf("reference and candidate must drop the same frame indices, then restamp by index: %s", graph)
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
	if got, reason := probeHFRDecimation(ctx, ffmpeg, t.TempDir(), model); got != quality.HFRDecimationFactor || reason != "" {
		t.Fatalf("decimated probe failed on a working libvmaf: %d %q", got, reason)
	}
	broken := model
	broken.LibvmafModel = "vmaf_model_that_does_not_exist"
	if got, reason := probeHFRDecimation(ctx, ffmpeg, t.TempDir(), broken); got != 0 || reason != "hfr_probe_execution_failed" {
		t.Fatalf("failed probe advertised HFR: %d %q", got, reason)
	}
	expired, cancelExpired := context.WithCancel(ctx)
	cancelExpired()
	if got, reason := probeHFRDecimation(expired, ffmpeg, t.TempDir(), model); got != 0 || reason != hfrProbeTimeout {
		t.Fatalf("cancelled probe must report a retryable timeout: %d %q", got, reason)
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
		runFFmpeg(t, ctx, ffmpeg, append([]string{"-y", "-nostdin", "-v", "error"}, args...)...)
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
		runFFmpeg(t, ctx, ffmpeg, args...)
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

func runFFmpeg(t *testing.T, ctx context.Context, ffmpeg string, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v: %s", args, err, out)
	}
}

// Regression: lossless extraction rounds both samples to a 1 ms timebase, and
// at 60000/1001 a source and a differently muxed candidate can disagree by
// 1 ms on some frames. verifySampleAlignment tolerates that, so the metric
// graph must pair by frame index; pairing by timestamp matched those frames
// with a neighbour and scored them far below the rest.
func TestMetricGraphPairsNTSCHFRByFrameIndex(t *testing.T) {
	ffmpeg := requireLibvmaf(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	encoders, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil || !strings.Contains(string(encoders), " libx264 ") || !strings.Contains(string(encoders), " libx265 ") {
		t.Skip("libx264/libx265 unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	model := hfrTestModel(t, ctx, ffmpeg)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	candidate := filepath.Join(dir, "candidate.mkv")
	runFFmpeg(t, ctx, ffmpeg, "-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=960x540:rate=60000/1001:duration=6", "-c:v", "libx264", "-crf", "10", "-pix_fmt", "yuv420p", source)
	runFFmpeg(t, ctx, ffmpeg, "-y", "-nostdin", "-v", "error", "-i", source, "-c:v", "libx265", "-crf", "20", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p", candidate)
	refSample := filepath.Join(dir, "ref.mkv")
	candSample := filepath.Join(dir, "cand.mkv")
	extractSampleWindow(t, ctx, ffmpeg, ffprobe, source, refSample, 3.217, 2, 60000.0/1001)
	extractSampleWindow(t, ctx, ffmpeg, ffprobe, candidate, candSample, 3.217, 2, 60000.0/1001)
	refPTS, err := probeSamplePTS(ctx, ffprobe, refSample)
	if err != nil {
		t.Fatal(err)
	}
	candPTS, err := probeSamplePTS(ctx, ffprobe, candSample)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySampleAlignment(refPTS, candPTS, 60000.0/1001); err != nil {
		t.Fatalf("samples must pass the production alignment check: %v", err)
	}
	if len(refPTS) != len(candPTS) {
		t.Fatalf("sample frame counts differ: %d vs %d", len(refPTS), len(candPTS))
	}
	offset := 0
	for i := range refPTS {
		if refPTS[i]-refPTS[0] != candPTS[i]-candPTS[0] {
			offset++
		}
	}
	for _, decimation := range []int{1, quality.HFRDecimationFactor} {
		logPath := filepath.Join(dir, "vmaf.json")
		args, err := BuildVMAFModelArgs(candSample, refSample, logPath, model, 8, decimation, 0)
		if err != nil {
			t.Fatal(err)
		}
		runFFmpeg(t, ctx, ffmpeg, append([]string{"-v", "error"}, args...)...)
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 0, 0, 60000.0/1001, decimation, 0.5, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stats.FramesTotal != quality.DecimatedFrameCount(len(refPTS), decimation) {
			t.Fatalf("decimation %d measured %d of %d frames", decimation, stats.FramesTotal, len(refPTS))
		}
		// A crf 20 copy of this pattern scores ~95 on every correctly paired
		// frame; a frame paired with its neighbour drops to ~30-50.
		if stats.Min < 80 {
			t.Fatalf("decimation %d: a frame was paired with its neighbour (min %.2f, mean %.2f; %d samples with 1 ms timestamp offsets)", decimation, stats.Min, stats.Mean, offset)
		}
		t.Logf("decimation=%d frames=%d min=%.2f mean=%.2f timestamp_offsets=%d", decimation, stats.FramesTotal, stats.Min, stats.Mean, offset)
	}
	// SSIM pairs by index too: every frame of the crf 20 copy scores ~0.995;
	// a frame paired with its neighbour drops to ~0.93.
	ssimLog := filepath.Join(dir, "ssim.log")
	runFFmpeg(t, ctx, ffmpeg, append([]string{"-v", "error"}, BuildSSIMArgs(candSample, refSample, ssimLog)...)...)
	ssimData, err := os.ReadFile(ssimLog)
	if err != nil {
		t.Fatal(err)
	}
	ssimFrames, ssimMin := 0, 1.0
	for _, line := range strings.Split(strings.TrimSpace(string(ssimData)), "\n") {
		var all float64
		if i := strings.Index(line, "All:"); i < 0 {
			t.Fatalf("unexpected SSIM stats line %q", line)
		} else if _, err := fmt.Sscanf(line[i:], "All:%f", &all); err != nil {
			t.Fatalf("SSIM stats line %q: %v", line, err)
		}
		ssimFrames++
		ssimMin = min(ssimMin, all)
	}
	if ssimFrames != len(refPTS) || ssimMin < 0.98 {
		t.Fatalf("SSIM paired a frame with its neighbour: %d of %d frames, min %.4f", ssimFrames, len(refPTS), ssimMin)
	}
	t.Logf("ssim frames=%d min=%.4f", ssimFrames, ssimMin)
	// Without an offset, timestamp pairing would have scored the same, so the
	// regression was not exercised (a different ffmpeg muxes differently).
	if offset == 0 {
		t.Skip("this ffmpeg produced no 1 ms timestamp offsets; index pairing regression not exercised")
	}
}

// qualityShim forwards to the real ffmpeg but measures with vmaf_v0.6.1 when
// this libvmaf cannot run the v1 model, so the production gates, graphs and
// evidence run end to end. noHFR makes the 48 fps HFR probe fail, as on a
// worker that cannot prove decimation.
func qualityShim(t *testing.T, ffmpeg string, model quality.VMAFModel, noHFR bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	rewrite := ""
	if model.LibvmafModel != "vmaf_v1.0.16_3d0h" {
		rewrite = `a=$(printf '%s' "$a" | sed 's/vmaf_v1\.0\.16_3d0h/vmaf_v0.6.1/g'); `
	}
	refuse := ""
	if noHFR {
		refuse = `case "$*" in *rate=48:*) echo "simulated HFR probe failure" >&2; exit 1;; esac` + "\n"
	}
	script := "#!/bin/sh\n" + refuse + "for a do shift; " + rewrite + "set -- \"$@\" \"$a\"; done\nexec \"" + ffmpeg + "\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHFRWorkerGatesEndToEnd(t *testing.T) {
	ffmpeg := requireLibvmaf(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	model := hfrTestModel(t, ctx, ffmpeg)
	capable, older := qualityShim(t, ffmpeg, model, false), qualityShim(t, ffmpeg, model, true)
	dir := t.TempDir()
	if caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, capable, dir); err != nil || !caps.SupportsHFRQuality() || !caps.Encoders["libx265"] {
		t.Skip("decimated VMAF or libx265 unavailable through the shim")
	}
	if caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, older, dir); err != nil || caps.SupportsHFRQuality() || caps.Quality.HFRProbeReason != "hfr_probe_execution_failed" || !caps.Quality.Models["v1_1080p_3h"].Available || caps.Quality.ProbeError != "" {
		t.Fatalf("a failed HFR probe must only withhold HFR: %+v %v", caps.Quality, err)
	}
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=50:duration=2", "-c:v", "ffv1", "-pix_fmt", "yuv420p", source)
	runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-i", source, "-c:v", "libx265", "-crf", "18", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p", candidate)
	worker := func(ffmpegPath, name string) *Worker {
		return NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, name), AllowedRoots: []string{dir}, FFmpeg: ffmpegPath, FFprobe: ffprobe, MaxParallelJobs: 1})
	}
	threshold, limit := 50.0, 100
	policy := transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 60, Target: 70, WorstWindowSeconds: 0.5, FrameThreshold: &threshold, MaxFramesBelowThreshold: &limit, GuardrailEnforcement: "reject"}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
	samples := []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}}
	plan := &transcode.Plan{Mode: "size", PlanDigest: "sha256:" + strings.Repeat("2", 64), QualityValidation: &transcode.QualityValidationPlan{Metric: "vmaf", Samples: samples, Quality: policy, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}}

	t.Run("mode preflight", func(t *testing.T) {
		if err := worker(capable, "pre-capable").validateModeSourceQuality(ctx, source, plan); err != nil {
			t.Fatalf("capable worker refused a 50 fps source: %v", err)
		}
		if err := worker(older, "pre-older").validateModeSourceQuality(ctx, source, plan); err == nil || !strings.Contains(err.Error(), "quality_source_class_unsupported") {
			t.Fatalf("worker without the capability admitted a 50 fps source: %v", err)
		}
	})

	t.Run("final validation", func(t *testing.T) {
		for _, name := range []string{"final-capable", "final-older"} {
			if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		evidence, err := worker(capable, "final-capable").validateFinalPerceptual(ctx, filepath.Join(dir, "final-capable"), source, candidate, plan, "sha256:candidate", 1)
		if err != nil || evidence == nil || evidence.Verdict != "pass" || evidence.VMAF == nil {
			t.Fatalf("final HFR validation: %+v %v", evidence, err)
		}
		if evidence.VMAF.TemporalDecimation != 2 || evidence.VMAF.MeasuredFPS != 25 || evidence.VMAF.FramesTotal != 25 {
			t.Fatalf("final evidence lacks the 2:1 decimation of 50 source frames: %+v", evidence.VMAF)
		}
		evidence, err = worker(older, "final-older").validateFinalPerceptual(ctx, filepath.Join(dir, "final-older"), source, candidate, plan, "sha256:candidate", 1)
		if err == nil || evidence == nil || evidence.Verdict != "fail" || len(evidence.ReasonCodes) == 0 || evidence.ReasonCodes[0] != "quality_vmaf_hfr_model_unavailable" {
			t.Fatalf("final validation without the capability: %+v %v", evidence, err)
		}
	})
}

// A slow HFR probe times out on its own budget: native-rate VMAF/CAMBI stay
// available, the reason says why HFR is withheld, and the result is not
// cached so the next lookup probes again.
func TestHFRProbeTimeoutKeepsNativeQualityAndRetriesOnlyHFR(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "fake-ffmpeg")
	hfrCalls := filepath.Join(dir, "hfr-calls")
	nativeCalls := filepath.Join(dir, "native-calls")
	script := `#!/bin/sh
filter=""
for arg in "$@"; do case "$arg" in *libvmaf*) filter="$arg";; esac; done
log=$(printf '%s' "$filter" | sed -n 's/.*log_path=\([^:]*\).*/\1/p')
case "$*" in
  *rate=48:*) echo x >> "` + hfrCalls + `"; exec sleep 5;;
  *cambi=*|*name=cambi*)
    echo '{"version":"3.2.0","frames":[{"frameNum":0,"metrics":{"cambi_full_reference":0}}]}' > "$log"; exit 0;;
  *vmaf_v1.0.16_3d0h*)
    echo x >> "` + nativeCalls + `"
    echo '{"version":"3.2.0","frames":[{"frameNum":0,"metrics":{"vmaf":99}},{"frameNum":1,"metrics":{"vmaf":99}}]}' > "$log"; exit 0;;
esac
exit 1
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("%s:%d:%d", ffmpeg, info.Size(), info.ModTime().UnixNano())
	t.Cleanup(func() {
		qualityProbeCache.Lock()
		delete(qualityProbeCache.results, key)
		delete(qualityProbeCache.hfrTimedOut, key)
		qualityProbeCache.Unlock()
	})
	old := hfrProbeTimeoutDuration
	hfrProbeTimeoutDuration = 200 * time.Millisecond
	defer func() { hfrProbeTimeoutDuration = old }()
	count := func(path string) int {
		data, _ := os.ReadFile(path)
		return strings.Count(string(data), "x")
	}
	lookup := func(wantHFR, wantNative int) {
		t.Helper()
		caps := cachedQualityCapabilities(context.Background(), ffmpeg, dir)
		if caps.ProbeError != "" || !caps.Models["v1_1080p_3h"].Available || !caps.CAMBIFullRef {
			t.Fatalf("slow HFR probe disabled native quality: %+v", caps)
		}
		if caps.HFRTemporalDecimation != 0 || caps.HFRProbeReason != hfrProbeTimeout {
			t.Fatalf("timed-out HFR probe: %+v", caps)
		}
		if got := count(hfrCalls); got != wantHFR {
			t.Fatalf("HFR probes = %d, want %d", got, wantHFR)
		}
		if got := count(nativeCalls); got != wantNative {
			t.Fatalf("native probes = %d, want %d", got, wantNative)
		}
	}
	lookup(1, 1)
	// Within the retry window the timeout is served from cache.
	lookup(1, 1)
	qualityProbeCache.Lock()
	qualityProbeCache.hfrTimedOut[key] = time.Now().Add(-hfrProbeRetryAfter)
	qualityProbeCache.Unlock()
	// After it, only the HFR probe runs again; native results stay cached.
	lookup(2, 1)
	lookup(2, 1)
}

func TestHFRProbeBudgetFitsCoordinatorRequestTimeout(t *testing.T) {
	// The native probes share the same request; leave them most of it.
	if hfrProbeTimeoutDuration > httpclient.DefaultHTTPRequestTimeout/2 {
		t.Fatalf("HFR probe budget %v is not well under the coordinator timeout %v", hfrProbeTimeoutDuration, httpclient.DefaultHTTPRequestTimeout)
	}
}
