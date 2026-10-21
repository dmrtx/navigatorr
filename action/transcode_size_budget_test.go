package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

const budgetSourceBytes = int64(10_000_000_000)

// budgetReport is a 100 min 1080p film whose video stream takes 9.7 GB and
// whose lossy audio, subtitles and container take the other 300 MB.
func budgetReport() *mediainspect.DetailedReport {
	return &mediainspect.DetailedReport{DurationSec: 6000,
		Video: []mediainspect.DetailedStream{{Index: 0, Codec: "h264", BitDepth: 8, Width: 1920, Height: 1080, FPS: 24, Tags: map[string]string{"NUMBER_OF_BYTES": "9700000000"}}},
		Audio: []mediainspect.DetailedStream{{Index: 1, Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng", BitRate: 384000}},
	}
}

type budgetCase struct {
	ec       *ExecutionContext
	engine   *Engine
	executor *mockTranscodeExecutor
	snapshot *ModePolicySnapshot
	base     *transcode.Plan
	request  transcode.BenchmarkRequest
}

func budgetCaps() transcode.WorkerCapabilities {
	caps := modeTestCapabilities()
	caps.X265TwoPass = true
	caps.SizeAudioOnly = true
	return caps
}

func newBudgetCase(t *testing.T, inputs map[string]any, caps transcode.WorkerCapabilities) *budgetCase {
	t.Helper()
	in := map[string]any{"mode": "size"}
	for k, v := range inputs {
		in[k] = v
	}
	// legacy_comparison freezes the budget the way snapshots made before the
	// VideoToolbox ladder were: the encoder comparison in place of the ladder.
	_, legacy := in["legacy_comparison"]
	delete(in, "legacy_comparison")
	sha := strings.Repeat("a", 64)
	ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: "size-budget", Inputs: in, Outputs: map[string]any{}, State: map[string]any{"original_sha256": sha, "original_size": budgetSourceBytes, "source_report": budgetReport(), "size_main10": true, "optimization_enabled": true, "benchmark_job_id": "bench-1"}}
	snapshot, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		snapshot = legacyComparisonSnapshot(t, ec, snapshot)
	}
	qc := ensureBenchmarkQualityMetric(buildBenchmarkQualityConfig(snapshot.Profile.Optimization.Quality), "vmaf")
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 24, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: snapshot.Profile.Audio.Mode}
	base.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", Quality: *qc, BenchmarkRequestDigest: "request"}
	if err := applyModePlan(base, snapshot); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = base
	request := transcode.BenchmarkRequest{ID: "bench-1", Mode: "size", SourceSHA256: sha, SourceDuration: 6000, Metric: "vmaf", Quality: qc,
		Samples:    []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 1200, DurationSeconds: 10}, {Index: 1, StartSeconds: 3000, DurationSeconds: 10}},
		Candidates: []transcode.BenchmarkCandidate{{ID: "x265-q22", VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le"}}}
	digest, err := transcode.DigestBenchmarkRequest(&request)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["benchmark_request"] = request
	ec.State["benchmark_request_digest"] = digest
	executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return caps, nil }}
	return &budgetCase{ec: ec, engine: NewEngine(EngineDeps{Transcode: executor}), executor: executor, snapshot: snapshot, base: base, request: request}
}

// legacyComparisonSnapshot rewrites a frozen snapshot into the shape snapshots
// carried before the ladder: the encoder comparison and no ladder, under its
// own digest.
func legacyComparisonSnapshot(t *testing.T, ec *ExecutionContext, snapshot *ModePolicySnapshot) *ModePolicySnapshot {
	t.Helper()
	if snapshot.Size == nil || snapshot.Size.SizeBudget == nil || snapshot.Size.Hardware == nil {
		return snapshot
	}
	p := *snapshot
	size := *p.Size
	budget := size.SizeBudget.Clone()
	rules := sizeBudgetComparisonRules
	budget.VTLadder, budget.Comparison = nil, &rules
	size.SizeBudget, p.Size = budget, &size
	p.Digest = ""
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	ec.State["mode_policy"], ec.State["policy_digest"] = p, p.Digest
	return &p
}

