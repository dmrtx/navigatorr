package action

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"os"
	"strings"
	"testing"
)

func TestModeInputsRejectTuningAndKeepLegacy(t *testing.T) {
	if err := validateModeInputs(map[string]any{"profile": "general-hevc", "min_savings_percent": 0}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		for _, key := range []string{"profile", "profile_config", "priority", "metric", "shared_calibration", "min_savings_percent", "max_size_increase_percent", "preserve_source_bit_depth"} {
			if validateModeInputs(map[string]any{"mode": mode, key: nil}) == nil {
				t.Fatalf("%s accepted %s", mode, key)
			}
		}
	}
	for _, raw := range []any{"", "balanced", nil, 12} {
		if validateModeInputs(map[string]any{"mode": raw}) == nil {
			t.Fatalf("accepted mode %v", raw)
		}
	}
}
func TestModePolicyFreezeAndDigest(t *testing.T) {
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	p, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(ec.State)
	_ = json.Unmarshal(data, &ec.State)
	after, err := freezeModePolicy(ec)
	if err != nil || after.Digest != p.Digest {
		t.Fatalf("restart %+v %v", after, err)
	}
	altered := *after
	altered.MinSavingsPercent = 0
	ec.State["mode_policy"] = altered
	if _, err := freezeModePolicy(ec); err == nil {
		t.Fatal("policy tampering not rejected")
	}
}
func TestModeSearchBudgetReservationsAndSettlement(t *testing.T) {
	ec := &ExecutionContext{State: map[string]any{"search_budget_seconds": 600, "operation_search_budget_seconds": 3600}}
	for i := 0; i < 6; i++ {
		if !reserveModeSearch(ec, string(rune('a'+i))) {
			t.Fatal("early exhausted")
		}
	}
	if reserveModeSearch(ec, "g") {
		t.Fatal("operation bound exceeded")
	}
	if !reserveModeSearch(ec, "a") {
		t.Fatal("same admission charged twice")
	}
	entries := modeBudgetEntries(ec)
	used := 2.5
	e := entries["a"]
	e.Consumed = &used
	entries["a"] = e
	ec.State["search_budget_entries"] = entries
	if reserveModeSearch(ec, "g") {
		t.Fatal("fractional unspent allowance cannot fund another 600s job yet")
	}
	used = 0
	e.Consumed = &used
	entries["a"] = e
	ec.State["search_budget_entries"] = entries
	if !reserveModeSearch(ec, "g") {
		t.Fatal("unused reservation was not released")
	}
}
func modeTestCapabilities() transcode.WorkerCapabilities {
	return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, Encoders: map[string]bool{transcode.VideoCodecLibX265: true, transcode.VideoCodecHEVCVideoToolbox: true}, Filters: map[string]bool{"libvmaf": true}, Quality: &transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true, MeasurementBitDepth: 10}}}}
}
func TestModeMixedCandidatesBudgetAndMain10(t *testing.T) {
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		for _, depth := range []int{8, 10} {
			ec := &ExecutionContext{InstanceID: "mode-bench", Inputs: map[string]any{"mode": mode}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
			policy, err := freezeModePolicy(ec)
			if err != nil {
				t.Fatal(err)
			}
			plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: depth}
			if err := preserveSourceBitDepth(plan, depth, true); err != nil {
				t.Fatal(err)
			}
			ec.State["plan"] = plan
			rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: depth, FPS: 24, BitRate: 5000000}}}
			req, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, policy.Profile.Optimization, modeTestCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			want := 6
			if mode == "x265_preserve" {
				want = 3
			}
			if len(req.Candidates) != want || req.SearchBudgetSeconds != 600 {
				t.Fatalf("%s/%d: %+v", mode, depth, req)
			}
			for _, candidate := range req.Candidates {
				if mode == "x265_preserve" && candidate.VideoCodec != transcode.VideoCodecLibX265 {
					t.Fatal("preserve selected hardware")
				}
				if depth == 10 && candidate.VideoProfile != "main10" {
					t.Fatalf("10bit profile lost %+v", candidate)
				}
			}
		}
	}
}
func TestModeSourceEligibility(t *testing.T) {
	report := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "hevc", BitDepth: 10, FPS: 24}}}
	if modeSourceReason("x265_preserve", report) != "already_target_codec" || modeSourceReason("size", report) != "" {
		t.Fatal("HEVC routing wrong")
	}
	report.Video[0].BitDepth = 12
	if modeSourceReason("quality", report) != "unsupported_source" {
		t.Fatal("unsupported class admitted")
	}
}
func TestModeFinalUnroundedSizeGate(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		bytes int
		skip  bool
	}{{"size", 85, false}, {"quality", 95, true}, {"x265_preserve", 95, false}, {"x265_preserve", 110, true}} {
		candidate := t.TempDir() + "/candidate.mkv"
		if err := os.WriteFile(candidate, make([]byte, tc.bytes), 0600); err != nil {
			t.Fatal(err)
		}
		ec := &ExecutionContext{Inputs: map[string]any{"mode": tc.mode}, State: map[string]any{"candidate_path": candidate, "original_size": int64(100), "original": map[string]any{"size_bytes": int64(100)}}}
		policy, _ := freezeModePolicy(ec)
		plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}
		plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", Quality: *ensureBenchmarkQualityMetric(buildBenchmarkQualityConfig(policy.Profile.Optimization.Quality), "vmaf"), BenchmarkRequestDigest: "request"}
		_ = applyModePlan(plan, policy)
		ec.State["candidate_sha256"] = "candidate"
		ec.State["quality_evidence"] = map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest, "benchmark_request_digest": "request"}
		ec.State["plan"] = plan
		result, err := (&Engine{}).stepTranscodeValidate(context.Background(), ec)
		if err != nil || result.Status != StepCompleted || getBool(ec.State, "skip_transcode") != tc.skip {
			t.Fatalf("%+v -> %+v %v", tc, result, err)
		}
	}
}
func TestFilesystemCalibrationUsesPersistedMembership(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st})
	profile := automaticBatchSearchProfile(false)
	_, digest, _, err := decodeEphemeralProfileInput(map[string]any{"profile_config": profile})
	if err != nil {
		t.Fatal(err)
	}
	result := BatchCalibrationResult{ProfileDigest: digest, Quality: 20, Digest: "sha256:calibration"}
	inputs, _ := json.Marshal(map[string]any{"shared_calibration": true})
	state, _ := json.Marshal(map[string]any{"shared_calibration_result": result, "shared_calibration_profile_digest": digest})
	if err := st.CreateActionInstance(store.ActionInstance{ID: "parent", ActionName: "transcode_batch", Status: StatusWaitingExternal, InputsJSON: string(inputs), StateJSON: string(state)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "parent", ItemKey: "file-authorized", FilePath: "/media/file.mkv", Decision: "transcode", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	ec := &ExecutionContext{ActionName: "transcode_media", Inputs: map[string]any{"parent_action_id": "parent", "batch_item_key": "file-authorized", "batch_fixed_quality": 20, "batch_calibration_digest": result.Digest}, State: map[string]any{}}
	report := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24}}}
	if err := e.applySharedBatchCalibration(ec, &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}, report, profile, "source", digest, "/media/file.mkv"); err != nil {
		t.Fatal(err)
	}
	if err := e.applySharedBatchCalibration(ec, &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265}, report, profile, "source", digest, "/media/other.mkv"); err == nil {
		t.Fatal("unrelated filesystem source accepted")
	}
}

