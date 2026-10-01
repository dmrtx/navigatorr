package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestBenchmarkExplicitMain10RequiresOptOutAndReachesWorkerUnchanged(t *testing.T) {
	for _, optOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "conflicting_default", true: "explicit_conversion"}[optOut], func(t *testing.T) {
			var submitted *transcode.BenchmarkRequest
			mock := &mockTranscodeExecutor{
				benchmarkSubmitFunc: func(_ context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
					submitted = &req
					return transcode.BenchmarkJob{ID: req.ID}, nil
				},
				benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
					return transcode.BenchmarkStatus{ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: transcode.StatusRunning}, nil
				},
			}
			e, _, path, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, nil)
			profile := testEphemeralBatchProfileConfig()
			video := profile["video"].(map[string]any)
			video["codec"], video["profile"], video["pixel_format"] = "hevc_videotoolbox", "main10", "p010le"
			delete(video, "preset")
			delete(video, "tune")
			video["quality"] = 65
			profile["optimization"] = map[string]any{"enabled": true, "search": map[string]any{"quality_values": []int{65}, "max_candidates": 1}}
			inputs := map[string]any{"path": path, "profile_config": profile}
			if optOut {
				inputs["preserve_source_bit_depth"] = false
			}
			r, err := e.Run(context.Background(), "benchmark_transcode", inputs)
			if err != nil {
				t.Fatal(err)
			}
			if !optOut {
				if r.Status != StatusFailed || !strings.Contains(r.Error, "preserve_source_bit_depth=false") || submitted != nil {
					t.Fatalf("explicit Main10 silently changed: %+v", r)
				}
				return
			}
			if r.Status != StatusWaitingExternal || submitted == nil || len(submitted.Candidates) != 1 {
				t.Fatalf("Main10 benchmark not submitted: %+v", r)
			}
			for _, candidate := range submitted.Candidates {
				if candidate.VideoProfile != "main10" || candidate.PixelFormat != "p010le" {
					t.Fatalf("worker received changed Main10 settings: %+v", candidate)
				}
			}
			plan := getPlan(r.State["plan"])
			if plan == nil || plan.ExpectedBitDepth != 10 {
				t.Fatalf("plan lost actual 10-bit output: %+v", plan)
			}
		})
	}
}

func TestValidateTranscodeInputsRejectsIgnoredKnobs(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := &ExecutionContext{
		ActionName: "benchmark_transcode",
		Inputs: map[string]any{
			"path":           "/media/example.mkv",
			"profile":        "live-action-hevc",
			"tune":           "animation",
			"crf_candidates": []any{20.0, 22.0, 24.0},
		},
	}
	err := e.validateTranscodeInputs(ec)
	if err == nil || !strings.Contains(err.Error(), "unsupported input") || !strings.Contains(err.Error(), "profile_config") {
		t.Fatalf("expected explicit rejection of ignored knobs, got %v", err)
	}
}

func TestValidateTranscodeInputsAcceptsProfileConfig(t *testing.T) {
	e := NewEngine(EngineDeps{})
	for _, actionName := range []string{"benchmark_transcode", "transcode_media", "transcode_batch"} {
		t.Run(actionName, func(t *testing.T) {
			inputs := map[string]any{
				"profile_config": map[string]any{
					"container": "mkv",
				},
			}
			if actionName == "transcode_batch" {
				inputs["service"] = "sonarr"
				inputs["series_id"] = 10
			} else {
				inputs["path"] = "/media/example.mkv"
			}
			ec := &ExecutionContext{ActionName: actionName, Inputs: inputs}
			if err := e.validateTranscodeInputs(ec); err != nil {
				t.Fatalf("profile_config should be a declared input: %v", err)
			}
		})
	}
}

func testEphemeralBatchProfileConfig() map[string]any {
	return map[string]any{
		"container": "mkv",
		"video": map[string]any{
			"codec":        "libx265",
			"quality":      24,
			"preset":       "slow",
			"tune":         "animation",
			"profile":      "main",
			"pixel_format": "yuv420p",
		},
		"audio":     map[string]any{"mode": "copy"},
		"subtitles": map[string]any{"mode": "preserve", "convert_incompatible": true},
		"preserve":  map[string]any{"metadata": true, "chapters": true, "attachments": true},
		"resilience": map[string]any{
			"max_attempts":          1,
			"transient_retries":     0,
			"retry_backoff_seconds": []any{},
			"max_fallbacks":         1,
			"fallbacks": []any{
				map[string]any{"when": "container_subtitle_incompatible", "action": "apply_container_conversion"},
			},
		},
	}
}

