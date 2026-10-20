package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestSizeModePolicyV3Snapshot(t *testing.T) {
	p, err := modePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != optimization.ModePolicyV3 || p.Size == nil || p.MinSavingsPercent != 15 || p.Profile.Audio.Mode != transcode.AudioModeCompactLossless {
		t.Fatalf("size v3 snapshot %+v", p)
	}
	if !reflect.DeepEqual(p.Profile.Optimization.Search.QualityValues, []int{22, 24, 26, 28, 30}) || !p.Size.EightBitMain10 || p.Size.VideoToolboxCandidates || !p.Size.AudioOnlyFallback {
		t.Fatalf("size v3 search %+v %+v", p.Profile.Optimization.Search, p.Size)
	}
	if s := p.Size; s.FinalWindowSeconds != 10 || s.FinalWindowEverySeconds != 600 || s.FinalWindowsMinimum != 5 || s.FinalWindowsMaximum != 12 {
		t.Fatalf("final windows %+v", s)
	}
	if !p.Size.LosslessObjectAudioToAAC {
		t.Fatalf("size v3 must freeze lossless object audio to AAC: %+v", p.Size)
	}
	// Pinned: any change to what v3 freezes must change this digest, so an
	// operation frozen under an earlier v3 snapshot never runs a new rule.
	// The previous v3 snapshot copied lossless Atmos/DTS:X (digest
	// sha256:0b92adbc...0177f) and was never deployed. Adding the search
	// extension gave new snapshots the digest below; snapshots frozen before it
	// have no such field and still re-hash to sha256:c7a08511...26ca3 (see
	// TestSizeModePolicyV3FreezesSearchExtension). Adding the automatic search
	// rules (selection margin, quality-first verification, grid-edge holdouts,
	// medium-first quality fallback) gave new snapshots the digest below;
	// snapshots frozen before them still re-hash to sha256:7239d124...743ad
	// (see TestSizeModePolicyV3FreezesAutomaticSearchRules). Verifying on the
	// whole final validation grid until a setting passes (with fresh final
	// windows) gave new snapshots the digest below; snapshots frozen with the
	// grid-edge holdouts still re-hash to sha256:33812f4b...80ee (see
	// TestGridEraSnapshotsKeepTheirDigestAndSearch).
	if p.Digest != "sha256:6e519947ac9039f9dbfe36a8beee2ebdd891f0a02d28b5e5b776ddc7f8928ba8" {
		t.Fatalf("size v3 snapshot digest changed: %s", p.Digest)
	}
	// The quality floor is v2's, unchanged: VMAF mean 88, p5 84, worst 5 s
	// window 85, full-reference CAMBI mean 6 / peak 16, sampled final check.
	floor, _ := json.Marshal(automaticBatchSearchProfile(false).Optimization.Quality)
	got, _ := json.Marshal(p.Profile.Optimization.Quality)
	if string(floor) != string(got) {
		t.Fatalf("size v3 changed the quality floor:\n%s\n%s", floor, got)
	}
	v := p.Profile.Optimization.Quality.VMAF
	b := p.Profile.Optimization.Quality.Banding
	if v.Minimum != 88 || *v.P5Minimum != 84 || *v.WorstWindowMinimum != 85 || v.WorstWindowSeconds != 5 || !b.Enabled || *b.MaxMean != 6 || *b.MaxPeak != 16 {
		t.Fatalf("floor %+v %+v", v, b)
	}
	for _, mode := range []string{"quality", "x265_preserve", "compact"} {
		other, err := modePolicy(mode, false)
		if err != nil || other.Version != ModePolicyVersion || other.Size != nil {
			t.Fatalf("%s policy changed: %+v %v", mode, other, err)
		}
	}
}

