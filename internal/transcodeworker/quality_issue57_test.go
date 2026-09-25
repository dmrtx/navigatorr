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
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

func TestPooledVMAFStillValidatesFrames(t *testing.T) {
	for _, data := range []string{
		`{"pooled_metrics":{"vmaf":{"mean":95}},"frames":[{"metrics":{"vmaf":95}}]}`,
		`{"pooled_metrics":{"vmaf":{"mean":95}},"frames":[{"frameNum":0,"metrics":{"vmaf":95}},{"frameNum":2,"metrics":{"vmaf":95}}]}`,
		`{"pooled_metrics":{"vmaf":{"mean":95}},"frames":[{"frameNum":0,"metrics":{"vmaf":90}}]}`,
		`{"pooled_metrics":{"vmaf":{"mean":95}},"frames":[{"frameNum":0,"metrics":{}}]}`,
	} {
		if _, err := ParseVMAFJSON([]byte(data)); err == nil {
			t.Errorf("accepted corrupt pooled/frame evidence %s", data)
		}
	}
}

func TestQualityGuardrailRejectsCandidateAndObserveDoesNot(t *testing.T) {
	model, _ := quality.Model("v1_1080p_3h")
	stats, err := quality.ParseVMAF([]byte(`{"frames":[{"frameNum":0,"metrics":{"vmaf":90}},{"frameNum":1,"metrics":{"vmaf":100}}]}`), 4096, model, 0, 10, 1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		enforcement string
		rejected    bool
	}{{"observe", false}, {"reject", true}} {
		minimum := 95.0
		record := &BenchmarkRecord{Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 10, DurationSeconds: 2}}, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: model.ID, Target: 96, Minimum: 90, GuardrailEnforcement: tc.enforcement, P5Minimum: &minimum, WorstWindowSeconds: 2}}}
		sh := &pipelineShared{record: record, evidence: &BenchmarkExecutionEvidence{}, vcs: []validatedCandidate{{index: 0, candidate: transcode.BenchmarkCandidate{ID: "c1"}}}, runVMAF: true, vmafModel: &model, metricOut: []pipelineMetricOutcome{{executed: true, vmafStats: &stats, vmafScores: []optimization.SampleScore{{SampleIndex: 0, Score: 95, Valid: true}}, entry: &BenchmarkMetricSampleResult{CandidateID: "c1", SampleIndex: 0}}}, encodeOut: []pipelineEncodeOutcome{{entry: BenchmarkCandidateSampleResult{CandidateID: "c1", SampleIndex: 0, SizeBytes: 100}}}}
		sh.assembleEvidence([]pipelineUnit{{vcOrd: 0, sampOrd: 0}})
		if got := !sh.evidence.CandidateMetrics[0].Aggregate.Valid; got != tc.rejected {
			t.Errorf("enforcement=%s rejected=%t want %t", tc.enforcement, got, tc.rejected)
		}
		if len(sh.evidence.Quality) != 1 || sh.evidence.Quality[0].ReasonCodes[0] != "quality_vmaf_p5_below_minimum" {
			t.Fatalf("missing reason: %+v", sh.evidence.Quality)
		}
	}
}

func TestFinalSampleAlignmentRequiresFrameAndTimestampCorrespondence(t *testing.T) {
	if err := verifySampleAlignment([]float64{0, 0.04, 0.08}, []float64{1, 1.04, 1.08}, 25); err != nil {
		t.Fatal(err)
	}
	if err := verifySampleAlignment([]float64{0, 0.04}, []float64{0}, 25); err == nil {
		t.Fatal("accepted missing candidate frame")
	}
	if err := verifySampleAlignment([]float64{0, 0.04}, []float64{0, 0.08}, 25); err == nil {
		t.Fatal("accepted drifted candidate frame")
	}
}

