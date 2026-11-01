package action

import (
	"context"
	"encoding/json"
	"github.com/jakenesler/navigatorr/mediainspect"
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
	altered.MinSavingsPercent = 15
	ec.State["mode_policy"] = altered
	if _, err := freezeModePolicy(ec); err == nil {
		t.Fatal("policy tampering not rejected")
	}
}
func modeTestCapabilities() transcode.WorkerCapabilities {
	return transcode.WorkerCapabilities{AutomaticModesV2: true, SizeModeV3: true, SizeAudioLevels: true, SizeAudioObjectBed: true, TrackSelection: true, ExplicitMain10Benchmarks: true, ProtocolVersion: transcode.WorkerProtocolVersion, Encoders: map[string]bool{transcode.VideoCodecLibX265: true, transcode.VideoCodecHEVCVideoToolbox: true}, Filters: map[string]bool{"libvmaf": true}, Quality: &transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true, MeasurementBitDepth: 10}}}}
}
func TestModeFinalValidationRequestAcrossBitDepths(t *testing.T) {
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		for _, depth := range []int{8, 10} {
			ec := &ExecutionContext{InstanceID: "mode-bench", Inputs: map[string]any{"mode": mode}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
			policy, err := freezeModePolicy(ec)
			if err != nil {
				t.Fatal(err)
			}
			plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: depth, AudioMode: policy.Profile.Audio.Mode}
			if err := preserveSourceBitDepth(plan, depth, true); err != nil {
				t.Fatal(err)
			}
			ec.State["plan"] = plan
			rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: depth, FPS: 24, BitRate: 5000000}}}
			req, err := finalRequestForPlan(ec, "/media/source.mkv", rep, policy.Profile.Optimization, modeTestCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			if req.Mode != mode || req.ModePolicyVersion == "" || req.Metric != "vmaf" || len(req.Samples) == 0 || req.Quality == nil || req.Quality.FinalValidation == nil {
				t.Fatalf("%s/%d: %+v", mode, depth, req)
			}
		}
	}
}
func TestModeSourceEligibility(t *testing.T) {
	report := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "hevc", BitDepth: 10, FPS: 24}}}
	if modeSourceReason("x265_preserve", report, false) != "already_target_codec" || modeSourceReason("size", report, false) != "" {
		t.Fatal("HEVC routing wrong")
	}
	report.Video[0].BitDepth = 12
	if modeSourceReason("quality", report, false) != "unsupported_source" {
		t.Fatal("unsupported class admitted")
	}
}
func TestModeFinalUnroundedSizeGate(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		bytes int
		skip  bool
	}{{"quality", 95, false}, {"quality", 101, true}, {"x265_preserve", 95, false}, {"x265_preserve", 110, true}} {
		c := newRecipeCase(t, map[string]any{"mode": tc.mode}, nil)
		planRecipe(t, c)
		validateRecipeCase(t, c, tc.bytes)
		result, err := c.engine.stepTranscodeValidate(context.Background(), c.ec)
		if err != nil || result.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") != tc.skip {
			t.Fatalf("%+v -> %+v %v", tc, result, err)
		}
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
			executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return modeTestCapabilities(), nil }, submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
				plans = append(plans, req.Plan)
				return transcode.Job{ID: req.ID}, nil
			}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
				return transcode.JobStatus{ID: id, Status: transcode.StatusRunning}, nil
			}}
			e, st := setupTranscodeEngine(t, executor, probe, []string{dir}, []string{dir}, true)
			defer st.Close()
			// Two slots: the single encode keeps one while the batch's starts.
			e.deps.Config.Transcode.MaxParallelJobs = 2
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
	data, err := os.ReadFile("../docs/transcode/reviews/fixtures/transcode-modes-v1.json")
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
			policy, _ := modePolicyFor(mode, false, 0)
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
