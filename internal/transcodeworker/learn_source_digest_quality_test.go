package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// A job whose plan validates final quality, submitted with no digest: the
// worker learns it from the input it will encode, and the quality gate (which
// refuses to run without an immutable source digest) uses that learned digest
// end to end. Needs a real ffmpeg with libx265 and ssim; skipped without them.
func TestDigestlessQualityPlanValidatesAgainstTheLearnedDigest(t *testing.T) {
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
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, ffmpeg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Encoders["libx265"] {
		t.Skip("libx265 unavailable")
	}
	source := filepath.Join(dir, "source.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2", "-c:v", "ffv1", "-pix_fmt", "yuv420p", source).CombinedOutput(); err != nil {
		t.Fatalf("synthetic source: %v %s", err, out)
	}
	sourceSHA := ioOptSHA(t, source)
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1}
	w := NewWorker(cfg)
	plan := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 10, Preset: "ultrafast", ExpectedBitDepth: 8, AudioMode: "copy", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "synthetic-learn-v1", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}, QualityValidation: &transcode.QualityValidationPlan{
		Metric:                 "ssim",
		Samples:                []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 0.5}},
		Quality:                transcode.BenchmarkQualityConfig{SSIM: &transcode.BenchmarkQualityThresholds{Minimum: 0.5}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
		BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64),
	}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	candidate := filepath.Join(dir, "candidate.mkv")
	job := &JobRecord{ID: "learn-quality", Status: "running", Source: source, ExpectedSourceSizeBytes: info.Size(), SourceSizeBytes: info.Size(), Candidate: candidate, Plan: plan, CreatedAt: time.Now().UTC(), StartedAt: time.Now().UTC(), ExecutionSpecDigest: pr5Digest, StagingState: string(StagingStateNotRequired), FinalizationState: string(FinalizationStateNotRequired), LocalCandidatePath: candidate, EffectiveInputPath: source}
	jobDir := filepath.Join(cfg.StateDir, job.ID)
	jobFile := filepath.Join(jobDir, "job.json")
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	operational, err := resolveOperationalForExecution(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.executeOperational(ctx, jobDir, jobFile, job, operational); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.QualityEvidence == nil || got.QualityEvidence.Verdict != "pass" {
		t.Fatalf("status=%q err=%q evidence=%+v", got.Status, got.Error, got.QualityEvidence)
	}
	if got.LearnedSourceSHA256 != sourceSHA || got.SourceSHA256 != "" {
		t.Fatalf("learned %q (request digest %q), want %q learned and no request digest", got.LearnedSourceSHA256, got.SourceSHA256, sourceSHA)
	}
	if strings.Contains(got.Error, "quality_final_source_identity_missing") {
		t.Fatalf("the quality gate did not see the learned digest: %s", got.Error)
	}
}