func budgetDecision(winner *transcode.BenchmarkWinner) *transcode.BenchmarkDecision {
	return &transcode.BenchmarkDecision{Winner: winner, Evaluations: []transcode.BenchmarkCandidateEvaluation{
		{CandidateID: "x265-q22", VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", EstimatedBytes: 6_100_000_000, Eligible: winner != nil, VMAF: &transcode.BenchmarkVMAFSummary{Mean: 90.1, P5: 85.2, WorstWindowMean: 86.0}},
		{CandidateID: "x265-q26", VideoCodec: transcode.VideoCodecLibX265, Quality: 26, Preset: "medium", EstimatedBytes: 3_900_000_000, VMAF: &transcode.BenchmarkVMAFSummary{Mean: 86.4, P5: 79.0, WorstWindowMean: 80.2}},
		{CandidateID: "x265-q30", VideoCodec: transcode.VideoCodecLibX265, Quality: 30, Preset: "medium"},
	}}
}

func TestModeSizeBudgetInput(t *testing.T) {
	b, err := modeSizeBudgetInput("size", map[string]any{"size_budget_percent": 40.0})
	if err != nil || b == nil || b.Percent != 40 || b.Bytes != 0 || b.X265.AQMode != 3 || !b.X265.TwoPass || b.Preset != "medium" || b.Floor != sizeBudgetFloor || b.FillPercent != 97 {
		t.Fatalf("percent budget %+v %v", b, err)
	}
	if b, err := modeSizeBudgetInput("size", map[string]any{"size_budget_bytes": json.Number("2000000000")}); err != nil || b.Bytes != 2_000_000_000 || b.Percent != 0 {
		t.Fatalf("byte budget %+v %v", b, err)
	}
	if b, err := modeSizeBudgetInput("size", map[string]any{"size_budget_percent": nil}); err != nil || b != nil {
		t.Fatalf("absent budget %+v %v", b, err)
	}
	for name, in := range map[string]map[string]any{
		"both":           {"size_budget_percent": 40.0, "size_budget_bytes": 2_000_000_000.0},
		"too small":      {"size_budget_percent": 4.0},
		"no saving":      {"size_budget_percent": 86.0},
		"fractional":     {"size_budget_bytes": 2_000_000_000.5},
		"megabytes typo": {"size_budget_bytes": 2000.0},
		"not a number":   {"size_budget_percent": "40"},
		"negative":       {"size_budget_bytes": -1.0},
	} {
		if _, err := modeSizeBudgetInput("size", in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := modeSizeBudgetInput("quality", map[string]any{"size_budget_percent": 40.0}); err == nil {
		t.Fatal("budget accepted outside size mode")
	}
	if err := validateModeInputs(map[string]any{"mode": "compact", "size_budget_bytes": 2_000_000_000.0}); err == nil {
		t.Fatal("compact accepted a budget")
	}
	if err := validateModeInputs(map[string]any{"size_budget_percent": 40.0}); err == nil {
		t.Fatal("budget accepted without a mode")
	}
	if err := ValidateHardwareInputs("size", map[string]any{"size_budget_percent": 90.0}); err == nil {
		t.Fatal("goal change accepted a budget with no saving")
	}
}

func TestSizeBudgetPolicyDigest(t *testing.T) {
	plain, err := modePolicyWithOptions("size", false, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	none, err := modePolicyWithBudget("size", false, 0, nil, false, nil)
	if err != nil || none.Digest != plain.Digest {
		t.Fatalf("no budget changed the digest: %s vs %s (%v)", none.Digest, plain.Digest, err)
	}
	raw, _ := json.Marshal(plain)
	if strings.Contains(string(raw), "size_budget") {
		t.Fatal("snapshot without a budget serializes one")
	}
	budget, _ := modeSizeBudgetInput("size", map[string]any{"size_budget_percent": 40.0})
	with, err := modePolicyWithBudget("size", false, 0, nil, false, budget)
	if err != nil || with.Digest == plain.Digest || with.Size.SizeBudget == nil {
		t.Fatalf("budget not frozen: %+v %v", with.Size, err)
	}
	// The frozen budget, its encode settings and floor are part of the identity.
	const pinned = "sha256:acab2deaadb3e3deb498dda0ff3080a5a8f86844531d9bc79153679cfdd6839b"
	if with.Digest != pinned {
		t.Fatalf("size budget policy digest changed: %s", with.Digest)
	}
	// A frozen budget is re-verified like the rest of the snapshot.
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "size", "size_budget_percent": 40.0}, State: map[string]any{"mode_policy": with}}
	if p, err := freezeModePolicy(ec); err != nil || p.Size.SizeBudget.Percent != 40 {
		t.Fatalf("frozen budget unreadable %+v %v", p, err)
	}
	tampered := with
	size := *with.Size
	b := *size.SizeBudget
	b.Floor.VMAFMean = 50
	size.SizeBudget = &b
	tampered.Size = &size
	ec.State["mode_policy"] = tampered
	if _, err := freezeModePolicy(ec); err == nil {
		t.Fatal("tampered floor accepted")
	}
	if _, err := modePolicyWithBudget("quality", false, 0, nil, false, budget); err == nil {
		t.Fatal("quality mode froze a budget")
	}
}

func TestSizeBudgetBytesAndBitrate(t *testing.T) {
	percent := &SizeBudgetPolicy{Percent: 40}
	if got := percent.BudgetBytes(budgetSourceBytes, nil); got != 4_000_000_000 {
		t.Fatalf("percent budget %d", got)
	}
	// An absolute budget never allows less than the size mode's 15% saving.
	if got := (&SizeBudgetPolicy{Bytes: 9_500_000_000}).BudgetBytes(budgetSourceBytes, nil); got != 8_500_000_000 {
		t.Fatalf("absolute budget not capped: %d", got)
	}
	nonVideo, known := sizeBudgetNonVideoBytes(budgetReport(), budgetSourceBytes)
	if !known || nonVideo != 300_000_000 {
		t.Fatalf("non-video bytes %d %v", nonVideo, known)
	}
	// (3.88 GB - 300 MB) * 8 / 6000 s
	if kbps := sizeBudgetVideoKbps(3_880_000_000, nonVideo, 6000); kbps != 4773 {
		t.Fatalf("bitrate %d", kbps)
	}
	if kbps := sizeBudgetVideoKbps(200_000_000, nonVideo, 6000); kbps != 0 {
		t.Fatalf("unreachable budget gave %d kbps", kbps)
	}
	// Lossless audio that will be compacted leaves more room for the video.
	rep := audioOnlyReport()
	rep.Video[0].BitRate, rep.Video[0].Tags = 0, map[string]string{"NUMBER_OF_BYTES": "4000000000"}
	lossless, known := sizeBudgetNonVideoBytes(rep, 10_700_000_000)
	if !known || lossless >= 10_700_000_000-4_000_000_000 {
		t.Fatalf("lossless audio savings ignored: %d", lossless)
	}
	// Without the video's statistics the output's other streams are sized
	// directly: converted lossless audio at its compact bitrate (even when the
	// source track states no size, as FLAC in Matroska often does), copied
	// audio at its own size.
	rep.Video[0].Tags = nil
	_, kbps := transcode.AudioTarget(transcode.AudioModeCompactLossless, transcode.AudioTrack{Codec: "dts", Profile: "DTS-HD MA", Channels: 6, Layout: "5.1(side)"})
	want := 10_700_000_000*sizeBudgetOverheadPerMille/1000 + 2*int64(kbps)*1000*6000/8 + 192000*6000/8
	if derived, known := sizeBudgetNonVideoBytes(rep, 10_700_000_000); !known || derived != want {
		t.Fatalf("other streams %d %v, want %d", derived, known, want)
	}
	rep.Audio[1].BitRate = 0
	if derived, known := sizeBudgetNonVideoBytes(rep, 10_700_000_000); !known || derived != want {
		t.Fatalf("unsized lossless track: %d %v, want %d", derived, known, want)
	}
	// A copied track that states no size leaves the video size unknown.
	rep.Audio[2].BitRate = 0
	if _, known := sizeBudgetNonVideoBytes(rep, 10_700_000_000); known {
		t.Fatal("unknown video size treated as known")
	}
}

func TestSizeBudgetFallbackWithoutWinner(t *testing.T) {
	c := newBudgetCase(t, map[string]any{"size_budget_percent": 40.0}, budgetCaps())
	outputs := map[string]any{}
	result, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, outputs, budgetDecision(nil), nil)
	if !handled || result.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("fallback %+v handled=%v state=%+v", result, handled, c.ec.State)
	}
	plan := getPlan(c.ec.State["plan"])
	want := sizeBudgetX265
	want.BitrateKbps = 4773
	if plan == nil || plan.X265 == nil || *plan.X265 != want || plan.VideoCodec != transcode.VideoCodecLibX265 || plan.Quality != 0 || plan.Preset != "medium" || plan.VideoProfile != "main10" || plan.ExpectedBitDepth != 10 || plan.PolicyDigest != c.base.PolicyDigest || plan.SizePolicy.MinSavingsPercent != 15 {
		t.Fatalf("budget plan %+v x265 %+v", plan, plan.X265)
	}
	if plan.X265.Validate(plan.VideoCodec, plan.Quality) != nil {
		t.Fatal("plan's x265 settings are out of bounds")
	}
	if d, err := transcode.DigestPlan(withoutDigest(plan)); err != nil || d != plan.PlanDigest || c.ec.State["plan_digest"] != plan.PlanDigest || c.ec.State["plan_source"] != sizeBudgetPlanSource {
		t.Fatalf("plan identity %s state %v", plan.PlanDigest, c.ec.State["plan_digest"])
	}
	q := plan.QualityValidation
	windows, _ := sizeFinalWindowsWithFresh(c.ec, 6000, 0)
	if q == nil || q.Metric != "vmaf" || len(q.Samples) != len(windows) || len(windows) == 0 || q.BenchmarkRequestDigest != c.ec.State["benchmark_request_digest"] {
		t.Fatalf("final validation %+v", q)
	}
	v := q.Quality.VMAF
	if v.Minimum != 70 || *v.P5Minimum != 60 || *v.WorstWindowMinimum != 65 || v.WorstWindowSeconds != 5 || v.GuardrailEnforcement != "reject" || *q.Quality.Banding.MaxMean != 6 || *q.Quality.Banding.MaxPeak != 16 {
		t.Fatalf("floor %+v banding %+v", v, q.Quality.Banding)
	}
	// The frozen policy's own thresholds are untouched.
	if c.snapshot.Profile.Optimization.Quality.VMAF.Minimum != 88 {
		t.Fatal("floor leaked into the frozen policy")
	}
	rec := sizeBudgetOf(c.ec.State)
	if rec == nil || rec.Trigger != sizeBudgetTriggerNoWinner || rec.BudgetBytes != 4_000_000_000 || rec.TargetBytes != 3_880_000_000 || rec.NonVideoBytes != 300_000_000 || rec.VideoBitrateKbps != 4773 || *rec.Target.VMAFMean != 88 || *rec.Floor.VMAFMean != 70 || *rec.Floor.CAMBIPeak != 16 || rec.Message == "" {
		t.Fatalf("record %+v", rec)
	}
	if len(rec.Curve) != 3 || rec.Curve[0].CandidateID != "x265-q26" || !rec.Curve[0].WithinBudget || rec.Curve[1].WithinBudget || rec.Curve[2].Kind != "budget_fallback" || rec.Curve[2].Bytes != 3_880_000_000 || rec.Curve[2].Measured {
		t.Fatalf("curve %+v", rec.Curve)
	}
	if outputs["plan_source"] != sizeBudgetPlanSource || outputs[sizeBudgetKey] == nil {
		t.Fatalf("outputs %+v", outputs)
	}
}

