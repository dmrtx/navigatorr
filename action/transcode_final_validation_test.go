package action

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// finalRequestForPlan builds the final validation request for the encoder the
// state's plan selected.
func finalRequestForPlan(ec *ExecutionContext, cleanPath string, rep *mediainspect.DetailedReport, opt *recipe.OptimizationPolicy, caps transcode.WorkerCapabilities) (*transcode.BenchmarkRequest, error) {
	codec := ""
	if plan := getPlan(ec.State["plan"]); plan != nil {
		codec = plan.VideoCodec
	}
	return buildFinalValidationRequest(ec, cleanPath, rep, opt, caps, codec)
}

// A missing or unreadable resolved plan cannot silently degrade the request to
// VideoToolbox. The encoder must come from the plan (fail closed).
func TestFinalValidationRequestFailsClosedWithoutResolvedPlan(t *testing.T) {
	rep := &mediainspect.DetailedReport{
		DurationSec: 100,
		Video:       []mediainspect.DetailedStream{{BitDepth: 8, BitRate: 5000000}},
	}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 3, Positions: []float64{0.2, 0.5, 0.8}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "vmaf"},
	}

	cases := []struct {
		name  string
		state map[string]any
	}{
		{name: "missing plan", state: map[string]any{}},
		{name: "unreadable plan", state: map[string]any{"plan": make(chan int)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec := &ExecutionContext{InstanceID: "bench-test", State: tc.state}
			req, err := finalRequestForPlan(ec, "/media/source.mkv", rep, opt, transcode.WorkerCapabilities{})
			if err == nil {
				t.Fatalf("expected fail-closed error, got request %+v", req)
			}
			if !strings.Contains(err.Error(), "resolved plan is missing or unreadable") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestFinalValidationRequestRejectsNativeMain10CAMBI(t *testing.T) {
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 10, BitRate: 5000000, FPS: 24}}}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 3, Positions: []float64{0.2, 0.5, 0.8}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "ssim", Banding: &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "observe"}},
	}
	ec := &ExecutionContext{InstanceID: "bench-main10-cambi", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, Quality: 65}}}
	_, err := finalRequestForPlan(ec, "/media/source.mkv", rep, opt, transcode.WorkerCapabilities{})
	if err == nil || !strings.Contains(err.Error(), "quality_cambi_native_main10_unverified") {
		t.Fatalf("native Main10 CAMBI was not rejected: %v", err)
	}
}

func TestFinalValidationRequestMain10CompactAudio(t *testing.T) {
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 10, BitRate: 5000000, FPS: 24}}}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 3, Positions: []float64{0.2, 0.5, 0.8}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "vmaf", VMAF: &recipe.MetricTarget{Model: "v1_1080p_3h", Target: 96, Minimum: 95}, Banding: &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "observe"}},
	}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, CompactAudio: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"libvmaf": true, "ssim": true}, Quality: &transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}}
	ec := &ExecutionContext{InstanceID: "bench-main10-compact", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 23, AudioMode: "compact"}}}
	req, err := finalRequestForPlan(ec, "/media/source.mkv", rep, opt, caps)
	if err != nil {
		t.Fatal(err)
	}
	if req.Metric != "vmaf" || len(req.Samples) == 0 {
		t.Fatalf("unexpected request: %+v", req)
	}
	caps.CompactAudio = false
	if _, err := finalRequestForPlan(ec, "/media/source.mkv", rep, opt, caps); err == nil || !strings.Contains(err.Error(), "compact audio") {
		t.Fatalf("old worker accepted audio: %v", err)
	}
	caps.Quality.Native10Bit = false
	if err := validateWorkerCapabilitiesForFinalValidation(caps, "vmaf", 10, "libx265"); err == nil {
		t.Fatal("unverified native VMAF accepted")
	}
}

// A container that runs on past its video (a subtitle or attachment carries
// its duration) gets its windows on the video, where there are pictures to
// extract; the worker refuses any window past the video before encoding.
func TestFinalValidationWindowsStayInsideTheVideo(t *testing.T) {
	rep := &mediainspect.DetailedReport{DurationSec: 2115.59, VideoSeconds: 1855.87, Video: []mediainspect.DetailedStream{{BitDepth: 8, BitRate: 2000000, FPS: 23.976}}}
	opt := &recipe.OptimizationPolicy{
		Enabled:  true,
		Sampling: &recipe.SamplingPolicy{SampleSeconds: 5, SampleCount: 3, Positions: []float64{0.2, 0.5, 0.95}},
		Quality:  &recipe.QualityPolicy{PreferredMetric: "ssim"},
	}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"ssim": true}}
	ec := &ExecutionContext{InstanceID: "bench-video-shorter", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 23}}}
	req, err := finalRequestForPlan(ec, "/media/source.mkv", rep, opt, caps)
	if err != nil {
		t.Fatal(err)
	}
	if req.SourceDuration != 1855.87 {
		t.Fatalf("source duration = %v, want the video's 1855.87", req.SourceDuration)
	}
	for _, w := range req.Samples {
		if w.StartSeconds+w.DurationSeconds > rep.VideoSeconds {
			t.Fatalf("window %d ends at %.1fs, past the video's end", w.Index, w.StartSeconds+w.DurationSeconds)
		}
	}
	rep.VideoSeconds = 0
	if req, err = finalRequestForPlan(ec, "/media/source.mkv", rep, opt, caps); err != nil || req.SourceDuration != 2115.59 {
		t.Fatalf("unmeasured video: duration %v, err %v; want the container's", req.SourceDuration, err)
	}
}
