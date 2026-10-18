package action

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
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
		{50, "hfr_quality_unavailable", ""},
		{59.94, "hfr_quality_unavailable", ""},
		{60, "hfr_quality_unavailable", ""},
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
	for _, order := range []string{"tt", "bb", "TB", "bt"} {
		interlaced := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: 50, FieldOrder: order}}}
		if modeSourceReason("size", interlaced, true) != "unsupported_source" {
			t.Fatalf("field-rate %s source decimated", order)
		}
	}
	interlaced25 := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: 25, FieldOrder: "tt"}}}
	progressive50 := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: 50, FieldOrder: "progressive"}}}
	if modeSourceReason("size", interlaced25, false) != "" || modeSourceReason("size", progressive50, true) != "" {
		t.Fatal("field order changed sources that need no decimation")
	}
	// Cover art is not a second video stream for the HFR gate.
	withCover := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: 50}, {Kind: "video", Codec: "mjpeg", Disposition: map[string]int{"attached_pic": 1}}}}
	if modeSourceReason("size", withCover, true) != "" || modeSourceReason("size", withCover, false) != "hfr_quality_unavailable" {
		t.Fatal("cover art changed the HFR gate")
	}
}

func hfrModeCapabilities() transcode.WorkerCapabilities {
	caps := modeTestCapabilities()
	caps.Quality.HFRTemporalDecimation = 2
	return caps
}

func TestHFRQualitySupportAsksOnlyForHFRAndReturnsLookupErrors(t *testing.T) {
	calls := 0
	mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		calls++
		return hfrModeCapabilities(), nil
	}}
	h := &hfrQualitySupport{e: &Engine{deps: EngineDeps{Transcode: mock}}}
	at := func(fps float64) *mediainspect.DetailedReport {
		return &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: fps}}}
	}
	ask := func(h *hfrQualitySupport, fps float64) (bool, error) {
		return h.forSource(context.Background(), at(fps))
	}
	if ok, err := ask(h, 25); ok || err != nil || calls != 0 {
		t.Fatalf("non-HFR source queried the worker (%d calls, %v)", calls, err)
	}
	if ok, err := ask(h, 120); ok || err != nil || calls != 0 {
		t.Fatalf("120 fps queried the worker or was admitted (%d calls, %v)", calls, err)
	}
	if ok, _ := ask(h, 50); !ok {
		t.Fatal("capable worker refused 50 fps")
	}
	if ok, _ := ask(h, 59.94); !ok || calls != 1 {
		t.Fatalf("HFR support not memoized (%d calls)", calls)
	}
	cover := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 50}, {Kind: "video", Codec: "png", Disposition: map[string]int{"attached_pic": 1}}}}
	if ok, err := h.forSource(context.Background(), cover); !ok || err != nil {
		t.Fatalf("HFR source with cover art not recognised: %v %v", ok, err)
	}

	old := &hfrQualitySupport{e: &Engine{deps: EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return modeTestCapabilities(), nil
	}}}}}
	if ok, err := ask(old, 50); ok || err != nil {
		t.Fatalf("worker without decimation capability admitted HFR: %v %v", ok, err)
	}

	failures := 1
	flaky := &hfrQualitySupport{e: &Engine{deps: EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		if failures > 0 {
			failures--
			return transcode.WorkerCapabilities{}, &httpclient.UncertainError{Op: "capabilities", Err: errors.New("timeout")}
		}
		return hfrModeCapabilities(), nil
	}}}}}
	if ok, err := ask(flaky, 50); ok || err == nil {
		t.Fatalf("lookup failure must be returned, not answered: %v %v", ok, err)
	}
	if ok, err := ask(flaky, 50); !ok || err != nil {
		t.Fatalf("a failed lookup was cached as unsupported: %v %v", ok, err)
	}
	if r := hfrCapabilityLookupResult(&httpclient.UncertainError{Op: "capabilities", Err: errors.New("timeout")}); r.Status != StepWaitingExternal || r.WaitingCondition != "worker_unreachable" {
		t.Fatalf("transient lookup failure must wait: %+v", r)
	}
	if r := hfrCapabilityLookupResult(&httpclient.HTTPError{StatusCode: 400, Message: "bad request"}); r.Status != StepFailed {
		t.Fatalf("permanent lookup failure must fail closed: %+v", r)
	}
}

const hfrProbeJSON = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"50/1","r_frame_rate":"50/1","field_order":"progressive","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}`