func TestModeNativeMain10Preflight(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/native10.mkv"
	probe := dir + "/ffprobe"
	if err := os.WriteFile(file, make([]byte, 100), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p10le","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"10"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}
JSON
`
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, probe, []string{dir}, []string{dir}, false)
	defer st.Close()
	ec := &ExecutionContext{InstanceID: "main10-mode", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "quality"}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	result, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || result.Status != StepCompleted {
		t.Fatalf("preflight %+v %v", result, err)
	}
	plan := getPlan(ec.State["plan"])
	if plan == nil || plan.ExpectedBitDepth != 10 || plan.VideoProfile != "main10" || plan.Mode != "quality" || plan.SizePolicy == nil {
		t.Fatalf("native Main10 plan lost: %+v", plan)
	}
}

func TestModeFilesystemBatchEightQuickSearches(t *testing.T) {
	dir := t.TempDir()
	probe := dir + "/ffprobe"
	script := `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}
JSON
`
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for i := 0; i < 8; i++ {
		path := dir + "/source-" + string(rune('a'+i)) + ".mkv"
		if err := os.WriteFile(path, []byte("different source "+path), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	seconds := 2.0
	executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return modeTestCapabilities(), nil }, benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, SearchSeconds: &seconds, Decision: &transcode.BenchmarkDecision{DecisionReason: "no_suitable_candidate"}}, nil
	}}
	e, st := setupTranscodeEngine(t, executor, probe, []string{dir}, []string{dir}, true)
	defer st.Close()
	e.deps.Config.Transcode.MaxParallelJobs = 3
	result, err := e.Run(context.Background(), "transcode_batch", map[string]any{"paths": paths, "mode": "quality"})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("8 quick searches %+v %v", result, err)
	}
	items, err := st.ListTranscodeBatchItems(result.ID)
	if err != nil || len(items) != 8 {
		t.Fatalf("manifest %d %v", len(items), err)
	}
	for _, item := range items {
		if item.Status != "skip" || item.ChildActionID == "" || item.CandidatePath != "" {
			t.Fatalf("false successful encode %+v", item)
		}
	}
	inst, _ := st.GetActionInstance(result.ID)
	reloaded := parseExecutionContext(inst, e)
	entries := modeBudgetEntries(reloaded)
	if len(entries) != 8 {
		t.Fatalf("reservations reset/lost after restart: %+v", entries)
	}
	for _, entry := range entries {
		if entry.Consumed == nil || *entry.Consumed != 2 {
			t.Fatalf("reservation was not settled: %+v", entry)
		}
	}
	if executor.submitCalls != 0 || executor.benchmarkSubmitCalls != 8 {
		t.Fatalf("unexpected effects encode=%d benchmarks=%d", executor.submitCalls, executor.benchmarkSubmitCalls)
	}
}

func TestModeSingleAndBatchMaterializeEquivalentPlans(t *testing.T) {
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			probe := dir + "/ffprobe"
			source := dir + "/source.mkv"
			_ = os.WriteFile(source, make([]byte, 1000), 0600)
			_ = os.WriteFile(probe, []byte(`#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"1000"},"chapters":[]}
