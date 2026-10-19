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