func withoutDigest(p *transcode.Plan) *transcode.Plan {
	cp := *p
	cp.PlanDigest = ""
	return &cp
}

func TestSizeBudgetFallbackDeclines(t *testing.T) {
	old := modeTestCapabilities()
	for name, tc := range map[string]struct {
		inputs map[string]any
		caps   transcode.WorkerCapabilities
		mutate func(*budgetCase)
		reason string // recorded beside the search's outcome
	}{
		"no budget":  {caps: budgetCaps()},
		"old worker": {inputs: map[string]any{"size_budget_percent": 40.0}, caps: old, reason: "size_budget_worker_unsupported"},
		"no video bytes": {inputs: map[string]any{"size_budget_percent": 40.0}, caps: budgetCaps(), reason: "size_budget_estimate_unavailable", mutate: func(c *budgetCase) {
			rep := budgetReport()
			rep.Video[0].Tags, rep.Audio[0].BitRate = nil, 0
			c.ec.State["source_report"] = rep
		}},
		"audio alone takes the budget": {inputs: map[string]any{"size_budget_bytes": 300_000_000.0}, caps: budgetCaps(), reason: "size_budget_unreachable"},
		"audio-only compaction fits": {inputs: map[string]any{"size_budget_percent": 60.0}, caps: budgetCaps(), mutate: func(c *budgetCase) {
			rep := audioOnlyReport()
			rep.Video[0].BitRate, rep.Video[0].Tags = 0, map[string]string{"NUMBER_OF_BYTES": "4500000000"}
			c.ec.State["source_report"], c.ec.State["original_size"] = rep, int64(10_700_000_000)
		}},
		"quality mode": {caps: budgetCaps(), mutate: func(c *budgetCase) { c.ec.Inputs["mode"] = "quality" }},
	} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCase(t, tc.inputs, tc.caps)
			if tc.mutate != nil {
				tc.mutate(c)
			}
			result, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil)
			// A budget that cannot be planned never ends the action: the
			// search's outcome (winner, audio-only, no candidate) stands.
			if handled {
				t.Fatalf("handled (%+v)", result)
			}
			if getPlan(c.ec.State["plan"]) != c.base || c.ec.State[sizeBudgetKey] != nil || c.ec.State["skip_transcode"] != nil || c.ec.State["reason_code"] != nil {
				t.Fatal("declined fallback changed the outcome")
			}
			if getString(c.ec.State, sizeBudgetSkipReasonKey) != tc.reason || (tc.reason != "") != (getString(c.ec.State, sizeBudgetSkipMessageKey) != "") {
				t.Fatalf("recorded reason %q want %q", getString(c.ec.State, sizeBudgetSkipReasonKey), tc.reason)
			}
		})
	}
	// A capable worker is checked only once the encode can be planned.
	c := newBudgetCase(t, map[string]any{"size_budget_percent": 40.0}, budgetCaps())
	c.executor.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, os.ErrDeadlineExceeded
	}
	if result, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); !handled || result.Status == StepCompleted {
		t.Fatalf("capability failure %+v", result)
	}
}

