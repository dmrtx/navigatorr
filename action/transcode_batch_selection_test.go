package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestAutomaticBatchDryRunByIntent(t *testing.T) {
	for _, priority := range []string{"", "balanced", "quality", "savings", "preserve_quality"} {
		t.Run("priority="+priority, func(t *testing.T) {
			mock := &mockTranscodeExecutor{}
			e, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
			defer st.Close()
			defer srv.Close()
			probe := strings.ReplaceAll(fakeMultiProbeJSON, `"pix_fmt": "yuv420p"`, `"pix_fmt": "yuv420p", "r_frame_rate": "24/1", "avg_frame_rate": "24/1"`)
			if err := os.WriteFile(e.deps.Ffprobe, []byte(probe), 0755); err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{"service": "sonarr", "series_id": 10, "dry_run": true}
			if priority != "" {
				inputs["priority"] = priority
			} else {
				inputs["shared_calibration"] = true
			}
			r, err := e.Run(context.Background(), "transcode_batch", inputs)
			if err != nil || r.Status != StatusCompleted {
				t.Fatalf("preview: %+v %v", r, err)
			}
			if atomic.LoadInt32(&mock.submitCalls) != 0 || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 0 {
				t.Fatal("preview started an encode")
			}
			items, err := st.ListTranscodeBatchItems(r.ID)
			if err != nil || len(items) != 3 {
				t.Fatalf("items: %+v %v", items, err)
			}
			for _, item := range items {
				if item.ItemKey == "epfile-102" {
					if item.Status != "skip" {
						t.Fatal("10-bit source was not preserved")
					}
				} else if item.Status != "queued" || item.Profile != "batch-generated" {
					t.Fatalf("wrong automatic selection: %+v", item)
				}
			}
			inst, _ := st.GetActionInstance(r.ID)
			var original map[string]any
			if err := json.Unmarshal([]byte(inst.InputsJSON), &original); err != nil {
				t.Fatal(err)
			}
			if len(original) != len(inputs) || original["profile"] != nil {
				t.Fatal("automatic routing rewrote user inputs")
			}
			if r.State["batch_evidence"] != nil {
				t.Fatal("preview invented measurements")
			}
			profile, _, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": r.State["shared_calibration_profile"]})
			if err != nil {
				t.Fatal(err)
			}
			if priority == "preserve_quality" && profile.Optimization.Quality.VMAF.Minimum != profile.Optimization.Quality.VMAF.Target {
				t.Fatal("preservation target not frozen")
			}
		})
	}
}

func TestAutomaticBatchSelectionFrozenAcrossRestart(t *testing.T) {
	for _, anime := range []bool{false, true} {
		t.Run(map[bool]string{false: "general", true: "anime"}[anime], func(t *testing.T) {
			st := setupTestStore(t)
			e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
			inputs := map[string]any{"service": "sonarr", "series_id": 10, "priority": "savings", "profile": "auto"}
			seedActionInstance(t, st, "automatic", "transcode_batch", StatusRunning, 0, "", inputs, map[string]any{})
			inst, _ := st.GetActionInstance("automatic")
			ec := parseExecutionContext(inst, e)
			if err := e.prepareAutomaticBatch(context.Background(), ec, anime); err != nil {
				t.Fatal(err)
			}
			profile := ec.State["shared_calibration_profile"].(recipe.Profile)
			if (profile.Video.Tune == "animation") != anime {
				t.Fatalf("wrong tuning: %+v", profile.Video)
			}
			digest := getString(ec.State, "shared_calibration_profile_digest")
			inst, _ = st.GetActionInstance("automatic")
			// No config after restart: the persisted policy must be sufficient.
			restarted := NewEngine(EngineDeps{Store: st})
			ec = parseExecutionContext(inst, restarted)
			if err := restarted.prepareAutomaticBatch(context.Background(), ec, !anime); err != nil {
				t.Fatal(err)
			}
			if getString(ec.State, "shared_calibration_profile_digest") != digest || !batchCalibrationEnabled(ec.Inputs, ec.State) {
				t.Fatal("restart changed the frozen selection")
			}
		})
	}
}

