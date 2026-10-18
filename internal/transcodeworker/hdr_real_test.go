package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

const realHDR10MasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"

// realHDR10Fixture encodes a synthetic BT.2020 PQ clip carrying HDR10 static
// metadata in both the bitstream SEI and the Matroska colour element.
func realHDR10Fixture(t *testing.T, ctx context.Context, ffmpeg, path, x265Extra string, crf string) {
	t.Helper()
	params := "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:hdr10-opt=1:repeat-headers=1:master-display=" + realHDR10MasterDisplay + ":max-cll=1000,400:log-level=error"
	if x265Extra != "" {
		params = x265Extra
	}
	args := []string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
		"-c:v", "libx265", "-preset", "fast", "-crf", crf, "-pix_fmt", "yuv420p10le", "-x265-params", params}
	if x265Extra == "" {
		args = append(args, "-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv")
	}
	if output, err := exec.CommandContext(ctx, ffmpeg, append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("synthetic HDR10 clip: %v %s", err, output)
	}
}

func realVMAFMean(t *testing.T, path string) float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Pooled map[string]struct {
			Mean float64 `json:"mean"`
		} `json:"pooled_metrics"`
	}
	if err := json.Unmarshal(b, &log); err != nil {
		t.Fatal(err)
	}
	return log.Pooled["vmaf"].Mean
}

type realHDR10Env struct {
	ctx              context.Context
	ffmpeg, ffprobe  string
	dir, source, sha string
	caps             transcode.WorkerCapabilities
	hdr              *transcode.HDR10Plan
}

