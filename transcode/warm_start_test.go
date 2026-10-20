package transcode

import (
	"strings"
	"testing"
)

func TestWarmStartValidate(t *testing.T) {
	ok := WarmStart{QualityValue: 26, Basis: WarmStartSeriesSeason, Siblings: 3}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid warm start rejected: %v", err)
	}
	for name, mutate := range map[string]func(*WarmStart){
		"CRF 0":             func(w *WarmStart) { w.QualityValue = 0 },
		"CRF 52":            func(w *WarmStart) { w.QualityValue = 52 },
		"unknown basis":     func(w *WarmStart) { w.Basis = "release_group" },
		"no siblings":       func(w *WarmStart) { w.Siblings = 0 },
		"too many siblings": func(w *WarmStart) { w.Siblings = MaxWarmStartSiblings + 1 },
	} {
		w := ok
		mutate(&w)
		if w.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	clone := ok.Clone()
	clone.QualityValue = 20
	if ok.QualityValue != 26 {
		t.Fatal("Clone shares the warm start")
	}
	if (*WarmStart)(nil).Clone() != nil {
		t.Fatal("a nil warm start clones to a value")
	}
}

// A warm start only orders an automatic size v3 search's ladder; any other
// request carrying one is refused.
func TestWarmStartRequiresAnAutomaticSizeV3Search(t *testing.T) {
	ws := &WarmStart{QualityValue: 26, Basis: WarmStartFolder, Siblings: 1}
	for name, mutate := range map[string]func(*BenchmarkRequest){
		"size v3 without automatic search rules": func(r *BenchmarkRequest) {},
		"profile benchmark": func(r *BenchmarkRequest) {
			r.Mode, r.ModePolicyVersion, r.Allow8BitTo10Bit, r.AudioMode = "", "", false, ""
		},
	} {
		r := sizeV3TestRequest()
		mutate(&r)
		if err := ValidateBenchmarkRequest(&r); err != nil {
			t.Fatalf("%s without a warm start rejected: %v", name, err)
		}
		r.WarmStart = ws
		if err := ValidateBenchmarkRequest(&r); err == nil || !strings.Contains(err.Error(), "warm start") {
			t.Errorf("%s with a warm start: %v", name, err)
		}
	}
}
