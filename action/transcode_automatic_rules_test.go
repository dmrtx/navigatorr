package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// preRulesSizeV3Digest is the size v3 digest from before automatic search
// rules existed (search extension with its verification allowance).
const preRulesSizeV3Digest = "sha256:7239d124b7e3d2f00bed145aba8b4d2bf1aab66ad93a6da964a3281a153743ad"

func rulesTestCapabilities() transcode.WorkerCapabilities {
	caps := modeTestCapabilities()
	caps.AutomaticSearchRules = true
	return caps
}

func TestSizeModePolicyV3FreezesAutomaticSearchRules(t *testing.T) {
	p, err := modePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	rules := p.Size.AutomaticSearch
	if rules == nil || rules.SelectionMarginVMAF != 0 || rules.HoldoutWindows != transcode.HoldoutWindowsFinalGrid || !rules.VerifyUntilExhausted || rules.FinalFreshWindows != 2 || strings.Join(rules.QualityFallbackPresets, ",") != "medium,slow" || rules.Validate() != nil {
		t.Fatalf("size policy rules %+v", rules)
	}
	// The thresholds themselves are unchanged.
	v := p.Profile.Optimization.Quality.VMAF
	if v.Minimum != 88 || *v.P5Minimum != 84 || *v.WorstWindowMinimum != 85 || p.MinSavingsPercent != 15 {
		t.Fatalf("thresholds changed: %+v %v", v, p.MinSavingsPercent)
	}

	// A snapshot frozen before the rules keeps its digest and gets no rules.
	legacy := p
	size := *p.Size
	size.AutomaticSearch = nil
	legacy.Size = &size
	legacy.Digest = ""
	b, _ := json.Marshal(legacy)
	sum := sha256.Sum256(b)
	legacy.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if legacy.Digest != preRulesSizeV3Digest || legacy.Digest == p.Digest || legacy.Version != p.Version {
		t.Fatalf("pre-rules snapshot digest %s (new %s)", legacy.Digest, p.Digest)
	}
	ec := &ExecutionContext{InstanceID: "legacy-rules", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": legacy}}
	frozen, err := freezeModePolicy(ec)
	if err != nil || frozen.Size.AutomaticSearch != nil {
		t.Fatalf("pre-rules snapshot: %+v %v", frozen, err)
	}
	if f := policyAutomaticFallbacks(frozen, rulesTestCapabilities()); f.Rules != nil {
		t.Fatalf("pre-rules snapshot gained rules: %+v", f.Rules)
	}
	// A stored (JSON round-tripped) new snapshot verifies with its rules.
	stored := map[string]any{}
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &stored)
	ec = &ExecutionContext{InstanceID: "new-rules", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}
	if frozen, err = freezeModePolicy(ec); err != nil || frozen.Size.AutomaticSearch == nil {
		t.Fatalf("stored rules policy: %+v %v", frozen, err)
	}
	// Rules go only to workers that advertise them.
	if f := policyAutomaticFallbacks(frozen, modeTestCapabilities()); f.Rules != nil {
		t.Fatal("rules sent to a worker without automatic_search_rules")
	}
}