func TestAutomaticBatchRejectsConflictingOptions(t *testing.T) {
	for _, extra := range []map[string]any{{"priority": "fast"}, {"metric": "ssim"}, {"shared_calibration": "true"}, {"calibration_items": 0}} {
		if _, ok := extra["priority"]; !ok {
			extra["priority"] = "balanced"
		}
		st := setupTestStore(t)
		e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})
		ec := &ExecutionContext{Inputs: extra, State: map[string]any{}}
		if err := e.prepareAutomaticBatch(context.Background(), ec, true); err == nil {
			t.Fatalf("accepted conflicting options: %+v", extra)
		}
	}
}

func TestAutomaticBatchPreservesExplicitProfile(t *testing.T) {
	for _, inputs := range []map[string]any{{}, {"profile": "auto"}, {"profile": "anime-hevc"}, {"shared_calibration": false}, {"profile_config": calibratedTestProfileConfig()}} {
		e := NewEngine(EngineDeps{})
		ec := &ExecutionContext{Inputs: inputs, State: map[string]any{}}
		if err := e.prepareAutomaticBatch(context.Background(), ec, true); err != nil || len(ec.State) != 0 {
			t.Fatalf("explicit choice overwritten: %+v %v", ec.State, err)
		}
	}
}

func TestAutomaticBatchCalibratesBeforeEncodingAndCancelsBenchmark(t *testing.T) {
	var benchmark transcode.BenchmarkRequest
	mock := &mockTranscodeExecutor{
		capabilitiesFunc: func(ctx context.Context) (transcode.WorkerCapabilities, error) {
			caps, _ := (&mockTranscodeExecutor{}).Capabilities(ctx)
			caps.Encoders["libx265"] = true
			caps.Quality = &transcode.QualityCapabilities{Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}, CAMBIFullRef: true}
			caps.CapabilityFingerprint, _ = transcode.ComputeCapabilityFingerprint(caps)
			return caps, nil
		},
		benchmarkSubmitFunc: func(_ context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			benchmark = req
			return transcode.BenchmarkJob{ID: req.ID}, nil
		},
		benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusRunning}, nil
		},
	}
	e, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer st.Close()
	defer srv.Close()
	probe := strings.ReplaceAll(fakeMultiProbeJSON, `"pix_fmt": "yuv420p"`, `"pix_fmt": "yuv420p", "r_frame_rate": "24/1", "avg_frame_rate": "24/1"`)
	if err := os.WriteFile(e.deps.Ffprobe, []byte(probe), 0755); err != nil {
		t.Fatal(err)
	}
	r, err := e.Run(context.Background(), "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "max_items": 1, "priority": "quality"})
	if err != nil || r.Status != StatusWaitingExternal || r.WaitingCondition != "batch_calibration" {
		t.Fatalf("expected sampling before encode: status=%s error=%s %v", r.Status, r.Error, err)
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 || benchmark.Metric != "vmaf" {
		t.Fatalf("wrong dispatch order: %+v", benchmark)
	}
	// Same persisted action is resumed by a fresh engine; no duplicate sample.
	e = NewEngine(e.deps)
	r, err = e.Resume(context.Background(), r.ID, "", nil)
	if err != nil || r.Status != StatusWaitingExternal || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 {
		t.Fatalf("restart duplicated sampling: %+v %v", r, err)
	}
	r, err = e.Resume(context.Background(), r.ID, "cancel", nil)
	if err != nil || r.Status != StatusCancelled || atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatalf("cancel started full encode: %+v %v", r, err)
	}
	child, err := st.FindActionByIdempotencyKey("benchmark_transcode", "batch-calibration-"+r.ID+"-epfile-101")
	if err != nil || child == nil || child.Status != StatusCancelled {
		t.Fatalf("sample not cancelled: %+v %v", child, err)
	}
}

