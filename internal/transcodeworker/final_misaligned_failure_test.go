package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// A window whose source and candidate extracts differ in frame count fails the
// job with the window named and both extracts' timestamps in the message, and
// with a class no recipe can make retryable: the encode before this check runs
// for hours and the same cut misaligns the same way on a replay.
func TestFinalMisalignedWindowNamesTheWindowAndIsNeverRetried(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	source, candidate := filepath.Join(dir, "source.mkv"), filepath.Join(dir, "candidate.mkv")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2", "-c:v", "ffv1", "-y", source},
		// The candidate loses four pictures inside the window.
		{"-i", source, "-vf", "select=not(between(n\\,14\\,17))", "-fps_mode", "passthrough", "-c:v", "ffv1", "-y", candidate},
	} {
		if output, err := exec.Command(ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("creating test media: %v: %s", err, output)
		}
	}
	size, err := os.Stat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: ffmpeg, FFprobe: ffprobe})
	plan := &transcode.Plan{PlanDigest: "test-plan", QualityValidation: &transcode.QualityValidationPlan{
		Metric: "ssim", Samples: []transcode.BenchmarkSampleWindow{{Index: 3, StartSeconds: 0.5, DurationSeconds: 0.5}},
		Quality:                transcode.BenchmarkQualityConfig{SSIM: &transcode.BenchmarkQualityThresholds{Minimum: 0.99}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
		BenchmarkRequestDigest: "test-request",
	}}
	evidence, err := w.validateFinalPerceptual(context.Background(), dir, source, candidate, plan, "test-sha", size.Size())
	if err == nil || evidence == nil || evidence.Verdict != "fail" || len(evidence.ReasonCodes) == 0 || evidence.ReasonCodes[0] != "quality_final_sample_misaligned" {
		t.Fatalf("misaligned candidate was not rejected as misaligned: %v %+v", err, evidence)
	}
	for _, want := range []string{"window 3", "frame count mismatch: 12 versus 8", "source timestamps [", "candidate timestamps ["} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	class := resilience.ClassifyError(err)
	if class != resilience.CandidateInvalid {
		t.Fatalf("class %q", class)
	}
	if resilience.Retryable(class, []string{string(class)}) {
		t.Fatal("a recipe can make a misaligned final window retryable")
	}
}
