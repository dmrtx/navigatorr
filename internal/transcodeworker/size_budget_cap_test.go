package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// A candidate over the plan's absolute byte ceiling is refused right after the
// encode, before any quality scoring, and the failed job reports the reason
// code and the size so the coordinator can move to its next step.
func TestOverCapCandidateIsRefusedBeforeQualityValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate int
		refused   bool
	}{{"equal to the cap", 60, false}, {"one byte over", 61, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), string(make([]byte, 100)))
			candidate := filepath.Join(dir, "external", "candidate.mkv")
			cfg := pr6b2Config(dir, func(cfg *WorkerConfig) { cfg.ExternalRoots = []string{filepath.Join(dir, "external")} })
			w := NewWorker(cfg)
			newStubSpawn().install(w)
			rec := &pr6b2Recorder{createOut: true}
			rec.install(w)
			w.SetRunFFmpeg(func(ctx context.Context, plan *ExecutionPlan, job *JobRecord, input, output, progress, log string) error {
				if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
					return err
				}
				return os.WriteFile(output, make([]byte, tc.candidate), 0600)
			})
			plan := ioOptPlan(t)
			plan.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: 15, MaxCandidateBytes: 60}
			// A final quality plan that cannot be scored: had the worker got
			// that far it would record quality evidence for the job.
			plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}}, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			if _, err := w.Submit(context.Background(), SubmitRequest{ID: "capped", SourcePath: source, CandidatePath: candidate, Plan: plan}, "unused", ""); err != nil {
				t.Fatal(err)
			}
			err := pr6b2Run(t, w, "capped")
			job := pr6b1LoadJob(t, cfg.StateDir, "capped")
			if err == nil {
				t.Fatalf("the plan cannot pass final quality validation, so the job must fail: %+v", job)
			}
			if tc.refused {
				if job.ReasonCode != "size_budget_exceeded" || job.FailureClassification != "policy_rejected" || job.QualityEvidence != nil || job.CandidateSizeBytes != int64(tc.candidate) {
					t.Fatalf("over-cap candidate must be refused before quality scoring with its size: %+v", job)
				}
				return
			}
			if job.ReasonCode == "size_budget_exceeded" {
				t.Fatalf("a candidate equal to the cap must not be refused for its size: %+v", job)
			}
		})
	}
}