JSON
`), 0700)
			plans := []*transcode.Plan{}
			executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return modeTestCapabilities(), nil }, benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
				seconds := 1.0
				return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, SearchSeconds: &seconds, Decision: &transcode.BenchmarkDecision{Winner: &transcode.BenchmarkWinner{CandidateID: "cand_crf20", VideoCodec: transcode.VideoCodecLibX265, Quality: 20, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, Score: 99, MetricType: "vmaf", TargetReached: true, MinimumMet: true, EstimatedTotalBytes: 800, SavingsPercent: 20}}}, nil
			}, submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
				plans = append(plans, req.Plan)
				return transcode.Job{ID: req.ID}, nil
			}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
				return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
			}}
			e, st := setupTranscodeEngine(t, executor, probe, []string{dir}, []string{dir}, true)
			defer st.Close()
			single, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": source, "mode": mode})
			if err != nil || single.Status != StatusWaitingExternal {
				t.Fatalf("single %+v %v", single, err)
			}
			batch, err := e.Run(context.Background(), "transcode_batch", map[string]any{"paths": []string{source}, "mode": mode})
			if err != nil || batch.Status != StatusWaitingExternal {
				t.Fatalf("batch %+v %v", batch, err)
			}
			if len(plans) != 2 {
				t.Fatalf("plans=%d", len(plans))
			}
			a, b := *plans[0], *plans[1]
			// External request identities are deliberately distinct; effective policy,
			// encoder, complete rate/preservation settings and final windows are equal.
			a.PlanDigest = ""
			b.PlanDigest = ""
			a.QualityValidation.BenchmarkRequestDigest = ""
			b.QualityValidation.BenchmarkRequestDigest = ""
			encodedA, _ := json.Marshal(a)
			encodedB, _ := json.Marshal(b)
			if string(encodedA) != string(encodedB) {
				t.Fatalf("mode adapters diverge single=%s batch=%s", encodedA, encodedB)
			}
		})
	}
}

func TestModePolicyAgainstMeasuredCalibrationFixtures(t *testing.T) {
	data, err := os.ReadFile("../docs/reviews/fixtures/transcode-modes-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Fixture   string  `json:"fixture"`
		Quality   int     `json:"quality"`
		Mean      float64 `json:"vmaf_mean"`
		P5        float64 `json:"vmaf_p5"`
		CambiMean float64 `json:"cambi_mean"`
		CambiPeak float64 `json:"cambi_max"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 9 {
		t.Fatalf("expected9 real fixture measurements, got%d", len(rows))
	}
	for _, row := range rows {
		for _, mode := range []string{"size", "quality", "x265_preserve"} {
			policy, _ := modePolicy(mode, false)
			quality := policy.Profile.Optimization.Quality
			vmaf := quality.VMAF
			passing := row.Mean >= vmaf.Minimum && row.P5 >= *vmaf.P5Minimum && row.CambiMean <= *quality.Banding.MaxMean && row.CambiPeak <= *quality.Banding.MaxPeak
			// These expectations come from the inspected fixture characteristics,
			// rather than computing a second copy of selector ranking.
			if row.Quality == 40 && passing {
				t.Fatalf("visibly degraded%s q40 passes%s", row.Fixture, mode)
			}
			if row.Fixture == "grain10" && row.Quality == 26 && passing != (mode == "size") {
				t.Fatalf("noise-smoothed grain accepted by strict policy%s", mode)
			}
			if row.Fixture == "motion8" && row.Quality == 20 && !passing {
				t.Fatalf("high-detail motion fixture rejected by%s", mode)
			}
		}
	}
}

