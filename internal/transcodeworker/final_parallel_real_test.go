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

// Parallel final validation reports exactly the sequential evidence: the same
// per-window VMAF and CAMBI statistics in the same order, although windows run
// concurrently with a different libvmaf thread count. Per-window extracts are
// gone once validation returns.
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
		e, err := w.validateFinalPerceptual(withCPUMeter(ctx, meter), jobDir, source, candidate, plan, "sha256:candidate", 1)
		if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil || e.CAMBI == nil {
			t.Fatalf("%s: %+v %v", name, e, err)
		}
		// Extraction, VMAF and CAMBI children are all charged to the phase.
		if ms := meter.activeComputeMs(); ms == nil || *ms <= 0 {
			t.Fatalf("%s: final validation CPU not measured: %v", name, ms)
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
// verdict or the pooled statistics the limits are checked on.
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
	// A fresh window that cannot be measured is dropped, not a failure.
	broken := run("broken", plan([]transcode.BenchmarkSampleWindow{{Index: 4, StartSeconds: 3.9, DurationSeconds: 0.5}}))
	if !reflect.DeepEqual(base.VMAF, broken.VMAF) || broken.FreshVMAF != nil || len(broken.Windows) != 4 {
		t.Fatalf("an unmeasurable fresh window changed the result: %+v", broken)
	}
}
