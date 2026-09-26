package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestCalibratedBatchChildSkipsBenchmarkAndValidatesFinalCandidate(t *testing.T) {
	var submitted *transcode.Plan
	var candidatePath string
	mock := &mockTranscodeExecutor{
		submitFunc: func(_ context.Context, request transcode.Request) (transcode.Job, error) {
			submitted = request.Plan
			candidatePath = request.CandidatePath
			if err := os.MkdirAll(filepath.Dir(candidatePath), 0755); err != nil {
				return transcode.Job{}, err
			}
			if err := os.WriteFile(candidatePath, []byte("small"), 0644); err != nil {
				return transcode.Job{}, err
			}
			return transcode.Job{ID: request.ID}, nil
		},
		statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
			sum := sha256.Sum256([]byte("small"))
			sha := hex.EncodeToString(sum[:])
			return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, CandidatePath: candidatePath, CandidateSHA256: sha, CandidateSizeBytes: 5, QualityEvidence: &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: sha, CandidateSizeBytes: 5, PlanDigest: submitted.PlanDigest, BenchmarkRequestDigest: submitted.QualityValidation.BenchmarkRequestDigest}}, nil
		},
	}
	sourceJSON := strings.Replace(standard8BitProbeJSON, `"pix_fmt": "yuv420p"`, `"pix_fmt": "yuv420p", "r_frame_rate": "24/1", "avg_frame_rate": "24/1"`, 1)
	engine, st, mediaFile, _ := setupBenchmarkTestEnv(t, mock, sourceJSON, nil)
	defer st.Close()
	resolvedMediaFile, err := engine.Deps().Fs.ResolveRead(mediaFile)
	if err != nil {
		t.Fatal(err)
	}
	profile := calibratedTestProfileConfig()
	_, profileDigest, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": profile})
	if err != nil {
		t.Fatal(err)
	}
	calibration := BatchCalibrationResult{Profile: "anime-x265-calibrated", ProfileDigest: profileDigest, Quality: 22, ItemKeys: []string{"epfile-1"}, ActionIDs: []string{"representative-1"}, Digest: "sha256:calibration"}
	stateJSON := toJSON(map[string]any{"shared_calibration_result": calibration, "shared_calibration_profile_digest": profileDigest})
	if err := st.CreateActionInstance(store.ActionInstance{ID: "batch-parent-test", ActionName: "transcode_batch", Status: StatusWaitingExternal, InputsJSON: `{"profile":"anime-x265-calibrated"}`, StateJSON: stateJSON}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-parent-test", ItemKey: "epfile-1", FilePath: resolvedMediaFile, Decision: "transcode", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path": mediaFile, "profile_config": profile, "parent_action_id": "batch-parent-test", "batch_item_key": "epfile-1", "batch_fixed_quality": 22, "batch_calibration_digest": calibration.Digest,
		"replace_original": false, "min_savings_percent": 15.0,
	})
	if err != nil || result == nil {
		t.Fatalf("calibrated child run: result=%+v err=%v", result, err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("calibrated child status=%s error=%s", result.Status, result.Error)
	}
	if got := atomic.LoadInt32(&mock.benchmarkSubmitCalls); got != 0 {
		t.Fatalf("child repeated benchmark %d times", got)
	}
	if submitted == nil || submitted.Quality != 22 || submitted.QualityValidation == nil || len(submitted.QualityValidation.Samples) != 2 {
		t.Fatalf("full-file request lacks fixed quality and sampled validation: %+v", submitted)
	}
	if _, err := os.Stat(mediaFile); err != nil {
		t.Fatalf("original file was modified: %v", err)
	}
}