// newRealHDR10Env skips unless libx265 with HDR10 options, lut3d and libvmaf
// are executable, then encodes and classifies the synthetic HDR10 source.
func newRealHDR10Env(t *testing.T) *realHDR10Env {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, ffmpeg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.HDR10Transcode || !caps.Filters["libvmaf"] {
		t.Skip("executable libx265 HDR10 + lut3d + libvmaf path unavailable")
	}
	source := filepath.Join(dir, "source.mkv")
	realHDR10Fixture(t, ctx, ffmpeg, source, "", "4")
	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
	if err != nil || d.Class != mediainspect.DynamicRangeHDR10 || !d.HDR10Convertible() {
		t.Fatalf("synthetic source classified %+v %v", d, err)
	}
	hdr, err := transcode.NewHDR10Plan(d, false)
	if err != nil || hdr.MasterDisplay != realHDR10MasterDisplay || hdr.MaxCLL != 1000 || hdr.MaxFALL != 400 || hdr.ToneMap.SourcePeakNits != 1000 {
		t.Fatalf("plan %+v %v", hdr, err)
	}
	if _, err := verifySourceHDR10(ctx, ffprobe, source, &rep, hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySourceHDR10(ctx, ffprobe, source, &rep, nil); err == nil {
		t.Fatal("an HDR source without a plan must fail closed")
	}
	return &realHDR10Env{ctx: ctx, ffmpeg: ffmpeg, ffprobe: ffprobe, dir: dir, source: source, sha: ioOptSHA(t, source), caps: caps, hdr: hdr}
}

func (env *realHDR10Env) inspect(t *testing.T, path string) *mediainspect.DetailedReport {
	t.Helper()
	rep, err := mediainspect.InspectDetailed(env.ctx, env.ffprobe, path)
	if err != nil {
		t.Fatal(err)
	}
	return &rep
}

// TestRealHDR10MetadataPreservationAndToneMappedVMAF encodes with the
// production libx265 HDR10 arguments, proves the static metadata survived,
// proves a stripped encode is rejected, and checks that VMAF over the
// identical tone map discriminates quality. vmaf_v0.6.1 is used here because
// it ships with every libvmaf; production gates use the frozen v1 model.
func TestRealHDR10MetadataPreservationAndToneMappedVMAF(t *testing.T) {
	env := newRealHDR10Env(t)
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 18, Preset: "fast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, HDR10: env.hdr.Clone()}
	enc, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(env.dir, "candidate.mkv")
	args := append([]string{"-nostdin", "-v", "error", "-y", "-i", env.source, "-map", "0:v:0"}, enc...)
	if output, err := exec.CommandContext(env.ctx, env.ffmpeg, append(args, candidate)...).CombinedOutput(); err != nil {
		t.Fatalf("production HDR10 encode: %v %s", err, output)
	}
	e, err := validateHDR10Output(env.ctx, env.ffprobe, candidate, env.inspect(t, candidate), env.hdr)
	if err != nil || e.Verdict != "pass" || e.MasterDisplay != realHDR10MasterDisplay || e.MaxCLL != 1000 || e.MaxFALL != 400 || e.ColorTransfer != "smpte2084" || e.ColorPrimaries != "bt2020" || e.FramesProbed == 0 {
		t.Fatalf("HDR10 metadata not preserved: %+v %v", e, err)
	}

	// The same pixels encoded without HDR10 signalling must fail closed.
	stripped := filepath.Join(env.dir, "stripped.mkv")
	realHDR10Fixture(t, env.ctx, env.ffmpeg, stripped, "log-level=error", "10")
	if e, err := validateHDR10Output(env.ctx, env.ffprobe, stripped, env.inspect(t, stripped), env.hdr); err == nil || e.Verdict != "fail" || len(e.ReasonCodes) == 0 {
		t.Fatalf("metadata-stripped encode accepted: %+v", e)
	} else {
		t.Logf("stripped encode rejected: %v", e.ReasonCodes)
	}
	// Static metadata that differs from the source must also fail closed.
	other := *env.hdr
	other.MaxCLL, other.MaxFALL = 4000, 1000
	if _, err := validateHDR10Output(env.ctx, env.ffprobe, candidate, env.inspect(t, candidate), &other); err == nil {
		t.Fatal("a content light level mismatch was accepted")
	}

	tm, err := prepareHDRToneMap(env.dir, env.hdr)
	if err != nil {
		t.Fatal(err)
	}
	degraded := filepath.Join(env.dir, "degraded.mkv")
	realHDR10Fixture(t, env.ctx, env.ffmpeg, degraded, "", "40")
	scores := map[string]float64{}
	chain := tm.metricInputChain()
	for name, cand := range map[string]string{"identical": env.source, "candidate": candidate, "degraded": degraded} {
		logPath := filepath.Join(env.dir, name+"_vmaf.json")
		filter := "[0:v]" + chain + ",setpts=PTS-STARTPTS[d];[1:v]" + chain + ",setpts=PTS-STARTPTS[r];[d][r]libvmaf=model=version=vmaf_v0.6.1:log_fmt=json:log_path=" + escapeFFmpegFilterPath(logPath) + ":shortest=1:repeatlast=0"
		if output, err := exec.CommandContext(env.ctx, env.ffmpeg, "-nostdin", "-v", "error", "-i", cand, "-i", env.source, "-filter_complex", filter, "-f", "null", "-").CombinedOutput(); err != nil {
			t.Fatalf("tone-mapped VMAF %s: %v %s", name, err, output)
		}
		scores[name] = realVMAFMean(t, logPath)
	}
	if scores["identical"] < 99 || scores["candidate"] < 90 || scores["degraded"] > scores["candidate"]-5 {
		t.Fatalf("tone-mapped VMAF not discriminating: %+v", scores)
	}
	if ioOptSHA(t, env.source) != env.sha {
		t.Fatal("synthetic original changed")
	}
	t.Logf("tone-mapped vmaf_v0.6.1 identical=%.2f crf18=%.2f crf40=%.2f lut=%s", scores["identical"], scores["candidate"], scores["degraded"], tm.evidence.LUTSHA256)
}

// TestRealHDR10BenchmarkAndFinalValidation runs the HDR10 mode pipeline end to
// end. It needs the frozen VMAF v1 model and native 10-bit measurement.
func TestRealHDR10BenchmarkAndFinalValidation(t *testing.T) {
	env := newRealHDR10Env(t)
	caps := env.caps
	if !caps.SupportsHDR10Transcode() || caps.Quality == nil || !caps.Quality.Models["v1_1080p_3h"].Available || !caps.Quality.CAMBIFullRef {
		t.Skip("executable VMAF v1 + CAMBI + native10 path unavailable")
	}
	dir, source, sourceSHA, hdr, ctx := env.dir, env.source, env.sha, env.hdr, env.ctx
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, FFmpeg: env.ffmpeg, FFprobe: env.ffprobe, MaxParallelJobs: 1}
	w := NewWorker(cfg)
	p5, worst, mean, peak := 94., 93., 4., 12.
	policy := transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 96, Target: 97, P5Minimum: &p5, WorstWindowMinimum: &worst, WorstWindowSeconds: 0.5, GuardrailEnforcement: "reject"}, Banding: &transcode.BenchmarkBandingConfig{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "reject", MaxMean: &mean, MaxPeak: &peak}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
	samples := []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 0.5}}
	record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "bench-real-hdr10", Status: "running", Source: source, SourceSHA256: sourceSHA, Metric: "vmaf", Mode: "quality", SearchBudgetSeconds: 120, Samples: samples, Candidates: []transcode.BenchmarkCandidate{{ID: "software", VideoCodec: "libx265", Quality: 10, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le"}}, Quality: &policy, HDR10: hdr.Clone()}
	if err := (&ProductionBenchmarkRunner{}).RunBenchmark(ctx, w, record); err != nil {
		t.Fatal(err)
	}
	if record.Evidence == nil || record.Evidence.Decision == nil || record.Evidence.Decision.Winner == nil {
		b, _ := json.Marshal(record.Evidence.Decision)
		t.Fatalf("no qualifying HDR10 winner: %s", b)
	}
	if record.Evidence.ToneMap == nil || record.Evidence.ToneMap.Method != transcode.HDRToneMapMethod || record.Evidence.ToneMap.AppliedTo != "reference_and_candidate" {
		t.Fatalf("benchmark tone map evidence %+v", record.Evidence.ToneMap)
	}
	winner := record.Evidence.Decision.Winner
	plan := &transcode.Plan{Mode: "quality", PolicyDigest: "sha256:" + strings.Repeat("0", 64), SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 15}, Container: "mkv", VideoCodec: winner.VideoCodec, Quality: winner.Quality, Preset: winner.Preset, VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "copy", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "synthetic-mode-policy-v1", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}, HDR10: hdr.Clone(), QualityValidation: &transcode.QualityValidationPlan{Metric: "vmaf", Samples: samples, Quality: policy, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	candidate := filepath.Join(dir, "final.mkv")
	job := &JobRecord{ID: "real-hdr10", Status: "running", Source: source, SourceSHA256: sourceSHA, Candidate: candidate, Plan: plan, CreatedAt: time.Now().UTC(), StartedAt: time.Now().UTC(), ExecutionSpecDigest: pr5Digest, StagingState: string(StagingStateNotRequired), FinalizationState: string(FinalizationStateNotRequired), LocalCandidatePath: candidate, EffectiveInputPath: source}
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
	q := accepted.QualityEvidence
	if accepted.Status != "completed" || q == nil || q.Verdict != "pass" || q.ToneMap == nil || q.HDR10Output == nil || q.HDR10Output.Verdict != "pass" || q.HDR10Output.MasterDisplay != realHDR10MasterDisplay {
		t.Fatalf("HDR10 final evidence missing: %+v", accepted)
	}
	if ioOptSHA(t, source) != sourceSHA {
		t.Fatal("synthetic original changed")
	}
	t.Logf("final_vmaf=%.3f final_cambi=%.3f", q.VMAF.Mean, q.CAMBI.Mean)
}