// budgetWinner is a passing setting whose sampled estimate is 6.1 GB.
func budgetWinner() *transcode.BenchmarkWinner {
	return &transcode.BenchmarkWinner{CandidateID: "x265-q22", VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le", MetricType: "vmaf", Score: 90.1, MinimumMet: true, EstimatedTotalBytes: 6_100_000_000, EstimatedTotalMB: 6100, SavingsPercent: 39}
}

func TestSizeBudgetBenchmarkWait(t *testing.T) {
	run := func(t *testing.T, inputs map[string]any, winner *transcode.BenchmarkWinner) (*budgetCase, StepResult) {
		t.Helper()
		c := newBudgetCase(t, inputs, budgetCaps())
		c.executor.benchmarkStatusFunc = func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusCompleted, Decision: budgetDecision(winner)}, nil
		}
		result, err := c.engine.stepBenchmarkWait(context.Background(), c.ec)
		if err != nil {
			t.Fatal(err)
		}
		return c, result
	}
	t.Run("passing winner over budget", func(t *testing.T) {
		c, result := run(t, map[string]any{"size_budget_percent": 40.0}, budgetWinner())
		rec := sizeBudgetOf(c.ec.State)
		if result.Status != StepCompleted || rec == nil || rec.Trigger != sizeBudgetTriggerOverBudget || rec.OverBudgetCandidateID != "x265-q22" || rec.OverBudgetEstimatedBytes != 6_100_000_000 {
			t.Fatalf("result %+v record %+v", result, rec)
		}
		if c.ec.State["benchmark_winner"] != nil || getBool(c.ec.State, "has_winner") || c.ec.State["plan_source"] != sizeBudgetPlanSource || getPlan(c.ec.State["plan"]).X265 == nil {
			t.Fatalf("over-budget winner still selected: %+v", c.ec.State)
		}
	})
	t.Run("passing winner within budget", func(t *testing.T) {
		c, result := run(t, map[string]any{"size_budget_percent": 70.0}, budgetWinner())
		if result.Status != StepCompleted || c.ec.State[sizeBudgetKey] != nil || c.ec.State["plan_source"] != "benchmark_winner" || getPlan(c.ec.State["plan"]).Quality != 22 {
			t.Fatalf("within-budget winner changed: %+v %+v", result, c.ec.State)
		}
	})
	t.Run("no budget keeps the winner", func(t *testing.T) {
		c, _ := run(t, nil, budgetWinner())
		if c.ec.State[sizeBudgetKey] != nil || c.ec.State["plan_source"] != "benchmark_winner" {
			t.Fatalf("winner replaced without a budget: %+v", c.ec.State)
		}
	})
	t.Run("no passing setting", func(t *testing.T) {
		c, result := run(t, map[string]any{"size_budget_percent": 40.0}, nil)
		if result.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") || c.ec.State["plan_source"] != sizeBudgetPlanSource {
			t.Fatalf("no-winner fallback %+v %+v", result, c.ec.State)
		}
	})
	t.Run("over budget and no budget encode can be planned keeps the winner", func(t *testing.T) {
		c := newBudgetCase(t, map[string]any{"size_budget_percent": 40.0}, budgetCaps())
		rep := budgetReport()
		rep.Video[0].Tags, rep.Audio[0].BitRate = nil, 0
		c.ec.State["source_report"] = rep
		c.executor.benchmarkStatusFunc = func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusCompleted, Decision: budgetDecision(budgetWinner())}, nil
		}
		result, err := c.engine.stepBenchmarkWait(context.Background(), c.ec)
		if err != nil || result.Status != StepCompleted || c.ec.State["plan_source"] != "benchmark_winner" || getPlan(c.ec.State["plan"]).Quality != 22 || getBool(c.ec.State, "skip_transcode") {
			t.Fatalf("passing winner dropped: %+v %+v", result, c.ec.State)
		}
		if getString(c.ec.State, sizeBudgetSkipReasonKey) != "size_budget_estimate_unavailable" || !strings.Contains(getString(c.ec.State, sizeBudgetSkipMessageKey), "best passing setting is kept") {
			t.Fatalf("why the budget was not applied is not recorded: %+v", c.ec.State)
		}
		c.ec.Outputs["original_intact"] = true
		explanation := ExplainTranscode(&ActionResult{ActionName: "transcode_media", Status: StatusCompleted, Inputs: c.ec.Inputs, State: c.ec.State, Outputs: c.ec.Outputs})
		if explanation.Outcome != "candidate_ready" || !strings.Contains(explanation.Message, "best passing setting is kept") {
			t.Fatalf("explanation %+v", explanation)
		}
	})
	t.Run("a winner without an estimate is kept", func(t *testing.T) {
		w := budgetWinner()
		w.EstimatedTotalBytes, w.EstimatedTotalMB = 0, 0
		c, _ := run(t, map[string]any{"size_budget_percent": 40.0}, w)
		if c.ec.State[sizeBudgetKey] != nil || c.ec.State["plan_source"] != "benchmark_winner" {
			t.Fatalf("winner without an estimate replaced: %+v", c.ec.State)
		}
	})
	t.Run("no passing setting without a budget", func(t *testing.T) {
		c, _ := run(t, nil, nil)
		if getString(c.ec.State, "reason_code") != "no_suitable_candidate" || c.ec.State[sizeBudgetKey] != nil {
			t.Fatalf("no-winner outcome changed without a budget: %+v", c.ec.State)
		}
	})
}