func TestMetricAutoSelectsSafeCapabilityForSourceBitDepth(t *testing.T) {
	caps := transcode.WorkerCapabilities{Filters: map[string]bool{"libvmaf": true, "ssim": true}}
	if got, err := resolveAutoBenchmarkMetric("auto", caps, 8); err != nil || got != "vmaf" {
		t.Fatalf("8-bit auto metric = %q, %v; want vmaf", got, err)
	}
	if got, err := resolveAutoBenchmarkMetric("auto", caps, 10); err != nil || got != "ssim" {
		t.Fatalf("10-bit auto metric = %q, %v; want ssim", got, err)
	}
	if got, err := resolveAutoBenchmarkMetric("ssim", caps, 8); err != nil || got != "ssim" {
		t.Fatalf("explicit metric must be preserved, got %q, %v", got, err)
	}
	if _, err := resolveAutoBenchmarkMetric("auto", transcode.WorkerCapabilities{Filters: map[string]bool{"libvmaf": true}}, 10); err == nil || !strings.Contains(err.Error(), "no safe metric") {
		t.Fatalf("10-bit auto without SSIM must fail closed, got %v", err)
	}
}

func TestPreserveSourceBitDepthDefaultsToPreserveAndAllowsExplicitOptOut(t *testing.T) {
	plan := &transcode.Plan{
		Container:           "mkv",
		VideoCodec:          transcode.VideoCodecHEVCVideoToolbox,
		Quality:             70,
		AudioMode:           "copy",
		SubtitleMode:        "preserve",
		PreserveMetadata:    true,
		PreserveChapters:    true,
		PreserveAttachments: true,
		RecipeVersion:       "test",
		RecipeDigest:        "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	if err := preserveSourceBitDepth(plan, 10, true); err != nil {
		t.Fatal(err)
	}
	if plan.VideoProfile != "main10" || plan.PixelFormat != "p010le" || plan.ExpectedBitDepth != 10 || plan.PlanDigest == "" {
		t.Fatalf("10-bit source was not materialized safely: %+v", plan)
	}

	downgrade := *plan
	downgrade.VideoProfile = "main"
	downgrade.PixelFormat = "yuv420p"
	downgrade.ExpectedBitDepth = 8
	if err := preserveSourceBitDepth(&downgrade, 10, true); err != nil {
		t.Fatalf("default preservation should override the resolved downgrade: %v", err)
	}
	if downgrade.ExpectedBitDepth != 10 || downgrade.VideoProfile != "main10" || downgrade.PixelFormat != "p010le" {
		t.Fatalf("default preservation did not override the resolved downgrade: %+v", downgrade)
	}

	explicitDowngrade := *plan
	explicitDowngrade.VideoProfile = "main"
	explicitDowngrade.PixelFormat = "yuv420p"
	explicitDowngrade.ExpectedBitDepth = 8
	if err := preserveSourceBitDepth(&explicitDowngrade, 10, false); err != nil {
		t.Fatalf("explicit downgrade opt-in should retain the resolved plan: %v", err)
	}
	if explicitDowngrade.ExpectedBitDepth != 8 || explicitDowngrade.VideoProfile != "main" || explicitDowngrade.PixelFormat != "yuv420p" {
		t.Fatalf("explicit downgrade opt-in unexpectedly changed the plan: %+v", explicitDowngrade)
	}
}

func TestResolvePreserveSourceBitDepthRequiresBooleanOptOut(t *testing.T) {
	if got, err := resolvePreserveSourceBitDepth(nil); err != nil || !got {
		t.Fatalf("omitted preserve_source_bit_depth = %v, %v; want true", got, err)
	}
	if got, err := resolvePreserveSourceBitDepth(map[string]any{"preserve_source_bit_depth": false}); err != nil || got {
		t.Fatalf("explicit preserve_source_bit_depth=false = %v, %v", got, err)
	}
	if _, err := resolvePreserveSourceBitDepth(map[string]any{"preserve_source_bit_depth": "false"}); err == nil {
		t.Fatal("non-boolean preserve_source_bit_depth must fail")
	}
}
