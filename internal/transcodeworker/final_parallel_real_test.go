package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Parallel final validation reports exactly the sequential evidence: the same
// per-window VMAF and CAMBI statistics in the same order, although windows run
// concurrently with a different libvmaf thread count, which also changes from
// pass to pass as the last windows take the threads the others freed
// (windowThreads). Per-window extracts are gone once validation returns, and
// each step's cost is reported.
func TestRealFinalValidationParallelMatchesSequential(t *testing.T) {
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
	if !caps.Encoders["libx265"] || caps.Quality == nil || !caps.Quality.Models["v1_1080p_3h"].Available || !caps.Quality.CAMBIFullRef {
		t.Skip("executable VMAF v1/CAMBI/libx265 path unavailable")
	}
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	for _, args := range [][]string{
		{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-c:v", "ffv1", "-pix_fmt", "yuv420p", source},
		{"-nostdin", "-v", "error", "-i", source, "-c:v", "libx265", "-preset", "ultrafast", "-crf", "30", "-x265-params", "log-level=error", candidate},
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	plan := &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{
		Metric:                 "vmaf",
		Samples:                transcode.PlanFinalValidationWindows(4, 0.5, 1, 4, 4),
		BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64),
		Quality: transcode.BenchmarkQualityConfig{
			VMAF:            &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", WorstWindowSeconds: 0.25},
			Banding:         &transcode.BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref"},
			FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"},
		},
	}}
	if len(plan.QualityValidation.Samples) != 4 {
		t.Fatalf("windows %+v", plan.QualityValidation.Samples)
	}
	run := func(name string, concurrency int) *transcode.FinalQualityEvidence {
		old := maxFinalValidationConcurrency
		maxFinalValidationConcurrency = concurrency
		defer func() { maxFinalValidationConcurrency = old }()
		jobDir := filepath.Join(dir, name)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1})
		meter := &cpuMeter{}
		steps := &validationSteps{}
		e, err := w.validateFinalPerceptual(withValidationSteps(withCPUMeter(ctx, meter), steps), jobDir, source, candidate, plan, "sha256:candidate", 1)
		if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil || e.CAMBI == nil {
			t.Fatalf("%s: %+v %v", name, e, err)
		}
		// Extraction, VMAF and CAMBI children are all charged to the phase.
		if ms := meter.activeComputeMs(); ms == nil || *ms <= 0 {
			t.Fatalf("%s: final validation CPU not measured: %v", name, ms)
		}
		// And to their own step, summed over the four windows.
		costs := steps.phaseSteps()
		var stepCPU int64
		for _, step := range []string{validationStepExtract, validationStepTimestamps, validationStepAlignment, validationStepVMAF, validationStepCAMBI} {
			c, ok := costs[step]
			if !ok || c.Attempts != 1 || c.Runs != 4 || c.ActiveComputeMs == nil {
				t.Fatalf("%s: step %s cost %+v", name, step, c)
			}
			stepCPU += *c.ActiveComputeMs
		}
		if len(costs) != 5 {
			t.Fatalf("%s: steps that did not run reported: %v", name, costs)
		}
		if stepCPU > *meter.activeComputeMs() {
			t.Fatalf("%s: steps charged %d ms, more than the phase's %d ms", name, stepCPU, *meter.activeComputeMs())
		}
		if left, _ := filepath.Glob(filepath.Join(jobDir, "quality-samples-*")); len(left) != 0 {
			t.Fatalf("%s left scratch behind: %v", name, left)
		}
		return e
	}
	sequential := run("sequential", 1)
	parallel := run("parallel", 3)
	if !reflect.DeepEqual(sequential.VMAF, parallel.VMAF) {
		t.Fatalf("VMAF differs:\nsequential %+v\nparallel   %+v", sequential.VMAF, parallel.VMAF)
	}
	if !reflect.DeepEqual(sequential.CAMBI, parallel.CAMBI) {
		t.Fatalf("CAMBI differs:\nsequential %+v\nparallel   %+v", sequential.CAMBI, parallel.CAMBI)
	}
	if !reflect.DeepEqual(sequential, parallel) {
		t.Fatalf("evidence differs:\nsequential %+v\nparallel   %+v", sequential, parallel)
	}
}