func TestCalibratedCandidateRequiresMatchingWorkerQualityEvidence(t *testing.T) {
	engine := NewEngine(EngineDeps{})
	ec := &ExecutionContext{State: map[string]any{
		"candidate_path":   "/candidate.mkv",
		"candidate_sha256": "sha-a",
		"plan":             &transcode.Plan{PlanDigest: "sha256:plan", QualityValidation: &transcode.QualityValidationPlan{BenchmarkRequestDigest: "sha256:policy"}},
	}}
	result, err := engine.stepTranscodeValidate(context.Background(), ec)
	if err != nil || result.Status != StepFailed || !strings.Contains(result.Error, "quality validation evidence") {
		t.Fatalf("missing quality evidence passed: result=%+v err=%v", result, err)
	}
	ec.State["quality_evidence"] = &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: "sha-b", PlanDigest: "sha256:plan", BenchmarkRequestDigest: "sha256:policy"}
	result, err = engine.stepTranscodeValidate(context.Background(), ec)
	if err != nil || result.Status != StepFailed {
		t.Fatalf("wrong candidate evidence passed: result=%+v err=%v", result, err)
	}
}

func calibratedTestProfileConfig() map[string]any {
	profile := testEphemeralBatchProfileConfig()
	profile["optimization"] = map[string]any{
		"enabled":  true,
		"sampling": map[string]any{"strategy": "distributed", "sample_count": 2, "sample_seconds": 10, "positions": []float64{0.25, 0.75}},
		"quality": map[string]any{
			"preferred_metric": "vmaf",
			"vmaf":             map[string]any{"model": "v1_1080p_3h", "target": 91, "minimum": 88, "marginal_tolerance": 2, "guardrail_enforcement": "reject", "p5_minimum": 84, "worst_window_minimum": 85, "worst_window_seconds": 5},
			"banding":          map[string]any{"enabled": true, "metric": "cambi", "mode": "full_ref", "enforcement": "reject", "max_mean": 6, "max_peak": 16},
			"final_validation": map[string]any{"mode": "sampled"},
		},
		"search": map[string]any{"quality_values": []int{20, 22}, "max_candidates": 2},
	}
	return profile
}

func TestSharedBatchCalibrationReusesCompletedRepresentativeActions(t *testing.T) {
	for _, mode := range []string{"legacy", "quality", "savings", "none"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "actions.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			profile := calibratedTestProfileConfig()
			_, profileDigest, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": profile})
			if err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{"service": "sonarr", "series_id": 10, "profile_config": profile, "shared_calibration": true, "calibration_items": 2}
			if mode != "legacy" {
				inputs["priority"] = "quality"
				if mode == "savings" {
					inputs["priority"] = "savings"
				}
			}
			if err := st.CreateActionInstance(store.ActionInstance{ID: "batch-calibration-test", ActionName: "transcode_batch", Status: StatusWaitingExternal, InputsJSON: toJSON(inputs), OutputsJSON: "{}", StateJSON: "{}"}); err != nil {
				t.Fatal(err)
			}
			items := []store.TranscodeBatchItem{
				{BatchID: "batch-calibration-test", ItemKey: "epfile-1", FilePath: "/media/first.mkv", EpisodeInfo: "S01E01", Decision: "transcode", Status: "queued"},
				{BatchID: "batch-calibration-test", ItemKey: "epfile-2", FilePath: "/media/last.mkv", EpisodeInfo: "S01E12", Decision: "transcode", Status: "queued"},
			}
			for _, item := range items {
				if err := st.CreateTranscodeBatchItem(item); err != nil {
					t.Fatal(err)
				}
			}
			good := func(q int, eligible bool) transcode.BenchmarkCandidateEvaluation {
				return transcode.BenchmarkCandidateEvaluation{VideoCodec: transcode.VideoCodecLibX265, Quality: q, MetricType: "vmaf", Eligible: eligible, MinimumMet: eligible, EstimatedBytes: 100, SavingsPercent: 30}
			}
			for i, item := range items {
				decision := transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{good(20, true), good(22, i == 0)}}
				if mode != "legacy" && (i == 1 || mode == "none") {
					decision.Evaluations = []transcode.BenchmarkCandidateEvaluation{good(20, false), good(22, false)}
				}
				outputs := map[string]any{"benchmark_decision": decision, "ephemeral_recipe_digest": profileDigest}
				if err := st.CreateActionInstance(store.ActionInstance{ID: "representative-" + item.ItemKey, ActionName: "benchmark_transcode", Status: StatusCompleted, IdempotencyKey: "batch-calibration-batch-calibration-test-" + item.ItemKey, InputsJSON: "{}", OutputsJSON: toJSON(outputs), StateJSON: "{}"}); err != nil {
					t.Fatal(err)
				}
			}
			engine := NewEngine(EngineDeps{Store: st})
			ec := &ExecutionContext{InstanceID: "batch-calibration-test", ActionName: "transcode_batch", Inputs: inputs, Outputs: map[string]any{}, State: map[string]any{}}
			step, done := engine.ensureSharedBatchCalibration(context.Background(), ec, items)
			if !done || step.Status == StepFailed {
				t.Fatalf("calibration should use persisted evidence: done=%v step=%+v", done, step)
			}
			result := getBatchCalibrationResult(ec.State["shared_calibration_result"])
			expected := 20
			if mode == "savings" {
				expected = 22
			}
			if mode == "none" {
				expected = 0
			}
			if result == nil || result.Quality != expected || len(result.ActionIDs) != 2 || result.Digest == "" {
				t.Fatalf("expected conservative common CRF20, got %+v", result)
			}
			parent, err := st.GetActionInstance("batch-calibration-test")
			if err != nil || !strings.Contains(parent.StateJSON, result.Digest) {
				t.Fatalf("calibration was not persisted before full-file dispatch: %v %+v", err, parent)
			}
			if _, done := engine.ensureSharedBatchCalibration(context.Background(), ec, items); !done {
				t.Fatal("persisted calibration should be reused after resume")
			}

			if mode != "legacy" {
				second, err := st.GetTranscodeBatchItem(ec.InstanceID, "epfile-2")
				if err != nil || second.Status != "skip" {
					t.Fatalf("exception must be persisted as skip: %+v %v", second, err)
				}
				first, err := st.GetTranscodeBatchItem(ec.InstanceID, "epfile-1")
				expectedStatus := "queued"
				if mode == "none" {
					expectedStatus = "skip"
				}
				if err != nil || first.Status != expectedStatus {
					t.Fatalf("unexpected first episode: %+v %v", first, err)
				}
			}
		})
	}
}

