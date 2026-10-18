package action

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestModeSourceReasonFrameRateGate(t *testing.T) {
	for _, tc := range []struct {
		fps        float64
		withoutHFR string
		withHFR    string
	}{
		{0, "unsupported_source", "unsupported_source"},
		{25, "", ""},
		{30, "", ""},
		{50, "unsupported_source", ""},
		{59.94, "unsupported_source", ""},
		{60, "unsupported_source", ""},
		{120, "unsupported_source", "unsupported_source"},
	} {
		rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: tc.fps}}}
		if got := modeSourceReason("size", rep, false); got != tc.withoutHFR {
			t.Fatalf("%.3f fps without HFR capability: %q", tc.fps, got)
		}
		if got := modeSourceReason("size", rep, true); got != tc.withHFR {
			t.Fatalf("%.3f fps with HFR capability: %q", tc.fps, got)
		}
	}
	hdr := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 10, FPS: 50, ColorTransfer: "smpte2084"}}}
	if modeSourceReason("size", hdr, true) != "unsupported_source" {
		t.Fatal("HFR capability relaxed the HDR gate")
	}
	deep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 12, FPS: 50}}}
	if modeSourceReason("size", deep, true) != "unsupported_source" {
		t.Fatal("HFR capability relaxed the bit-depth gate")
	}
}

func TestHFRQualitySupportAsksOnlyForHFRAndFailsClosed(t *testing.T) {
	calls := 0
	hfrCaps := modeTestCapabilities()
	hfrCaps.Quality.HFRTemporalDecimation = 2
	mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		calls++
		return hfrCaps, nil
	}}
	h := &hfrQualitySupport{e: &Engine{deps: EngineDeps{Transcode: mock}}}
	at := func(fps float64) *mediainspect.DetailedReport {
		return &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: fps}}}
	}
	if h.forSource(context.Background(), at(25)) || calls != 0 {
		t.Fatalf("non-HFR source queried the worker (%d calls)", calls)
	}
	if !h.forSource(context.Background(), at(50)) || !h.forSource(context.Background(), at(59.94)) || calls != 1 {
		t.Fatalf("HFR support not memoized (%d calls)", calls)
	}
	if h.forSource(context.Background(), at(120)) {
		t.Fatal("120 fps reported as supported HFR")
	}

	old := &hfrQualitySupport{e: &Engine{deps: EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return modeTestCapabilities(), nil
	}}}}}
	if old.forSource(context.Background(), at(50)) {
		t.Fatal("worker without decimation capability admitted HFR")
	}
	down := &hfrQualitySupport{e: &Engine{deps: EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, errors.New("worker unreachable")
	}}}}}
	if down.forSource(context.Background(), at(50)) {
		t.Fatal("capability error did not fail closed")
	}
}

// The coordinator must never dispatch an HFR benchmark that the worker's own
// gate would reject, and must keep rejecting rates above the HFR class.
func TestModeBenchmarkRequestHFRMatchesWorkerCapability(t *testing.T) {
	hfrCaps := modeTestCapabilities()
	hfrCaps.Quality.HFRTemporalDecimation = 2
	for _, tc := range []struct {
		fps  float64
		caps transcode.WorkerCapabilities
		ok   bool
	}{
		{25, modeTestCapabilities(), true},
		{50, modeTestCapabilities(), false},
		{50, hfrCaps, true},
		{59.94, hfrCaps, true},
		{60, hfrCaps, true},
		{120, hfrCaps, false},
	} {
		ec := &ExecutionContext{InstanceID: "mode-hfr", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
		policy, err := freezeModePolicy(ec)
		if err != nil {
			t.Fatal(err)
		}
		plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8}
		if err := preserveSourceBitDepth(plan, 8, true); err != nil {
			t.Fatal(err)
		}
		ec.State["plan"] = plan
		rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: tc.fps, BitRate: 5000000}}}
		_, err = buildBenchmarkRequest(ec, "/media/source.mkv", rep, policy.Profile.Optimization, tc.caps)
		if (err == nil) != tc.ok {
			t.Fatalf("%.3f fps HFR=%d: err %v", tc.fps, tc.caps.Quality.HFRTemporalDecimation, err)
		}
		if err != nil && !strings.Contains(err.Error(), "quality_vmaf_model_unavailable") {
			t.Fatalf("%.3f fps: unexpected rejection %v", tc.fps, err)
		}
	}
}