// Final validation reports every window's scores, and measures fresh windows
// (never seen by the search) beside the grid without letting them change the
// verdict or the pooled statistics the limits are checked on, unless the plan
// gates them.
func TestRealFinalValidationReportsWindowsAndFreshWindowsDoNotGate(t *testing.T) {
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
	if !caps.Encoders["libx265"] || caps.Quality == nil || !caps.Quality.Models["v1_1080p_3h"].Available {
		t.Skip("executable VMAF v1/libx265 path unavailable")
	}
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	for _, args := range [][]string{
		{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-c:v", "ffv1", "-pix_fmt", "yuv420p", source},
		{"-nostdin", "-v", "error", "-i", source, "-c:v", "libx265", "-preset", "ultrafast", "-crf", "30", "-x265-params", "log-level=error", candidate},
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	grid := transcode.PlanFinalValidationWindows(4, 0.5, 1, 4, 4)
	plan := func(fresh []transcode.BenchmarkSampleWindow) *transcode.Plan {
		return &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{
			Metric: "vmaf", Samples: grid, FreshSamples: fresh,
			BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64),
			Quality: transcode.BenchmarkQualityConfig{
				VMAF:            &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", WorstWindowSeconds: 0.25},
				FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"},
			},
		}}
	}
	run := func(name string, p *transcode.Plan) *transcode.FinalQualityEvidence {
		jobDir := filepath.Join(dir, name)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1})
		e, err := w.validateFinalPerceptual(ctx, jobDir, source, candidate, p, "sha256:candidate", 1)
		if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil {
			t.Fatalf("%s: %+v %v", name, e, err)
		}
		return e
	}
	base := run("base", plan(nil))
	if len(base.Windows) != 4 || base.FreshVMAF != nil {
		t.Fatalf("grid report %+v", base.Windows)
	}
	for i, w := range base.Windows {
		if w.Role != "grid" || w.DurationSeconds != 0.5 || w.VMAFMean <= 0 || (i > 0 && w.StartSeconds <= base.Windows[i-1].StartSeconds) {
			t.Fatalf("window row %d: %+v", i, w)
		}
	}
	fresh := transcode.FreshValidationWindows(grid, 4, 0.5, 2)
	if len(fresh) != 2 {
		t.Fatalf("fresh windows %+v", fresh)
	}
	withFresh := run("fresh", plan(fresh))
	if !reflect.DeepEqual(base.VMAF, withFresh.VMAF) || withFresh.FreshVMAF == nil || len(withFresh.Windows) != 6 {
		t.Fatalf("fresh windows changed the verdict statistics or were not reported: %+v", withFresh)
	}
	roles := map[string]int{}
	for _, w := range withFresh.Windows {
		roles[w.Role]++
	}
	if roles["grid"] != 4 || roles["fresh"] != 2 {
		t.Fatalf("roles %v", roles)
	}
	// A fresh window that cannot be measured is reported, not a failure.
	brokenWindow := []transcode.BenchmarkSampleWindow{{Index: 4, StartSeconds: 3.9, DurationSeconds: 0.5}}
	broken := run("broken", plan(brokenWindow))
	if !reflect.DeepEqual(base.VMAF, broken.VMAF) || broken.FreshVMAF != nil || len(broken.Windows) != 4 {
		t.Fatalf("an unmeasurable fresh window changed the result: %+v", broken)
	}
	if len(broken.FreshUnmeasured) != 1 || broken.FreshUnmeasured[0].StartSeconds != 3.9 || broken.FreshUnmeasured[0].ReasonCode != "quality_final_sample_misaligned" {
		t.Fatalf("unmeasurable fresh window not reported: %+v", broken.FreshUnmeasured)
	}
	// A plan that gates fresh windows fails on it like a grid window.
	gated := plan(brokenWindow)
	gated.QualityValidation.GateFreshSamples = true
	jobDir := filepath.Join(dir, "gated")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-gated"), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1})
	e, err := w.validateFinalPerceptual(ctx, jobDir, source, candidate, gated, "sha256:candidate", 1)
	if err == nil || e == nil || e.Verdict == "pass" || !slices.Contains(e.ReasonCodes, "quality_final_sample_misaligned") {
		t.Fatalf("gated plan passed an unmeasurable fresh window: %+v %v", e, err)
	}
}