func TestSizeBudgetValidation(t *testing.T) {
	c := newBudgetCase(t, map[string]any{"size_budget_percent": 40.0}, budgetCaps())
	if _, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); !handled {
		t.Fatal("fallback not taken")
	}
	plan := getPlan(c.ec.State["plan"])
	evidence := func(mean, p5, worst float64) map[string]any {
		return map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest, "benchmark_request_digest": plan.QualityValidation.BenchmarkRequestDigest,
			"vmaf": map[string]any{"mean": mean, "p5": p5, "worst_window_mean": worst, "min": 50.0}, "cambi": map[string]any{"mean": 2.0, "max": 9.0}}
	}
	validate := func(t *testing.T, size int64, ev map[string]any, mutate func(map[string]any)) (StepResult, map[string]any) {
		t.Helper()
		candidate := t.TempDir() + "/candidate.mkv"
		// A sparse file has the candidate's size without its bytes.
		if err := os.WriteFile(candidate, nil, 0600); err != nil || os.Truncate(candidate, size) != nil {
			t.Fatal(err)
		}
		state := map[string]any{}
		for k, v := range c.ec.State {
			state[k] = v
		}
		state["candidate_path"], state["candidate_sha256"], state["quality_evidence"], state["candidate_size_bytes"] = candidate, "candidate", ev, size
		state["original"] = map[string]any{"size_bytes": budgetSourceBytes, "video_codec": "h264"}
		if mutate != nil {
			mutate(state)
		}
		ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: c.ec.InstanceID, Inputs: c.ec.Inputs, State: state}
		result, err := (&Engine{}).stepTranscodeValidate(context.Background(), ec)
		if err != nil {
			t.Fatal(err)
		}
		return result, state
	}
	t.Run("below target within budget", func(t *testing.T) {
		result, state := validate(t, 3_850_000_000, evidence(84.2, 74.0, 76.5), nil)
		rec := sizeBudgetOf(state)
		if result.Status != StepCompleted || getBool(state, "skip_transcode") || rec == nil || rec.Outcome != SizeBudgetBelowTarget || len(rec.BelowTarget) != 3 || rec.CandidateBytes != 3_850_000_000 {
			t.Fatalf("result %+v record %+v", result, rec)
		}
		last := rec.Curve[len(rec.Curve)-1]
		if last.Kind != "budget_fallback" || !last.Measured || !last.WithinBudget || last.Passed || *last.VMAFMean != 84.2 || last.Bytes != 3_850_000_000 {
			t.Fatalf("measured point %+v", last)
		}
		if !strings.Contains(rec.Message, "Below the quality target") || result.Outputs["result"].(map[string]any)["size_budget_outcome"] != SizeBudgetBelowTarget {
			t.Fatalf("message %q outputs %+v", rec.Message, result.Outputs["result"])
		}
	})
	t.Run("meets target within budget", func(t *testing.T) {
		_, state := validate(t, 3_850_000_000, evidence(91.0, 86.0, 87.0), nil)
		if rec := sizeBudgetOf(state); rec.Outcome != SizeBudgetMeetsTarget || len(rec.BelowTarget) != 0 {
			t.Fatalf("record %+v", rec)
		}
	})
	t.Run("over budget", func(t *testing.T) {
		// Over the budget, and also past the 15% minimum saving: both are
		// reported as over the budget, with the measured size.
		for _, size := range []int64{4_100_000_000, 9_000_000_000} {
			result, state := validate(t, size, evidence(84.2, 74.0, 76.5), nil)
			if result.Status != StepCompleted || !getBool(state, "skip_transcode") || state["reason_code"] != SizeBudgetExceeded || sizeBudgetOf(state).Outcome != SizeBudgetExceeded || sizeBudgetOf(state).CandidateBytes != size {
				t.Fatalf("%d byte candidate %+v %+v", size, result, state)
			}
		}
	})
	t.Run("encode settings changed after planning", func(t *testing.T) {
		for name, change := range map[string]func(*transcode.Plan){
			"aq mode":   func(p *transcode.Plan) { p.X265.AQMode = 1 },
			"lookahead": func(p *transcode.Plan) { p.X265.Lookahead = 10 },
			"preset":    func(p *transcode.Plan) { p.Preset = "ultrafast" },
		} {
			result, _ := validate(t, 3_850_000_000, evidence(84.2, 74.0, 76.5), func(state map[string]any) {
				p := *plan
				p.X265 = plan.X265.Clone()
				change(&p)
				state["plan"] = &p
			})
			if result.Status != StepFailed {
				t.Fatalf("%s: changed plan accepted: %+v", name, result)
			}
		}
	})
	t.Run("record changed after planning", func(t *testing.T) {
		result, _ := validate(t, 3_850_000_000, evidence(84.2, 74.0, 76.5), func(state map[string]any) {
			rec := *sizeBudgetOf(state)
			rec.TargetBytes += 1_000_000
			state[sizeBudgetKey] = &rec
		})
		if result.Status != StepFailed {
			t.Fatalf("changed record accepted: %+v", result)
		}
	})
	t.Run("policy limits on a budget plan", func(t *testing.T) {
		result, _ := validate(t, 3_850_000_000, evidence(84.2, 74.0, 76.5), func(state map[string]any) {
			p := *plan
			q := *p.QualityValidation
			q.Quality = *ensureBenchmarkQualityMetric(buildBenchmarkQualityConfig(c.snapshot.Profile.Optimization.Quality), "vmaf")
			p.QualityValidation = &q
			state["plan"] = &p
		})
		if result.Status != StepFailed {
			t.Fatalf("budget plan without the floor passed: %+v", result)
		}
	})
	t.Run("floor without the budget fallback", func(t *testing.T) {
		result, _ := validate(t, 3_850_000_000, evidence(84.2, 74.0, 76.5), func(state map[string]any) { state["plan_source"] = "benchmark_winner" })
		if result.Status != StepFailed {
			t.Fatalf("floor accepted for a normal winner: %+v", result)
		}
	})
	t.Run("bitrate changed after planning", func(t *testing.T) {
		result, _ := validate(t, 3_850_000_000, evidence(84.2, 74.0, 76.5), func(state map[string]any) {
			p := *plan
			p.X265 = plan.X265.Clone()
			p.X265.BitrateKbps++
			state["plan"] = &p
		})
		if result.Status != StepFailed {
			t.Fatalf("other bitrate accepted: %+v", result)
		}
	})
}

