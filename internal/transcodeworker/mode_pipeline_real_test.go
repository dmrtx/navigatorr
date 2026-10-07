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
)

// This is an opt-in executable model test, not a claim of production encoder
// availability. A worker lacking the exact model reports a clear skip.
func TestRealModeBenchmarkWinnerFinalValidationAndSize(t *testing.T) {
	for _, depth := range []int{8, 10} {
		t.Run(fmt.Sprintf("%dbit", depth), func(t *testing.T) { realModePipelineFixture(t, depth) })
	}
}
func realModePipelineFixture(t *testing.T, depth int) {
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
	pixelFormat := "yuv420p"
	if depth == 10 {
		pixelFormat = "yuv420p10le"
		if !caps.SupportsNative10BitQuality() {
			t.Skip("executable native10 model path unavailable")
		}
	}
	if output, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2", "-c:v", "ffv1", "-pix_fmt", pixelFormat, source).CombinedOutput(); err != nil {
		t.Fatalf("synthetic source: %v %s", err, output)
	}
	sourceSHA := ioOptSHA(t, source)
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &WorkerConfig{StateDir: filepath.Join(dir, mode, "state"), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1}
			w := NewWorker(cfg)
			minimum, target, p5, worst := 88., 92., 84., 85.
			saving := 15.
			if mode != "size" {
				minimum, target, p5, worst = 95, 96, 93, 92
			}
			if mode == "x265_preserve" {
				saving = 0
			}
			mean, peak := 6., 16.
			policy := transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: minimum, Target: target, P5Minimum: &p5, WorstWindowMinimum: &worst, WorstWindowSeconds: 0.5, GuardrailEnforcement: "reject"}, Banding: &transcode.BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "reject", MaxMean: &mean, MaxPeak: &peak}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
			candidates := []transcode.BenchmarkCandidate{{ID: "software", VideoCodec: "libx265", Quality: 10, Preset: "medium"}}
			if mode != "x265_preserve" && caps.Encoders["hevc_videotoolbox"] {
				candidates = append(candidates, transcode.BenchmarkCandidate{ID: "hardware", VideoCodec: "hevc_videotoolbox", Quality: 90})
			}
			record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-real-" + mode, Status: "running", Source: source, SourceSHA256: sourceSHA, Metric: "vmaf", Mode: mode, SearchBudgetSeconds: 120, Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 0.5}}, Candidates: candidates, Quality: &policy}
			start := time.Now()
			if err := (&ProductionBenchmarkRunner{}).RunBenchmark(ctx, w, record); err != nil {
				t.Fatal(err)
			}
			if record.Evidence == nil || record.Evidence.Decision == nil || record.Evidence.Decision.Winner == nil {
				t.Fatalf("no measured qualifying fixture winner: %+v", record.Evidence)
			}
			winner := record.Evidence.Decision.Winner
			if mode == "x265_preserve" && winner.VideoCodec != "libx265" {
				t.Fatal("preserve selected hardware")
			}
			plan := &transcode.Plan{Mode: mode, PolicyDigest: "sha256:" + strings.Repeat("0", 64), SizePolicy: &transcode.SizePolicy{MinSavingsPercent: saving}, Container: "mkv", VideoCodec: winner.VideoCodec, Quality: winner.Quality, Preset: winner.Preset, VideoProfile: winner.VideoProfile, PixelFormat: winner.PixelFormat, ExpectedBitDepth: depth, AudioMode: "copy", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "synthetic-mode-policy-v1", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}, QualityValidation: &transcode.QualityValidationPlan{Metric: "vmaf", Samples: record.Samples, Quality: policy, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			candidate := filepath.Join(dir, mode, "candidate.mkv")
			job := &JobRecord{ID: "real-" + mode, Status: "running", Source: source, SourceSHA256: sourceSHA, Candidate: candidate, Plan: plan, CreatedAt: time.Now().UTC(), StartedAt: time.Now().UTC(), ExecutionSpecDigest: pr5Digest, StagingState: string(StagingStateNotRequired), FinalizationState: string(FinalizationStateNotRequired), LocalCandidatePath: candidate, EffectiveInputPath: source}
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
			accepted, err := LoadJob(jobFile)
			if err != nil {
				t.Fatal(err)
			}
			if accepted.Status != "completed" || accepted.QualityEvidence == nil || accepted.QualityEvidence.Verdict != "pass" || accepted.QualityEvidence.CandidateSHA256 != accepted.CandidateSHA256 || accepted.QualityEvidence.PlanDigest != plan.PlanDigest {
				t.Fatalf("final accepted evidence missing: %+v", accepted)
			}
			if err := transcode.ValidateFinalSize(sourceInfo.Size(), accepted.CandidateSizeBytes, plan.SizePolicy); err != nil {
				t.Fatal(err)
			}
			if ioOptSHA(t, source) != sourceSHA {
				t.Fatal("synthetic original changed")
			}
			t.Logf("mode=%s candidates=%d winner=%s source_bytes=%d final_candidate_bytes=%d sampled_final_vmaf=%.3f final_cambi=%.3f wall=%s", mode, len(candidates), winner.VideoCodec, sourceInfo.Size(), accepted.CandidateSizeBytes, accepted.QualityEvidence.VMAF.Mean, accepted.QualityEvidence.CAMBI.Mean, time.Since(start))
		})
	}
}