func writeHFRProbe(t *testing.T, dir string) string {
	t.Helper()
	probe := dir + "/ffprobe"
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ncat <<'JSON'\n"+hfrProbeJSON+"\nJSON\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return probe
}

func TestModePreflightHFRCapabilityWiring(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   func(context.Context) (transcode.WorkerCapabilities, error)
		status StepStatus
		reason string
	}{
		{"capable worker", func(context.Context) (transcode.WorkerCapabilities, error) { return hfrModeCapabilities(), nil }, StepCompleted, ""},
		{"older worker", func(context.Context) (transcode.WorkerCapabilities, error) { return modeTestCapabilities(), nil }, StepCompleted, "hfr_quality_unavailable"},
		{"transient lookup failure", func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{}, &httpclient.UncertainError{Op: "capabilities", Err: errors.New("timeout")}
		}, StepWaitingExternal, ""},
		{"permanent lookup failure", func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{}, &httpclient.HTTPError{StatusCode: 400, Message: "bad request"}
		}, StepFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := dir + "/hfr.mkv"
			if err := os.WriteFile(file, make([]byte, 100), 0600); err != nil {
				t.Fatal(err)
			}
			e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{capabilitiesFunc: tc.caps}, writeHFRProbe(t, dir), []string{dir}, []string{dir}, false)
			defer st.Close()
			ec := &ExecutionContext{InstanceID: "hfr-mode", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: map[string]any{}}
			seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
			result, err := e.stepTranscodePreflight(context.Background(), ec)
			if err != nil || result.Status != tc.status {
				t.Fatalf("preflight %+v %v", result, err)
			}
			if tc.status != StepCompleted {
				if getBool(ec.State, "skip_transcode") {
					t.Fatal("a failed capability lookup skipped the source")
				}
				return
			}
			if got := getString(result.Outputs, "reason_code"); got != tc.reason || getBool(ec.State, "skip_transcode") != (tc.reason != "") {
				t.Fatalf("reason %q skip=%v, want %q", got, getBool(ec.State, "skip_transcode"), tc.reason)
			}
			if tc.reason == "" && getPlan(ec.State["plan"]) == nil {
				t.Fatal("capable worker did not get an HFR plan")
			}
		})
	}
}

func TestModeBatchHFRCapabilityWiring(t *testing.T) {
	dir := t.TempDir()
	probe := writeHFRProbe(t, dir)
	paths := []string{dir + "/hfr-a.mkv", dir + "/hfr-b.mkv"}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("source "+path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lookups := 0
	transient := true
	executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		lookups++
		if transient {
			return transcode.WorkerCapabilities{}, &httpclient.UncertainError{Op: "capabilities", Err: errors.New("timeout")}
		}
		return modeTestCapabilities(), nil
	}}
	e, st := setupTranscodeEngine(t, executor, probe, []string{dir}, []string{dir}, true)
	defer st.Close()
	result, err := e.Run(context.Background(), "transcode_batch", map[string]any{"paths": paths, "mode": "size"})
	if err != nil || result.Status != StatusWaitingExternal {
		t.Fatalf("transient lookup must wait, not finish the batch: %+v %v", result, err)
	}
	if items, _ := st.ListTranscodeBatchItems(result.ID); len(items) != 0 {
		t.Fatalf("a failed lookup created skipped items: %+v", items)
	}

	transient, lookups = false, 0
	result, err = e.Run(context.Background(), "transcode_batch", map[string]any{"paths": paths, "mode": "size"})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("batch on an older worker %+v %v", result, err)
	}
	items, err := st.ListTranscodeBatchItems(result.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("manifest %d %v", len(items), err)
	}
	for _, item := range items {
		if item.Status != "skip" || len(item.Reasons) != 1 || item.Reasons[0] != "hfr_quality_unavailable" {
			t.Fatalf("HFR item on an older worker: %+v", item)
		}
	}
	if lookups != 1 || executor.benchmarkSubmitCalls != 0 {
		t.Fatalf("capabilities asked %d times, %d benchmarks", lookups, executor.benchmarkSubmitCalls)
	}
}

// The coordinator must never dispatch an HFR benchmark that the worker's own
// gate would reject, and must keep rejecting rates above the HFR class.
func TestModeBenchmarkRequestHFRMatchesWorkerCapability(t *testing.T) {
	hfrCaps := hfrModeCapabilities()
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
