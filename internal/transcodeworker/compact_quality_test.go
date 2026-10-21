package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// TestCompactWorkerQualityEndToEnd runs the mode preflight, a benchmark and
// final validation with libvmaf on a compact plan. The source is 50 fps
// HDR10, so the tone-mapped reference chain and the 2:1 decimated metric
// pairing of #139 are exercised together.
func TestCompactWorkerQualityEndToEnd(t *testing.T) {
	ffmpeg := requireLibvmaf(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	model := hfrTestModel(t, ctx, ffmpeg)
	shim := qualityShim(t, ffmpeg, model, false)
	useTempCompactLUTDir(t)
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, shim, dir)
	if err != nil || !caps.SupportsHFRQuality() || !caps.Encoders["libx265"] || !caps.SupportsCompactTranscode() {
		t.Skipf("decimated VMAF, libx265 or the compact chain unavailable: %v", err)
	}

	source := filepath.Join(dir, "source.mkv")
	runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=50:duration=2",
		"-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "12", "-pix_fmt", "yuv420p10le",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:repeat-headers=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50):max-cll=1000,400:log-level=error",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv", source)
	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
	if err != nil || d.Class != mediainspect.DynamicRangeHDR10 {
		t.Fatalf("source classified %+v %v", d, err)
	}
	c, err := transcode.NewCompactPlan(d, rep.MainVideo()[0], 720)
	if err != nil {
		t.Fatal(err)
	}
	threshold, limit := 50.0, 100
	policy := transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 60, Target: 70, WorstWindowSeconds: 0.5, FrameThreshold: &threshold, MaxFramesBelowThreshold: &limit, GuardrailEnforcement: "reject"}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
	samples := []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}}
	plan := compactModePlanFor(t, c, rep.MainVideo()[0].BitDepth)
	plan.Quality = 18
	plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", Samples: samples, Quality: policy, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}
	plan.PlanDigest = ""
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	worker := func(name string) *Worker {
		return NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, name), AllowedRoots: []string{dir}, FFmpeg: shim, FFprobe: ffprobe, MaxParallelJobs: 1})
	}

	t.Run("mode preflight", func(t *testing.T) {
		if err := worker("pre").validateModeSourceQuality(ctx, source, plan); err != nil {
			t.Fatalf("compact HFR HDR10 source refused: %v", err)
		}
		changed := *plan
		changed.Compact = c.Clone()
		changed.Compact.ToneMap.SourcePeakNits = 4000
		if err := worker("pre-changed").validateModeSourceQuality(ctx, source, &changed); err == nil || !strings.Contains(err.Error(), "compact_source_changed") {
			t.Fatalf("a plan frozen for another source was admitted: %v", err)
		}
	})

	t.Run("final validation", func(t *testing.T) {
		probe, err := ProbeSourceDetails(ctx, ffprobe, source)
		if err != nil {
			t.Fatal(err)
		}
		ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ensureCompactLUT(c); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(dir, "candidate.mkv")
		args, err := BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
		if err != nil {
			t.Fatal(err)
		}
		runFFmpeg(t, ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...)
		jobDir := filepath.Join(dir, "final")
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		e, err := worker("final").validateFinalPerceptual(ctx, jobDir, source, candidate, plan, "sha256:candidate", 1)
		if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil {
			t.Fatalf("final compact validation: %+v %v", e, err)
		}
		if e.CompactOutput == nil || e.CompactOutput.Verdict != "pass" || e.CompactChain == nil || e.CompactChain.ToneMapMethod != transcode.CompactToneMapPQ {
			t.Fatalf("final evidence lacks compact output/chain: %+v %+v", e.CompactOutput, e.CompactChain)
		}
		if e.VMAF.TemporalDecimation != 2 || e.VMAF.FramesTotal != 25 || e.VMAF.Mean < 60 {
			t.Fatalf("final VMAF %+v", e.VMAF)
		}
		// Scoring the SDR candidate against the untransformed HDR source
		// would be meaningless; the plain path refuses an HDR source.
		plain := *plan
		plain.Compact = nil
		if e, err := worker("final-plain").validateFinalPerceptual(ctx, jobDir, source, candidate, &plain, "sha256:candidate", 1); err == nil && e != nil && e.Verdict == "pass" {
			t.Fatal("HDR source measured without the compact chain")
		}
	})
}