func TestModeBudgetUnknownRestartCostKeepsFullReservation(t *testing.T) {
	for _, raw := range []any{nil, "100"} {
		t.Run(fmt.Sprintf("cost=%v", raw), func(t *testing.T) {
			st := setupTestStore(t)
			defer st.Close()
			e := NewEngine(EngineDeps{Store: st})
			ec := &ExecutionContext{InstanceID: "parent", Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
			_, _ = freezeModePolicy(ec)
			item := store.TranscodeBatchItem{ItemKey: "file-a", ChildActionID: "resumed-child"}
			key := batchChildKey(ec, item)
			if !reserveModeSearch(ec, key) {
				t.Fatal("initial reservation failed")
			}
			state := map[string]any{"benchmark_submitted": true, "benchmark_done": true}
			// A restarted worker cannot prove cumulative cost from an interrupted
			// invocation. Missing/null/malformed cost must never mean zero or100seconds.
			if raw != nil {
				state["benchmark_search_seconds"] = raw
			}
			seedActionInstance(t, st, item.ChildActionID, "transcode_media", StatusCompleted, 7, "", map[string]any{"mode": "quality"}, state)
			e.settleModeSearches(ec, []store.TranscodeBatchItem{item})
			entry := modeBudgetEntries(ec)[key]
			if entry.Consumed != nil || entry.Reserved != 600 {
				t.Fatalf("unknown prior runner cost released reservation: %+v", entry)
			}
			data, _ := json.Marshal(ec.State)
			_ = json.Unmarshal(data, &ec.State)
			e.settleModeSearches(ec, []store.TranscodeBatchItem{item})
			if modeBudgetEntries(ec)[key].Consumed != nil {
				t.Fatal("coordinator reload released unknown reservation")
			}
		})
	}
}