func TestSharedBatchRepresentativeSelectionAndDecision(t *testing.T) {
	items := []store.TranscodeBatchItem{
		{ItemKey: "epfile-1", Decision: "transcode", Status: "queued"},
		{ItemKey: "epfile-2", Decision: "skip", Status: "skip"},
		{ItemKey: "epfile-3", Decision: "transcode", Status: "queued"},
		{ItemKey: "epfile-4", Decision: "transcode", Status: "queued"},
	}
	selected := representativeBatchItems(items, 2)
	if len(selected) != 2 || selected[0].ItemKey != "epfile-1" || selected[1].ItemKey != "epfile-4" {
		t.Fatalf("expected first and last eligible episode, got %+v", selected)
	}
	if one := representativeBatchItems(items, 1); len(one) != 1 || one[0].ItemKey != "epfile-3" {
		t.Fatalf("expected middle eligible episode, got %+v", one)
	}
	decision := func(q20, q22 transcode.BenchmarkCandidateEvaluation) *transcode.BenchmarkDecision {
		return &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{q20, q22}}
	}
	good := func(q int, savings float64) transcode.BenchmarkCandidateEvaluation {
		return transcode.BenchmarkCandidateEvaluation{VideoCodec: transcode.VideoCodecLibX265, Quality: q, MetricType: "vmaf", Eligible: true, MinimumMet: true, EstimatedBytes: 100, SavingsPercent: savings}
	}
	decisions := []*transcode.BenchmarkDecision{decision(good(20, 45), good(22, 35)), decision(good(20, 55), good(22, 25))}
	if q, err := chooseSharedBatchQuality(decisions, []int{20, 22}, 15); err != nil || q != 22 {
		t.Fatalf("common highest CRF = %d, %v; want 22", q, err)
	}
	decisions[1].Evaluations[1].Eligible = false
	if q, err := chooseSharedBatchQuality(decisions, []int{20, 22}, 15); err != nil || q != 20 {
		t.Fatalf("unsafe CRF22 should fall back to 20, got %d, %v", q, err)
	}
	decisions[1].Evaluations[0].SavingsPercent = 3
	if _, err := chooseSharedBatchQuality(decisions, []int{20, 22}, 15); err == nil {
		t.Fatal("no common quality-and-size candidate must fail before full encode")
	}
}