// v2SizeSnapshot is a size policy frozen before v3 shipped.
func v2SizeSnapshot(t *testing.T) ModePolicySnapshot {
	t.Helper()
	p := ModePolicySnapshot{Version: ModePolicyVersion, Mode: "size", MinSavingsPercent: 15, SearchBudgetSeconds: modeSearchBudgetSeconds, OperationBudgetSeconds: modeOperationBudgetSeconds, Profile: automaticBatchSearchProfile(false)}
	p.Profile.Optimization.Search.MaxCandidates = 12
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

func TestFrozenV2SizePolicyKeepsItsBehaviour(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "mode-v2", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64), "mode_policy": v2SizeSnapshot(t)}}
	policy, err := freezeModePolicy(ec)
	if err != nil || policy.Version != ModePolicyVersion || policy.Size != nil {
		t.Fatalf("frozen v2 policy did not verify: %+v %v", policy, err)
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	if err := preserveSourceBitDepth(plan, 8, true); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = plan
	rep := &mediainspect.DetailedReport{DurationSec: 6300, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
	caps := modeTestCapabilities()
	caps.SizeModeV3 = false
	req, err := buildBenchmarkRequest(ec, "/media/movies/Movie/Movie.mkv", rep, policy.Profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	vt := 0
	for _, c := range req.Candidates {
		if c.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
			vt++
		}
	}
	if req.ModePolicyVersion != ModePolicyVersion || len(req.Candidates) != 12 || vt != 3 || req.AudioMode != "" || req.Allow8BitTo10Bit {
		t.Fatalf("v2 request changed: %+v", req)
	}
	if sizeFinalWindows(ec, 6300) != nil {
		t.Fatal("v2 policy planned v3 final windows")
	}
}

// An 8-bit source with smooth gradients can fail CAMBI's peak at every
// 8-bit setting; size v3 evaluates the whole ladder in Main10 instead,
// with banding still enforced.
func TestSizeV3LadderIsMain10WithBanding(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "mode-v3", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	if err := preserveSourceBitDepth(plan, 8, true); err != nil {
		t.Fatal(err)
	}
	if !sizeMain10Supported(modeTestCapabilities()) {
		t.Fatal("proven worker refused Main10")
	}
	if err := setMain10(plan); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = plan
	rep := &mediainspect.DetailedReport{DurationSec: 6300, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 23.976, BitRate: 34500000}},
		Audio: []mediainspect.DetailedStream{{Index: 1, Codec: "dts", Profile: "DTS-HD MA", Channels: 6, ChannelLayout: "5.1(side)", Language: "jpn"}, {Index: 2, Codec: "dts", Profile: "DTS-HD MA", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng"}}}
	req, err := buildBenchmarkRequest(ec, "/media/movies/Movie/Movie.mkv", rep, policy.Profile.Optimization, modeTestCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if !req.Automatic || !req.Allow8BitTo10Bit || req.AudioMode != transcode.AudioModeCompactLossless || req.ModePolicyVersion != optimization.ModePolicyV3 {
		t.Fatalf("request %+v", req)
	}
	if req.Quality == nil || req.Quality.Banding == nil || !req.Quality.Banding.Enabled || req.Quality.Banding.MaxPeak == nil || *req.Quality.Banding.MaxPeak != 16 {
		t.Fatalf("banding not enforced: %+v", req.Quality)
	}
	var ladder []string
	for _, c := range req.Candidates {
		if c.VideoCodec != transcode.VideoCodecLibX265 || c.VideoProfile != "main10" || c.PixelFormat != "p010le" {
			t.Fatalf("candidate not x265 Main10: %+v", c)
		}
		ladder = append(ladder, fmt.Sprintf("%s:%s", c.ID, c.Preset))
	}
	want := "cand_crf22:medium,cand_crf24:medium,cand_crf26:medium,cand_crf28:medium,cand_crf30:medium," +
		"fallback-quality-cand_crf16:slow,fallback-quality-cand_crf18:slow,fallback-quality-cand_crf20:slow," +
		"fallback-savings-cand_crf24:slow,fallback-savings-cand_crf27:slow,fallback-savings-cand_crf30:slow"
	if got := strings.Join(ladder, ","); got != want {
		t.Fatalf("ladder\n got %s\nwant %s", got, want)
	}
}

func TestSizeV3NeedsUpdatedWorker(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "mode-v3", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	caps := modeTestCapabilities()
	caps.SizeModeV3 = false
	rep := &mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
	if _, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, policy.Profile.Optimization, caps); err == nil || !strings.Contains(err.Error(), "updated worker") {
		t.Fatalf("old worker admitted size v3: %v", err)
	}
}

func TestSizeMain10Supported(t *testing.T) {
	if !sizeMain10Supported(modeTestCapabilities()) {
		t.Fatal("proven worker refused")
	}
	for name, mutate := range map[string]func(*transcode.WorkerCapabilities){
		"no size v3":               func(c *transcode.WorkerCapabilities) { c.SizeModeV3 = false },
		"no explicit Main10":       func(c *transcode.WorkerCapabilities) { c.ExplicitMain10Benchmarks = false },
		"no native 10-bit quality": func(c *transcode.WorkerCapabilities) { c.Quality.Native10Bit = false },
		"no full-reference CAMBI":  func(c *transcode.WorkerCapabilities) { c.Quality.CAMBIFullRef = false },
		"quality probe failed":     func(c *transcode.WorkerCapabilities) { c.Quality.ProbeError = "probe failed" },
		"no libx265":               func(c *transcode.WorkerCapabilities) { delete(c.Encoders, transcode.VideoCodecLibX265) },
	} {
		caps := modeTestCapabilities()
		mutate(&caps)
		if sizeMain10Supported(caps) {
			t.Errorf("%s admitted Main10", name)
		}
	}
}

func TestSizeV3FinalWindowsScaleWithDuration(t *testing.T) {
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "size"}, State: map[string]any{}}
	for _, tc := range []struct {
		duration float64
		count    int
	}{{22 * 60, 5}, {105 * 60, 11}, {180 * 60, 12}, {60, 0}} {
		if got := len(sizeFinalWindows(ec, tc.duration)); got != tc.count {
			t.Fatalf("%.0f s: %d windows, want %d", tc.duration, got, tc.count)
		}
	}
	quality := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	if sizeFinalWindows(quality, 6300) != nil {
		t.Fatal("quality mode planned size windows")
	}
}