// A budget roomier than two-pass x265 can use is met at its bitrate ceiling
// instead of being refused.
func TestSizeBudgetBitrateCeiling(t *testing.T) {
	c := newBudgetCase(t, map[string]any{"size_budget_percent": 85.0}, budgetCaps())
	rep := budgetReport()
	rep.Video[0].Tags = map[string]string{"NUMBER_OF_BYTES": "59000000000"}
	enc := planSizeBudgetEncode(c.snapshot.Size.SizeBudget, rep, 60_000_000_000, 51_000_000_000)
	if enc.Reason != "" || enc.X265.BitrateKbps != transcode.X265MaxBitrateKbps {
		t.Fatalf("encode %+v", enc)
	}
	if enc := planSizeBudgetEncode(c.snapshot.Size.SizeBudget, rep, 60_000_000_000, 1_000_000_000); enc.Reason != "size_budget_unreachable" {
		t.Fatalf("budget under the other streams planned: %+v", enc)
	}
}

func TestOvershotSizeBudget(t *testing.T) {
	c := newBudgetCase(t, map[string]any{"size_budget_percent": 40.0}, budgetCaps())
	if _, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); !handled {
		t.Fatal("fallback not taken")
	}
	if rec := overshotSizeBudget(c.ec, 0); rec == nil || rec.Outcome != SizeBudgetExceeded || !strings.Contains(rec.Message, "over the") {
		t.Fatalf("record without a size %+v", rec)
	}
	if rec := overshotSizeBudget(c.ec, 9_000_000_000); rec.Outcome != SizeBudgetExceeded || rec.CandidateBytes != 9_000_000_000 {
		t.Fatalf("record with a size %+v", rec)
	}
}

func TestSizeBudgetBelowFloorAndExplanation(t *testing.T) {
	c := newBudgetCase(t, map[string]any{"size_budget_percent": 40.0}, budgetCaps())
	if _, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); !handled {
		t.Fatal("fallback not taken")
	}
	ev := &transcode.FinalQualityEvidence{Verdict: "fail", ReasonCodes: []string{"quality_vmaf_p5_below_minimum"}, CandidateSizeBytes: 3_860_000_000}
	rec := failSizeBudget(c.ec, ev)
	if rec == nil || rec.Outcome != SizeBudgetBelowFloor || rec.Final == nil || *rec.Final.Limits.VMAFP5 != 60 || !strings.Contains(rec.Message, "below the quality floor") {
		t.Fatalf("below-floor record %+v", rec)
	}
	c.ec.State["reason_code"] = "quality_not_met"
	explanation := ExplainTranscode(&ActionResult{ActionName: "transcode_media", Status: StatusCompleted, Inputs: c.ec.Inputs, State: c.ec.State, Outputs: map[string]any{}})
	if explanation.Outcome != "quality_rejected" || explanation.SizeBudget == nil || explanation.Message != rec.Message || explanation.Encoder != transcode.VideoCodecLibX265 {
		t.Fatalf("explanation %+v", explanation)
	}
}

func TestResolutionClass(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want string
	}{{1920, 800, "1080p"}, {1920, 1080, "1080p"}, {1440, 1080, "1080p"}, {1280, 720, "720p"}, {1280, 536, "720p"}, {720, 480, "sd"}, {3840, 1600, "2160p"}, {2560, 1440, "1440p"}, {2560, 1080, "1440p"}, {2048, 858, "1080p"}, {0, 0, ""}} {
		if got := ResolutionClass(tc.w, tc.h); got != tc.want {
			t.Errorf("%dx%d = %q, want %q", tc.w, tc.h, got, tc.want)
		}
	}
}

// siblingProbe is an ffprobe stand-in: 1500 s files, 1920x800 unless the file
// name says 720 (1280x720) or short (a 60 s extra). Only the file name is
// matched: the directories above it hold the test's random temp dir name, which
// contains "720" often enough to turn every sibling into a 720p file.
func siblingProbe(t *testing.T) string {
	t.Helper()
	probe := t.TempDir() + "/ffprobe"
	script := `#!/bin/sh
for p; do :; done
p=${p##*/}
w=1920; h=800; d=1500
case "$p" in *720*) w=1280; h=720;; esac
case "$p" in *short*) d=60;; esac
cat <<JSON
{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p10le","width":$w,"height":$h,"avg_frame_rate":"24/1","r_frame_rate":"24/1"}],"format":{"format_name":"matroska","duration":"$d"},"chapters":[]}
JSON
`
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return probe
}