func TestSharedBatchFinalPlanBoundToParentAndSource(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	result := BatchCalibrationResult{Profile: "anime-x265-calibrated", ProfileDigest: "sha256:profile", Quality: 20, ItemQualities: map[string]int{"epfile-1": 22}, ItemKeys: []string{"epfile-1"}, ActionIDs: []string{"act-benchmark-1"}, Digest: "sha256:calibration"}
	parentInputs, _ := json.Marshal(map[string]any{"profile": "anime-x265-calibrated"})
	parentState, _ := json.Marshal(map[string]any{"shared_calibration_result": result, "shared_calibration_profile_digest": result.ProfileDigest})
	if err := st.CreateActionInstance(store.ActionInstance{ID: "parent-batch", ActionName: "transcode_batch", Status: StatusWaitingExternal, InputsJSON: string(parentInputs), StateJSON: string(parentState)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "parent-batch", ItemKey: "epfile-1", FilePath: "/media/episode.mkv", Decision: "transcode", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(EngineDeps{Store: st})
	profile := recipe.Profile{Optimization: &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{Strategy: "distributed", SampleCount: 2, SampleSeconds: 10, Positions: []float64{0.25, 0.75}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "vmaf", VMAF: &recipe.MetricTarget{Model: "v1_1080p_3h", Minimum: 88, Target: 91, GuardrailEnforcement: "reject", WorstWindowSeconds: 5}, Banding: &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "reject"}, FinalValidation: &recipe.FinalValidationPolicy{Mode: "sampled"}},
		Search:   &recipe.SearchPolicy{QualityValues: []int{20, 22}, MaxCandidates: 2},
	}}
	source := &mediainspect.DetailedReport{DurationSec: 1400, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 23.976, Width: 1920, Height: 1080}}}
	makeContext := func() *ExecutionContext {
		return &ExecutionContext{ActionName: "transcode_media", Inputs: map[string]any{"parent_action_id": "parent-batch", "batch_item_key": "epfile-1", "batch_fixed_quality": 22, "batch_calibration_digest": result.Digest}, State: map[string]any{}}
	}
	ec := makeContext()
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 20}
	if err := engine.applySharedBatchCalibration(ec, plan, source, profile, "source-sha", result.ProfileDigest, "/media/episode.mkv"); err != nil {
		t.Fatal(err)
	}
	if plan.Quality != 22 || plan.QualityValidation == nil || len(plan.QualityValidation.Samples) != 2 || plan.PlanDigest == "" || !getBool(ec.State, "batch_shared_validation") {
		t.Fatalf("fixed plan lacks bound post-encode validation: %+v", plan)
	}
	if !strings.HasPrefix(plan.QualityValidation.BenchmarkRequestDigest, "sha256:") {
		t.Fatal("final validation provenance digest missing")
	}
	bad := makeContext()
	bad.Inputs["batch_fixed_quality"] = 20
	if err := engine.applySharedBatchCalibration(bad, &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}, source, profile, "source-sha", result.ProfileDigest, "/media/episode.mkv"); err == nil {
		t.Fatal("representative must use its own measured quality, not the batch default")
	}
	bad = makeContext()
	bad.Inputs["batch_calibration_digest"] = "sha256:wrong"
	if err := engine.applySharedBatchCalibration(bad, &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}, source, profile, "source-sha", result.ProfileDigest, "/media/episode.mkv"); err == nil {
		t.Fatal("mismatched parent calibration must fail closed")
	}
	bad = makeContext()
	if err := engine.applySharedBatchCalibration(bad, &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}, source, profile, "source-sha", result.ProfileDigest, "/media/other.mkv"); err == nil {
		t.Fatal("media outside parent batch must fail closed")
	}
	badSource := *source
	badSource.Video = []mediainspect.DetailedStream{{BitDepth: 10, FPS: 23.976}}
	if err := engine.applySharedBatchCalibration(makeContext(), &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}, &badSource, profile, "source-sha", result.ProfileDigest, "/media/episode.mkv"); err == nil {
		t.Fatal("10-bit source must not inherit 8-bit calibration")
	}
}