func sizeRulesRequest(t *testing.T, duration float64, caps transcode.WorkerCapabilities) *transcode.BenchmarkRequest {
	t.Helper()
	ec := &ExecutionContext{InstanceID: "mode-rules", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	if err := preserveSourceBitDepth(plan, 8, true); err != nil {
		t.Fatal(err)
	}
	if err := setMain10(plan); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = plan
	rep := &mediainspect.DetailedReport{DurationSec: duration, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 23.976, BitRate: 10000000}}}
	req, err := buildBenchmarkRequest(ec, "/media/show/show S01E04.mkv", rep, policy.Profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// A worker with the rules gets a medium quality fallback ahead of the slow
// one and holdout windows on the edges of the final validation grid, so the
// start and end of the source are checked before a full encode is spent.
func TestSizeV3RulesRequest(t *testing.T) {
	for _, tc := range []struct {
		duration      float64
		first, last   float64
		gridPositions int
	}{{45 * 60, 270, 2430, 5}, {105 * 60, 6300.0 / 22, 6300 - 6300.0/22, 11}, {180 * 60, 450, 10350, 12}} {
		req := sizeRulesRequest(t, tc.duration, rulesTestCapabilities())
		if req.AutomaticSearch == nil || req.AutomaticSearch.SelectionMarginVMAF != 1.5 {
			t.Fatalf("rules not sent: %+v", req.AutomaticSearch)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatal(err)
		}
		var ladder []string
		for _, c := range req.Candidates {
			ladder = append(ladder, fmt.Sprintf("%s:%s", c.ID, c.Preset))
		}
		want := "cand_crf22:medium,cand_crf24:medium,cand_crf26:medium,cand_crf28:medium,cand_crf30:medium," +
			"fallback-quality-medium-cand_crf16:medium,fallback-quality-medium-cand_crf18:medium,fallback-quality-medium-cand_crf20:medium," +
			"fallback-quality-cand_crf16:slow,fallback-quality-cand_crf18:slow,fallback-quality-cand_crf20:slow," +
			"fallback-savings-cand_crf24:slow,fallback-savings-cand_crf27:slow,fallback-savings-cand_crf30:slow"
		if got := strings.Join(ladder, ","); got != want {
			t.Fatalf("ladder\n got %s\nwant %s", got, want)
		}
		if len(req.Samples) != 5 {
			t.Fatalf("samples %+v", req.Samples)
		}
		grid := transcode.PlanFinalValidationWindows(tc.duration, 10, 600, 5, 12)
		if len(grid) != tc.gridPositions {
			t.Fatalf("grid %d positions", len(grid))
		}
		h1, h2 := req.Samples[3], req.Samples[4]
		if math.Abs(h1.CenterSeconds-grid[0].CenterSeconds) > 1e-9 || math.Abs(h2.CenterSeconds-grid[len(grid)-1].CenterSeconds) > 1e-9 ||
			math.Abs(h1.CenterSeconds-tc.first) > 1e-6 || math.Abs(h2.CenterSeconds-tc.last) > 1e-6 {
			t.Fatalf("%.0f s: holdouts at %.1f/%.1f, grid edges %.1f/%.1f", tc.duration, h1.CenterSeconds, h2.CenterSeconds, grid[0].CenterSeconds, grid[len(grid)-1].CenterSeconds)
		}
		// Discovery stays at 20/50/80%.
		for i, position := range []float64{0.2, 0.5, 0.8} {
			if math.Abs(req.Samples[i].CenterSeconds-tc.duration*position) > 1 {
				t.Fatalf("discovery window %d moved: %+v", i, req.Samples[i])
			}
		}
	}
}

// Older workers, and sources too short for the final grid, keep the earlier
// request: eleven candidates and holdouts at 35% and 65%.
func TestSizeV3RulesFallBackToEarlierRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		duration float64
		caps     transcode.WorkerCapabilities
		rules    bool
	}{"old worker": {45 * 60, modeTestCapabilities(), false}, "short source": {80, rulesTestCapabilities(), true}} {
		req := sizeRulesRequest(t, tc.duration, tc.caps)
		if (req.AutomaticSearch != nil) != tc.rules {
			t.Fatalf("%s: rules %+v", name, req.AutomaticSearch)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := map[bool]int{false: 11, true: 14}[tc.rules]; len(req.Candidates) != want {
			t.Fatalf("%s: %d candidates, want %d", name, len(req.Candidates), want)
		}
		if math.Abs(req.Samples[3].CenterSeconds-tc.duration*0.35) > 1e-9 || math.Abs(req.Samples[4].CenterSeconds-tc.duration*0.65) > 1e-9 {
			t.Fatalf("%s: holdouts moved: %+v", name, req.Samples[3:])
		}
	}
}

func TestAutomaticSearchRulesValidation(t *testing.T) {
	req := sizeRulesRequest(t, 45*60, rulesTestCapabilities())
	for name, mutate := range map[string]func(*transcode.BenchmarkRequest){
		"negative margin":  func(r *transcode.BenchmarkRequest) { r.AutomaticSearch.SelectionMarginVMAF = -1 },
		"huge margin":      func(r *transcode.BenchmarkRequest) { r.AutomaticSearch.SelectionMarginVMAF = 11 },
		"NaN margin":       func(r *transcode.BenchmarkRequest) { r.AutomaticSearch.SelectionMarginVMAF = math.NaN() },
		"unknown holdouts": func(r *transcode.BenchmarkRequest) { r.AutomaticSearch.HoldoutWindows = "random" },
		"duplicate preset": func(r *transcode.BenchmarkRequest) {
			r.AutomaticSearch.QualityFallbackPresets = []string{"slow", "slow"}
		},
		"not size v3":       func(r *transcode.BenchmarkRequest) { r.ModePolicyVersion = ModePolicyVersion; r.AudioMode = "" },
		"too many settings": func(r *transcode.BenchmarkRequest) { r.Candidates = append(r.Candidates, r.Candidates[0]) },
		"twelve without rules": func(r *transcode.BenchmarkRequest) {
			r.AutomaticSearch = nil
		},
	} {
		clone := *req
		clone.AutomaticSearch = req.AutomaticSearch.Clone()
		clone.Candidates = append([]transcode.BenchmarkCandidate(nil), req.Candidates...)
		mutate(&clone)
		if err := transcode.ValidateBenchmarkRequest(&clone); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