func TestExecutableQualityProbeDistinguishesUnsupportedModelFromCAMBI(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "fake-ffmpeg")
	script := `#!/bin/sh
filter=""
for arg in "$@"; do case "$arg" in *libvmaf*) filter="$arg";; esac; done
case "$filter" in
  *vmaf_v1.0.16_3d0h*) echo 'libvmaf ERROR could not initialize feature extractor "Speed_chroma_feature_speed_chroma_uv_score"' >&2; exit 1;;
  *cambi*)
    log=$(printf '%s' "$filter" | sed -n 's/.*log_path=\([^:]*\).*/\1/p')
    cat > "$log" <<'EOF'
{"version":"3.2.0","frames":[{"frameNum":0,"metrics":{"cambi_full_reference":0}},{"frameNum":1,"metrics":{"cambi_full_reference":0}}]}
EOF
    exit 0;;
esac
exit 1
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	caps := probeQualityCapabilities(context.Background(), ffmpeg, dir)
	if caps.ProbeError != "" || caps.Models["v1_1080p_3h"].Available || !caps.CAMBIFullRef || caps.CAMBIOutput != "cambi_full_reference" {
		t.Fatalf("unexpected quality capabilities: %+v", caps)
	}
	if !strings.Contains(caps.Models["v1_1080p_3h"].Reason, "model_initialization_failed") {
		t.Fatalf("missing model failure: %+v", caps)
	}
}

func TestCheckpointWithQualityPlanRequiresMatchingPassingAttestation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate.mkv")
	if err := os.WriteFile(path, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	sha, err := hashLocalFileSHA256(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	job := &JobRecord{CandidateSHA256: sha, CandidateSizeBytes: 9, Plan: &transcode.Plan{PlanDigest: "plan", QualityValidation: &transcode.QualityValidationPlan{BenchmarkRequestDigest: "request"}}}
	if err := verifyCheckpointCandidate(context.Background(), job, path); err == nil || !strings.Contains(err.Error(), "perceptual quality attestation") {
		t.Fatalf("missing quality attestation was not rejected: %v", err)
	}
	job.QualityEvidence = &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: sha, CandidateSizeBytes: 9, PlanDigest: "plan", BenchmarkRequestDigest: "request"}
	if err := verifyCheckpointCandidate(context.Background(), job, path); err != nil {
		t.Fatalf("matching quality attestation rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("replaced!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCheckpointCandidate(context.Background(), job, path); err == nil {
		t.Fatal("accepted candidate mutation after quality validation")
	}
}

func TestFinalSampledSSIMUsesRealFrameEvidence(t *testing.T) {
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
	cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2", "-c:v", "ffv1", "-y", source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating test source: %v: %s", err, output)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, data, 0600); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: ffmpeg, FFprobe: ffprobe})
	plan := &transcode.Plan{PlanDigest: "test-plan", QualityValidation: &transcode.QualityValidationPlan{
		Metric: "ssim", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 0.5}},
		Quality:                transcode.BenchmarkQualityConfig{SSIM: &transcode.BenchmarkQualityThresholds{Minimum: 0.99}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
		BenchmarkRequestDigest: "test-request",
	}}
	evidence, err := w.validateFinalPerceptual(context.Background(), dir, source, candidate, plan, "test-sha", int64(len(data)))
	if err != nil {
		t.Fatalf("final sampled validation failed: %v; evidence=%+v", err, evidence)
	}
	if evidence.Verdict != "pass" || evidence.SSIMMean == nil || *evidence.SSIMMean < 0.99 || evidence.CandidateSHA256 != "test-sha" {
		t.Fatalf("unexpected final quality evidence: %+v", evidence)
	}
	cmd = exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=black:s=320x180:r=24:d=2", "-c:v", "ffv1", "-y", candidate)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating degraded candidate: %v: %s", err, output)
	}
	evidence, err = w.validateFinalPerceptual(context.Background(), dir, source, candidate, plan, "degraded-sha", 1)
	if err == nil || evidence == nil || evidence.Verdict != "fail" || len(evidence.ReasonCodes) == 0 || evidence.ReasonCodes[0] != "quality_ssim_mean_below_minimum" {
		t.Fatalf("degraded candidate was not rejected: %v %+v", err, evidence)
	}
}

func TestFinalQualityCommandCancellationKillsMetricProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow-metric")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runQualityCommand(ctx, path, nil); err == nil {
		t.Fatal("cancelled metric command succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("metric command did not stop promptly after cancellation: %s", elapsed)
	}
}