func TestAutomaticBatchCreatesOneRecipeAndStopsSearching(t *testing.T) {
	for _, tc := range []struct {
		priority string
		want     int
	}{{"balanced", 23}, {"quality", 20}, {"savings", 26}, {"preserve_quality", 20}, {"none", 0}} {
		t.Run(tc.priority, func(t *testing.T) {
			st := setupTestStore(t)
			mock := &mockTranscodeExecutor{}
			e := NewEngine(EngineDeps{Store: st, Transcode: mock})
			priority := tc.priority
			if priority == "none" {
				priority = "balanced"
			}
			inputs := map[string]any{"service": "sonarr", "series_id": 10, "priority": priority}
			seedActionInstance(t, st, "generated", "transcode_batch", StatusWaitingExternal, 1, "", inputs, map[string]any{})
			inst, _ := st.GetActionInstance("generated")
			ec := parseExecutionContext(inst, e)
			if err := e.prepareAutomaticBatch(context.Background(), ec, false); err != nil {
				t.Fatal(err)
			}
			searchDigest := getString(ec.State, "shared_calibration_profile_digest")
			items := []store.TranscodeBatchItem{
				{BatchID: ec.InstanceID, ItemKey: "epfile-1", FilePath: "/media/first.mkv", Decision: "transcode", Status: "queued"},
				{BatchID: ec.InstanceID, ItemKey: "epfile-2", FilePath: "/media/last.mkv", Decision: "transcode", Status: "queued"},
			}
			for i, item := range items {
				if err := st.CreateTranscodeBatchItem(item); err != nil {
					t.Fatal(err)
				}
				evals := []transcode.BenchmarkCandidateEvaluation{}
				for j, q := range []int{20, 23, 26} {
					evals = append(evals, transcode.BenchmarkCandidateEvaluation{Quality: q, VideoCodec: transcode.VideoCodecLibX265, MetricType: "vmaf", Eligible: tc.priority != "none", MinimumMet: true, TargetReached: q < 26, EstimatedBytes: 100, Score: 96 - float64(j*3+i), SavingsPercent: float64(20 + j*10 + i)})
				}
				if err := st.CreateActionInstance(store.ActionInstance{ID: "sample-" + item.ItemKey, ActionName: "benchmark_transcode", Status: StatusCompleted, IdempotencyKey: "batch-calibration-generated-" + item.ItemKey, InputsJSON: "{}", StateJSON: "{}", OutputsJSON: toJSON(map[string]any{"benchmark_decision": transcode.BenchmarkDecision{Evaluations: evals}, "ephemeral_recipe_digest": searchDigest})}); err != nil {
					t.Fatal(err)
				}
			}
			step, done := e.ensureSharedBatchCalibration(context.Background(), ec, items)
			if !done || step.Status == StepFailed {
				t.Fatalf("calibration: %+v", step)
			}
			result := getBatchCalibrationResult(ec.State["shared_calibration_result"])
			if result == nil || result.Quality != tc.want {
				t.Fatalf("selection: %+v", result)
			}
			if tc.want > 0 {
				profile := ec.State["shared_calibration_profile"].(recipe.Profile)
				if profile.Video.Quality != tc.want || len(profile.Optimization.Search.QualityValues) != 1 || result.ProfileDigest == searchDigest {
					t.Fatalf("recipe not frozen: %+v", profile)
				}
				for _, item := range items {
					if result.qualityFor(item.ItemKey) != tc.want {
						t.Fatal("representatives received different recipes")
					}
				}
				if result.qualityFor("epfile-other") != tc.want {
					t.Fatal("remaining episodes would use a different recipe")
				}
			} else if len(result.SkippedItems) != 2 || ec.State["batch_evidence"] != nil {
				t.Fatal("unsuitable search must preserve originals")
			}
			inst, _ = st.GetActionInstance(ec.InstanceID)
			ec = parseExecutionContext(inst, e)
			step, done = e.ensureSharedBatchCalibration(context.Background(), ec, items)
			if !done || step.Status == StepFailed || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 0 || atomic.LoadInt32(&mock.submitCalls) != 0 {
				t.Fatal("restart repeated the search")
			}
		})
	}
}

func TestAutomaticBatchRequiresCommonPassingSetting(t *testing.T) {
	good := func(q int) transcode.BenchmarkCandidateEvaluation {
		return transcode.BenchmarkCandidateEvaluation{Quality: q, VideoCodec: transcode.VideoCodecLibX265, MetricType: "vmaf", Eligible: true, MinimumMet: true, TargetReached: true, EstimatedBytes: 100, SavingsPercent: 25}
	}
	a, b := good(20), good(23)
	decisions := []*transcode.BenchmarkDecision{{Evaluations: []transcode.BenchmarkCandidateEvaluation{a}}, {Evaluations: []transcode.BenchmarkCandidateEvaluation{b}}}
	if q := chooseAutomaticBatchQuality(decisions, []int{20, 23, 26}, 15, "balanced"); q != 0 {
		t.Fatalf("unmeasured common setting accepted: %d", q)
	}
}