func TestBalancedBatchQualityTargetsBeforeSavings(t *testing.T) {
	candidate := func(q int, target bool, savings float64) transcode.BenchmarkCandidateEvaluation {
		return transcode.BenchmarkCandidateEvaluation{VideoCodec: transcode.VideoCodecLibX265, Quality: q, MetricType: "vmaf", Eligible: true, MinimumMet: true, TargetReached: target, EstimatedBytes: 100, SavingsPercent: savings}
	}
	for _, tc := range []struct {
		name        string
		evaluations []transcode.BenchmarkCandidateEvaluation
		want        int
	}{
		{"target before compression", []transcode.BenchmarkCandidateEvaluation{candidate(22, false, 81), candidate(20, true, 77)}, 20},
		{"savings among target candidates", []transcode.BenchmarkCandidateEvaluation{candidate(20, true, 77), candidate(22, true, 81)}, 22},
		{"measured savings before CRF", []transcode.BenchmarkCandidateEvaluation{candidate(20, true, 82), candidate(22, true, 81)}, 20},
		{"conservative fallback", []transcode.BenchmarkCandidateEvaluation{candidate(22, false, 81), candidate(20, false, 77)}, 20},
		{"minimum savings still required", []transcode.BenchmarkCandidateEvaluation{candidate(20, true, 5), candidate(22, false, 81)}, 22},
		{"no passing candidate", []transcode.BenchmarkCandidateEvaluation{candidate(20, true, 5)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseBatchItemQuality(&transcode.BenchmarkDecision{Evaluations: tc.evaluations}, []int{20, 22}, 15, "balanced")
			if got != tc.want {
				t.Fatalf("got CRF %d, want %d", got, tc.want)
			}
		})
	}
	rejected := candidate(22, true, 90)
	rejected.Eligible = false
	if got := chooseBatchItemQuality(&transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{rejected, candidate(20, true, 77)}}, []int{20, 22}, 15, "balanced"); got != 20 {
		t.Fatalf("ineligible target selected: %d", got)
	}
	if err := validateBatchCalibrationInputs(map[string]any{"profile": "anime-x265-calibrated", "priority": "balanced"}, "anime-x265-calibrated"); err != nil {
		t.Fatal(err)
	}
}

func TestPreserveQualityFreezesTargetAndSkipsBelowTarget(t *testing.T) {
	profile, _, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": calibratedTestProfileConfig()})
	if err != nil {
		t.Fatal(err)
	}
	strict := batchPriorityProfile(profile, "preserve_quality", "")
	if strict.Optimization.Quality.VMAF.Minimum != 91 || profile.Optimization.Quality.VMAF.Minimum != 88 {
		t.Fatal("preservation must enforce target without mutating shared recipe")
	}
	if _, _, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": strict}); err != nil {
		t.Fatal(err)
	}
	below := transcode.BenchmarkCandidateEvaluation{VideoCodec: transcode.VideoCodecLibX265, Quality: 20, MetricType: "vmaf", Eligible: true, MinimumMet: true, EstimatedBytes: 100, SavingsPercent: 30}
	decision := &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{below}}
	if q := chooseBatchItemQuality(decision, []int{20, 22}, 15, "preserve_quality"); q != 0 {
		t.Fatalf("below target must retain original: %d", q)
	}
	decision.Evaluations[0].TargetReached = true
	higher := decision.Evaluations[0]
	higher.Quality = 22
	higher.SavingsPercent = 40
	decision.Evaluations = append(decision.Evaluations, higher)
	if q := chooseBatchItemQuality(decision, []int{20, 22}, 15, "preserve_quality"); q != 20 {
		t.Fatalf("preservation must prefer conservative candidate: %d", q)
	}
	if err := validateBatchCalibrationInputs(map[string]any{"profile": "anime-x265-calibrated", "priority": "preserve_quality"}, "anime-x265-calibrated"); err != nil {
		t.Fatal(err)
	}
}
