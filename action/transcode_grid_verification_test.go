package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func gridTestCapabilities() transcode.WorkerCapabilities {
	caps := rulesTestCapabilities()
	caps.AutomaticGridVerification = true
	return caps
}

// gridEraPolicy is a size v3 snapshot as it was frozen while the holdouts sat
// on the grid edges: the same policy without the grid verification rules.
func gridEraPolicy(t *testing.T, hardware *transcode.HardwareSearchRules) ModePolicySnapshot {
	t.Helper()
	p, err := modePolicyWithHardware("size", false, 0, hardware)
	if err != nil {
		t.Fatal(err)
	}
	size := *p.Size
	rules := *size.AutomaticSearch
	rules.HoldoutWindows = transcode.HoldoutWindowsFinalGridEdges
	rules.VerifyUntilExhausted = false
	rules.FinalFreshWindows = 0
	size.AutomaticSearch = &rules
	p.Size = &size
	p.Digest = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

func TestSizePolicyFreezesGridVerification(t *testing.T) {
	p, err := modePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Size.AutomaticSearch
	if r.HoldoutWindows != transcode.HoldoutWindowsFinalGrid || !r.VerifyUntilExhausted || r.FinalFreshWindows != 2 || r.SelectionMarginVMAF != 1.5 || r.Validate() != nil {
		t.Fatalf("rules %+v", r)
	}
	// The quality limits and the ladder are untouched.
	v := p.Profile.Optimization.Quality.VMAF
	if v.Minimum != 88 || *v.P5Minimum != 84 || *v.WorstWindowMinimum != 85 || p.MinSavingsPercent != 15 {
		t.Fatalf("limits changed: %+v", v)
	}
}

// Snapshots frozen while the holdouts sat on the grid edges re-hash to the
// digests they were frozen under and keep their two-attempt search; only new
// snapshots verify on the grid.
func TestGridEraSnapshotsKeepTheirDigestAndSearch(t *testing.T) {
	earlier := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	current, err := modeHardwareInput("size", map[string]any{"mode": "size"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hardware *transcode.HardwareSearchRules
		digest   string
	}{
		"no media engine":           {nil, "sha256:33812f4b39f3aff6852beb34dc5ceffdaeb3504f71d00cd17555c89fbf4c80ee"},
		"media engine":              {current, "sha256:1b73a704427a9c6a89a9ae36079422c842de9cbd799fa48f634d54d1a3b7ead6"},
		"media engine, earlier set": {earlier, "sha256:bc2e370bf9f26965f7faa121693c299d1fe2e6c2d02b37aee033dc369cb12e03"},
	} {
		legacy := gridEraPolicy(t, tc.hardware)
		if legacy.Digest != tc.digest {
			t.Fatalf("%s: grid-era digest %s, want %s", name, legacy.Digest, tc.digest)
		}
		fresh, _ := modePolicyWithHardware("size", false, 0, tc.hardware)
		if fresh.Digest == legacy.Digest {
			t.Fatalf("%s: new snapshots share the grid-era digest", name)
		}
		stored := map[string]any{}
		raw, _ := json.Marshal(legacy)
		_ = json.Unmarshal(raw, &stored)
		ec := &ExecutionContext{InstanceID: "grid-era", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored, "original_sha256": strings.Repeat("a", 64)}}
		frozen, err := freezeModePolicy(ec)
		if err != nil || frozen.Digest != tc.digest {
			t.Fatalf("%s: stored snapshot %+v %v", name, frozen, err)
		}
		// Even a worker that verifies on the grid runs the frozen rules.
		families := policyAutomaticFallbacks(frozen, gridTestCapabilities())
		if families.Rules == nil || families.Rules.HoldoutWindows != transcode.HoldoutWindowsFinalGridEdges || families.Rules.VerifyUntilExhausted || families.Rules.FinalFreshWindows != 0 {
			t.Fatalf("%s: grid-era rules changed: %+v", name, families.Rules)
		}
		req := hardwareRequestWith(t, ec, 8, 45*60, gridTestCapabilities())
		if len(req.Samples) != 5 || req.AutomaticSearch.HoldoutWindows != transcode.HoldoutWindowsFinalGridEdges {
			t.Fatalf("%s: request %+v %+v", name, req.Samples, req.AutomaticSearch)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGridVerificationRequest(t *testing.T) {
	for _, tc := range []struct {
		duration float64
		windows  int
	}{{45 * 60, 5}, {105 * 60, 11}, {180 * 60, 12}, {101, 5}} {
		req := sizeRulesRequest(t, tc.duration, gridTestCapabilities())
		r := req.AutomaticSearch
		if r == nil || r.HoldoutWindows != transcode.HoldoutWindowsFinalGrid || !r.VerifyUntilExhausted || r.FinalFreshWindows != 2 {
			t.Fatalf("%.0f s: rules %+v", tc.duration, r)
		}
		if err := transcode.ValidateBenchmarkRequest(req); err != nil {
			t.Fatalf("%.0f s: %v", tc.duration, err)
		}
		if len(req.Samples) != tc.windows {
			t.Fatalf("%.0f s: %d windows, want %d", tc.duration, len(req.Samples), tc.windows)
		}
		// The verification windows are exactly the windows final validation will
		// re-measure on the full encode.
		grid := transcode.PlanFinalValidationWindows(tc.duration, 10, 600, 5, 12)
		for _, w := range grid {
			found := false
			for _, s := range req.Samples {
				if math.Abs(s.StartSeconds-w.StartSeconds) < 1e-9 && s.DurationSeconds == w.DurationSeconds {
					found = true
				}
			}
			if !found {
				t.Fatalf("%.0f s: final window at %.1f s is not a search window", tc.duration, w.StartSeconds)
			}
		}
		// Discovery windows are the interior ones nearest 20/50/80%.
		for i, position := range []float64{0.2, 0.5, 0.8} {
			if math.Abs(req.Samples[i].CenterSeconds-tc.duration*position) > tc.duration/float64(len(grid))/2+1e-6 {
				t.Fatalf("%.0f s: discovery window %d at %.1f", tc.duration, i, req.Samples[i].CenterSeconds)
			}
		}
	}
}

// Without the capability the worker would reject the new rule fields and the
// new holdout value, so it gets the grid-edge request it understands.
func TestGridVerificationNeedsWorkerCapability(t *testing.T) {
	req := sizeRulesRequest(t, 45*60, rulesTestCapabilities())
	r := req.AutomaticSearch
	if r == nil || r.HoldoutWindows != transcode.HoldoutWindowsFinalGridEdges || r.VerifyUntilExhausted || r.FinalFreshWindows != 0 || len(req.Samples) != 5 {
		t.Fatalf("worker without grid verification: %+v %d windows", r, len(req.Samples))
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "verify_until_exhausted") || strings.Contains(string(b), "final_fresh") || strings.Contains(string(b), `"final_grid"`) {
		t.Fatalf("an older worker would see what it rejects: %s", b)
	}
	// A source too short for the grid keeps the earliest holdouts.
	short := sizeRulesRequest(t, 80, gridTestCapabilities())
	if short.AutomaticSearch == nil || short.AutomaticSearch.HoldoutWindows != "" || short.AutomaticSearch.FinalFreshWindows != 0 || len(short.Samples) != 5 {
		t.Fatalf("short source: %+v %d windows", short.AutomaticSearch, len(short.Samples))
	}
	if err := transcode.ValidateBenchmarkRequest(short); err != nil {
		t.Fatal(err)
	}
}

func TestGridVerificationRulesValidation(t *testing.T) {
	req := sizeRulesRequest(t, 105*60, gridTestCapabilities())
	if len(req.Samples) != 11 {
		t.Fatalf("%d windows", len(req.Samples))
	}
	for name, mutate := range map[string]func(*transcode.BenchmarkRequest){
		"too many fresh windows": func(r *transcode.BenchmarkRequest) { r.AutomaticSearch.FinalFreshWindows = 3 },
		"negative fresh windows": func(r *transcode.BenchmarkRequest) { r.AutomaticSearch.FinalFreshWindows = -1 },
		"four windows":           func(r *transcode.BenchmarkRequest) { r.Samples = r.Samples[:4] },
		"edges with a grid": func(r *transcode.BenchmarkRequest) {
			r.AutomaticSearch.HoldoutWindows = transcode.HoldoutWindowsFinalGridEdges
		},
		"no rules with a grid": func(r *transcode.BenchmarkRequest) { r.AutomaticSearch = nil },
		"thirteen windows": func(r *transcode.BenchmarkRequest) {
			r.Samples = append(r.Samples, transcode.BenchmarkSampleWindow{Index: 11, StartSeconds: 10, DurationSeconds: 5, CenterSeconds: 12.5}, transcode.BenchmarkSampleWindow{Index: 12, StartSeconds: 20, DurationSeconds: 5, CenterSeconds: 22.5})
		},
	} {
		clone := *req
		clone.AutomaticSearch = req.AutomaticSearch.Clone()
		clone.Samples = append([]transcode.BenchmarkSampleWindow(nil), req.Samples...)
		mutate(&clone)
		if err := transcode.ValidateBenchmarkRequest(&clone); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFinalValidationFreshWindows(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "fresh", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	grid, fresh := sizeFinalWindowsWithFresh(ec, 3700, 2)
	if len(grid) != 7 || len(fresh) != 2 {
		t.Fatalf("grid %d fresh %d", len(grid), len(fresh))
	}
	for _, f := range fresh {
		for _, g := range grid {
			if f.StartSeconds < g.StartSeconds+g.DurationSeconds && g.StartSeconds < f.StartSeconds+f.DurationSeconds {
				t.Fatalf("fresh window %+v overlaps grid window %+v", f, g)
			}
		}
		if f.Index < len(grid) {
			t.Fatalf("fresh window reuses a grid index: %+v", f)
		}
	}
	if _, none := sizeFinalWindowsWithFresh(ec, 3700, 0); none != nil {
		t.Fatalf("fresh windows without the rule: %+v", none)
	}
}