func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil || os.Truncate(path, size) != nil {
		t.Fatal(err)
	}
}

func TestSizeBudgetSiblings(t *testing.T) {
	rules := &SiblingBudgetRules{Multiplier: 1.5, MinimumSiblings: 3, MaxProbed: 60}
	source := func(root string) (string, *mediainspect.DetailedReport) {
		return root + "/Show/Season 01/e01.mkv", &mediainspect.DetailedReport{DurationSec: 1500, Video: []mediainspect.DetailedStream{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}}}
	}
	engine := func(t *testing.T, root string) *Engine {
		res, err := fsop.NewResolver([]string{root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return NewEngine(EngineDeps{Fs: res, Ffprobe: siblingProbe(t)})
	}
	t.Run("season", func(t *testing.T) {
		root := t.TempDir()
		src, rep := source(root)
		sparse(t, src, 2_000_000_000)
		season := filepath.Dir(src)
		for name, size := range map[string]int64{"e02.mkv": 300_000_000, "e03.mkv": 400_000_000, "e04.mkv": 500_000_000, "e05-720.mkv": 100_000_000, "e06-short.mkv": 10_000_000, ".navigatorr-candidates/e02.job.mkv": 50_000_000, ".hidden.mkv": 1_000_000, "notes.txt": 1_000} {
			sparse(t, season+"/"+name, size)
		}
		ev := engine(t, root).sizeBudgetSiblings(context.Background(), src, rep, 2_000_000_000, rules)
		if ev.Reason != "" || ev.Scope != "season" || ev.Count != 3 || ev.ResolutionClass != "1080p" || ev.MedianBitrateKbps != 2133 || ev.BudgetBytes != 600_000_000 || ev.Ratio != 5 || len(ev.Files) != 3 {
			t.Fatalf("season evidence %+v", ev)
		}
		for _, f := range ev.Files {
			if strings.Contains(f.Name, "/") || f.Name == "e01.mkv" {
				t.Fatalf("listed %q", f.Name)
			}
		}
		policy := &SizeBudgetPolicy{Siblings: rules}
		if got := policy.BudgetBytes(2_000_000_000, ev); got != 600_000_000 {
			t.Fatalf("budget %d", got)
		}
		// A budget the siblings put above the minimum saving is capped there.
		if got := policy.BudgetBytes(650_000_000, ev); got != 552_500_000 {
			t.Fatalf("capped budget %d", got)
		}
		if got := (&SizeBudgetPolicy{Siblings: &SiblingBudgetRules{Multiplier: 2, MinimumSiblings: 3, MaxProbed: 60}}).BudgetBytes(2_000_000_000, ev); got != 0 {
			t.Fatalf("evidence for another multiplier used: %d", got)
		}
	})
	t.Run("series", func(t *testing.T) {
		root := t.TempDir()
		src, rep := source(root)
		sparse(t, src, 2_000_000_000)
		sparse(t, filepath.Dir(src)+"/e02.mkv", 300_000_000)
		for i, size := range []int64{200_000_000, 250_000_000, 900_000_000} {
			sparse(t, root+"/Show/Season 02/e0"+string(rune('1'+i))+".mkv", size)
		}
		ev := engine(t, root).sizeBudgetSiblings(context.Background(), src, rep, 2_000_000_000, rules)
		// The series median takes the season sibling too: 200, 250, 300, 900 MB.
		if ev.Reason != "" || ev.Scope != "series" || ev.Count != 4 || ev.BudgetBytes != 412_500_000 {
			t.Fatalf("series evidence %+v", ev)
		}
	})
	t.Run("a folder that is not a season does not widen", func(t *testing.T) {
		// A movie in its own folder: the folder above is the whole library.
		root := t.TempDir()
		src := root + "/Movies/Film A/Film A.mkv"
		_, rep := source(root)
		sparse(t, src, 2_000_000_000)
		for _, name := range []string{"Film B/Film B.mkv", "Film C/Film C.mkv", "Film D/Film D.mkv"} {
			sparse(t, root+"/Movies/"+name, 300_000_000)
		}
		ev := engine(t, root).sizeBudgetSiblings(context.Background(), src, rep, 2_000_000_000, rules)
		if ev.Reason != "too_few_siblings" || ev.Scope != "season" || ev.Count != 0 {
			t.Fatalf("movie evidence %+v", ev)
		}
	})
	t.Run("an ffprobe that cannot run is reported, not counted as no siblings", func(t *testing.T) {
		root := t.TempDir()
		src, rep := source(root)
		sparse(t, src, 2_000_000_000)
		sparse(t, filepath.Dir(src)+"/e02.mkv", 300_000_000)
		res, err := fsop.NewResolver([]string{root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ev := NewEngine(EngineDeps{Fs: res, Ffprobe: root + "/missing-ffprobe"}).sizeBudgetSiblings(context.Background(), src, rep, 2_000_000_000, rules)
		if ev.Reason != "too_few_siblings" || ev.Count != 0 || ev.ProbeFailures != 1 || ev.ProbeFailure != "unavailable" || !strings.Contains(ev.Message, "ffprobe could not read 1 candidate file(s) (unavailable)") {
			t.Fatalf("probe failure evidence %+v", ev)
		}
		if strings.Contains(ev.Message, root) {
			t.Fatalf("message leaks a path: %q", ev.Message)
		}
	})
	t.Run("too few", func(t *testing.T) {
		root := t.TempDir()
		src, rep := source(root)
		sparse(t, src, 2_000_000_000)
		sparse(t, filepath.Dir(src)+"/e02.mkv", 300_000_000)
		sparse(t, filepath.Dir(src)+"/e03-720.mkv", 300_000_000)
		ev := engine(t, root).sizeBudgetSiblings(context.Background(), src, rep, 2_000_000_000, rules)
		if ev.Reason != "too_few_siblings" || ev.BudgetBytes != 0 || ev.Count != 1 || ev.Message == "" {
			t.Fatalf("too few evidence %+v", ev)
		}
		if got := (&SizeBudgetPolicy{Siblings: rules}).BudgetBytes(2_000_000_000, ev); got != 0 {
			t.Fatalf("budget from too few siblings: %d", got)
		}
	})
}

func TestSizeBudgetSiblingsInputAndFallback(t *testing.T) {
	b, err := modeSizeBudgetInput("size", map[string]any{"size_budget_source": "siblings"})
	if err != nil || b.Siblings == nil || b.Siblings.Multiplier != 1.5 || b.Percent != 0 || b.Bytes != 0 {
		t.Fatalf("sibling budget %+v %v", b, err)
	}
	if b, err := modeSizeBudgetInput("size", map[string]any{"size_budget_source": "siblings", "size_budget_sibling_multiplier": 2.0}); err != nil || b.Siblings.Multiplier != 2 {
		t.Fatalf("multiplier %+v %v", b, err)
	}
	for name, in := range map[string]map[string]any{
		"other source":         {"size_budget_source": "library"},
		"with a percent":       {"size_budget_source": "siblings", "size_budget_percent": 40.0},
		"multiplier alone":     {"size_budget_sibling_multiplier": 2.0},
		"multiplier too large": {"size_budget_source": "siblings", "size_budget_sibling_multiplier": 5.0},
		"multiplier below one": {"size_budget_source": "siblings", "size_budget_sibling_multiplier": 0.5},
	} {
		if _, err := modeSizeBudgetInput("size", in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Without a sibling budget the action goes on as if it had none; with one
	// the fallback encodes at it and records where it came from.
	c := newBudgetCase(t, map[string]any{"size_budget_source": "siblings"}, budgetCaps())
	c.ec.State[sizeBudgetSiblingsKey] = &SiblingBudgetEvidence{Reason: "too_few_siblings", Multiplier: 1.5, Message: "few"}
	if _, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); handled {
		t.Fatal("fallback without a sibling budget")
	}
	if !sizeBudgetWinnerFits(c.snapshot, budgetWinner(), budgetSourceBytes, siblingEvidenceOf(c.ec.State)) {
		t.Fatal("winner refused without a sibling budget")
	}
	c.ec.State[sizeBudgetSiblingsKey] = &SiblingBudgetEvidence{Scope: "season", Count: 5, ResolutionClass: "1080p", MedianBitrateKbps: 2000, Multiplier: 1.5, BudgetBytes: 2_250_000_000, Message: "from siblings"}
	if sizeBudgetWinnerFits(c.snapshot, budgetWinner(), budgetSourceBytes, siblingEvidenceOf(c.ec.State)) {
		t.Fatal("winner over the sibling budget fits")
	}
	if _, handled := c.engine.sizeBudgetFallback(context.Background(), c.ec, map[string]any{}, budgetDecision(nil), nil); !handled {
		t.Fatal("fallback not taken with a sibling budget")
	}
	if rec := sizeBudgetOf(c.ec.State); rec.BudgetBytes != 2_250_000_000 || rec.Siblings == nil || rec.Siblings.Count != 5 || !strings.Contains(rec.Message, "from siblings") {
		t.Fatalf("record %+v", rec)
	}
}

func TestSizeBudgetNonVideoBytesWithoutVideoStatistics(t *testing.T) {
	// A Bluray remux muxed without Matroska statistics: no stream states a
	// size, the AC-3 tracks state their bitrates and the subtitles are text.
	rep := &mediainspect.DetailedReport{DurationSec: 3600,
		Video:     []mediainspect.DetailedStream{{Index: 0, Codec: "hevc", BitDepth: 10, Width: 1920, Height: 1080, FPS: 25}},
		Audio:     []mediainspect.DetailedStream{{Index: 1, Codec: "ac3", Channels: 2, BitRate: 192000}, {Index: 2, Codec: "ac3", Channels: 6, BitRate: 384000}},
		Subtitles: []mediainspect.DetailedStream{{Index: 3, Codec: "subrip"}, {Index: 4, Codec: "subrip"}},
	}
	const source = int64(5_000_000_000)
	got, known := sizeBudgetNonVideoBytes(rep, source)
	audio := int64(192000+384000) * 3600 / 8
	want := audio + 2*optimization.DefaultTextSubtitleSizeBytes + source*sizeBudgetOverheadPerMille/1000
	if !known || got != want {
		t.Fatalf("non-video bytes = %d, %v; want %d derived from the other streams", got, known, want)
	}
	enc := planSizeBudgetEncode(&SizeBudgetPolicy{Percent: 20, FillPercent: sizeBudgetFillPercent, Preset: sizeBudgetPreset, X265: sizeBudgetX265, Floor: sizeBudgetFloor}, rep, source, source/5)
	if enc.Reason != "" || enc.X265.BitrateKbps <= 0 {
		t.Fatalf("budget not planned from the derived video size: %+v", enc)
	}

	for name, mutate := range map[string]func(*mediainspect.DetailedReport){
		"audio without a size": func(r *mediainspect.DetailedReport) { r.Audio[1].BitRate = 0 },
		"bitmap subtitles":     func(r *mediainspect.DetailedReport) { r.Subtitles[0].Codec = "hdmv_pgs_subtitle" },
		"others fill the file": func(r *mediainspect.DetailedReport) { r.Audio[0].BitRate = 20_000_000 },
	} {
		r := *rep
		r.Audio = append([]mediainspect.DetailedStream(nil), rep.Audio...)
		r.Subtitles = append([]mediainspect.DetailedStream(nil), rep.Subtitles...)
		mutate(&r)
		if _, known := sizeBudgetNonVideoBytes(&r, source); known {
			t.Fatalf("%s: a video size was derived from streams that do not bound it", name)
		}
	}
}