func TestDirectBatchDoesNotRequireQualityCalibration(t *testing.T) {
	mock := &mockTranscodeExecutor{submitFunc: func(_ context.Context, r transcode.Request) (transcode.Job, error) {
		writeCandidateOutput(r.CandidatePath)
		return transcode.Job{ID: r.ID}, nil
	}}
	e, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer st.Close()
	defer srv.Close()
	// The mock has no VMAF model/CAMBI capability. Direct static encoding must
	// still work, with ordinary output validation and no original replacement.
	r, err := e.Run(context.Background(), "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "max_items": 1, "profile": "general-hevc"})
	if err != nil || r.Status != StatusCompleted || atomic.LoadInt32(&mock.submitCalls) != 1 || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 0 {
		t.Fatalf("direct path changed: %+v %v", r, err)
	}
	if r.State["batch_selection"] != nil {
		t.Fatal("calibration imposed on direct request")
	}
}

func TestIntentBatchSampleToCandidate(t *testing.T) {
	var request transcode.Request
	mock := &mockTranscodeExecutor{
		capabilitiesFunc: func(ctx context.Context) (transcode.WorkerCapabilities, error) {
			caps, _ := (&mockTranscodeExecutor{}).Capabilities(ctx)
			caps.Encoders["libx265"] = true
			caps.Quality = &transcode.QualityCapabilities{Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}, CAMBIFullRef: true}
			caps.CapabilityFingerprint, _ = transcode.ComputeCapabilityFingerprint(caps)
			return caps, nil
		},
		benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			evaluations := []transcode.BenchmarkCandidateEvaluation{}
			for i, q := range []int{20, 23, 26} {
				evaluations = append(evaluations, transcode.BenchmarkCandidateEvaluation{Quality: q, VideoCodec: transcode.VideoCodecLibX265, MetricType: "vmaf", Eligible: true, MinimumMet: true, TargetReached: q < 26, Score: 96 - float64(i*3), EstimatedBytes: 100, SavingsPercent: float64(20 + i*10)})
			}
			return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, Decision: &transcode.BenchmarkDecision{Evaluations: evaluations}}, nil
		},
		submitFunc: func(_ context.Context, r transcode.Request) (transcode.Job, error) {
			request = r
			writeCandidateOutput(r.CandidatePath)
			return transcode.Job{ID: r.ID}, nil
		},
		statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
			if request.Plan == nil || request.Plan.QualityValidation == nil {
				t.Fatal("generated recipe lost final validation")
			}
			data, err := os.ReadFile(request.CandidatePath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			hash, size := hex.EncodeToString(sum[:]), int64(len(data))
			return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, CandidatePath: request.CandidatePath, CandidateSHA256: hash, CandidateSizeBytes: size, QualityEvidence: &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: hash, CandidateSizeBytes: size, PlanDigest: request.Plan.PlanDigest, BenchmarkRequestDigest: request.Plan.QualityValidation.BenchmarkRequestDigest}}, nil
		},
	}
	e, st, _, srv, _ := setupBatchTestEnv(t, mock, 1)
	defer st.Close()
	defer srv.Close()
	probe := strings.ReplaceAll(fakeMultiProbeJSON, `"pix_fmt": "yuv420p"`, `"pix_fmt": "yuv420p", "r_frame_rate": "24/1", "avg_frame_rate": "24/1"`)
	probe = strings.ReplaceAll(probe, `"bits_per_raw_sample": "10"`, `"bits_per_raw_sample": "8"`)
	if err := os.WriteFile(e.deps.Ffprobe, []byte(probe), 0755); err != nil {
		t.Fatal(err)
	}
	r, err := e.Run(context.Background(), "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "priority": "balanced", "max_items": 1})
	if err != nil || r.Status != StatusCompleted || getInt(r.Outputs, "completed") != 1 {
		t.Fatalf("end-to-end batch: %+v %v", r, err)
	}
	if atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 || atomic.LoadInt32(&mock.submitCalls) != 1 || request.Plan.Quality != 23 {
		t.Fatal("repeated search or wrong recipe in full encode")
	}
	selection := r.State["batch_selection"].(BatchSelection)
	if selection.Status != "ready" || selection.Quality != 23 {
		t.Fatalf("stale selection: %+v", selection)
	}
	items, _ := st.ListTranscodeBatchItems(r.ID)
	if len(items) != 1 || items[0].CandidatePath == "" {
		t.Fatal("candidate not persisted")
	}
	if info, err := os.Stat(items[0].FilePath); err != nil || info.Size() != 1400000000 {
		t.Fatal("source was replaced")
	}
}