// Preflight encodes an 8-bit source as Main10 only on a proven worker; a
// worker without native 10-bit quality keeps the source depth.
func TestSizeV3PreflightMain10Decision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		native bool
		depth  int
	}{{"proven worker", true, 10}, {"unproven worker", false, 8}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := dir + "/ffprobe"
			source := dir + "/source.mkv"
			_ = os.WriteFile(source, make([]byte, 1000), 0600)
			_ = os.WriteFile(probe, []byte(`#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"1000"},"chapters":[]}
JSON
`), 0700)
			caps := modeTestCapabilities()
			caps.Quality.Native10Bit = tc.native
			var submitted transcode.BenchmarkRequest
			executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return caps, nil }, benchmarkSubmitFunc: func(_ context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
				submitted = req
				return transcode.BenchmarkJob{ID: req.ID}, nil
			}}
			e, st := setupTranscodeEngine(t, executor, probe, []string{dir}, []string{dir}, true)
			defer st.Close()
			if _, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": source, "mode": "size"}); err != nil {
				t.Fatal(err)
			}
			if len(submitted.Candidates) == 0 {
				t.Fatal("no benchmark submitted")
			}
			want := map[int]string{8: "main", 10: "main10"}[tc.depth]
			for _, c := range submitted.Candidates {
				if c.VideoProfile != want {
					t.Fatalf("candidate %+v, want %s", c, want)
				}
			}
			if submitted.Allow8BitTo10Bit != (tc.depth == 10) {
				t.Fatalf("allow 8->10 = %v", submitted.Allow8BitTo10Bit)
			}
		})
	}
}
