package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestSizeModePolicyV3Snapshot(t *testing.T) {
	p, err := legacyModePolicy("size", false)
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
	// TestGridEraSnapshotsKeepTheirDigestAndSearch). Dropping the 1.5 selection
	// margin for the searches that verify on the grid gave the digest
	// sha256:5ca0b1d8...0976; snapshots frozen with the margin still re-hash to
	// sha256:6e519947...8ba8 (see TestMarginEraSnapshotsKeepTheirDigestAndMargin).
	// The x265 quality walk (CRF 14-21 at medium, slow only on request,
	// refinement rungs, banding measured after VMAF, no operation budget) gave
	// the digest below; snapshots frozen without the margin but before it
	// re-hash to sha256:5ca0b1d8...0976 (see
	// TestPreWalkSnapshotsKeepTheirDigestAndSearch).
	if p.Digest != "sha256:cb53779c2cbdfe1d2f3ef41b16a2c986ddac436f8b19f4801cb4cd4e830b825c" {
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
		other, err := legacyModePolicy(mode, false)
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
	if grid, _ := sizeFinalWindowsWithFresh(ec, 6300, 0); grid != nil {
		t.Fatal("v2 policy planned v3 final windows")
	}
}

func TestSizeNeedsUpdatedWorker(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "mode-v3", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	caps := modeTestCapabilities()
	caps.SizeModeV3 = false
	rep := &mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
	if _, err := finalRequestForPlan(ec, "/media/source.mkv", rep, policy.Profile.Optimization, caps); err == nil || !strings.Contains(err.Error(), "lossless audio compaction") {
		t.Fatalf("old worker admitted the size goal: %v", err)
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
		grid, _ := sizeFinalWindowsWithFresh(ec, tc.duration, 0)
		if got := len(grid); got != tc.count {
			t.Fatalf("%.0f s: %d windows, want %d", tc.duration, got, tc.count)
		}
	}
	quality := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	if grid, _ := sizeFinalWindowsWithFresh(quality, 6300, 0); grid != nil {
		t.Fatal("quality mode planned size windows")
	}
}

// Preflight encodes an 8-bit source as Main10 only on a proven worker; a
// worker without native 10-bit quality keeps the source depth.
func TestSizePreflightMain10Decision(t *testing.T) {
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
			var submitted transcode.Request
			executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return caps, nil }, submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
				submitted = req
				return transcode.Job{ID: req.ID}, nil
			}}
			e, st := setupTranscodeEngine(t, executor, probe, []string{dir}, []string{dir}, true)
			defer st.Close()
			if _, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": source, "mode": "size", "allow_hardware": false}); err != nil {
				t.Fatal(err)
			}
			if submitted.Plan == nil {
				t.Fatal("no encode submitted")
			}
			want := map[int]string{8: "main", 10: "main10"}[tc.depth]
			if submitted.Plan.VideoProfile != want || submitted.Plan.ExpectedBitDepth != tc.depth {
				t.Fatalf("plan %+v, want %s", submitted.Plan, want)
			}
		})
	}
}
