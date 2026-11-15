package action

import (
	"math"
	"testing"
)

func TestEncodeFamilyNamesTheEncoder(t *testing.T) {
	for codec, want := range map[string]string{"copy": "copy", "libx265": "x265", "hevc_videotoolbox": "hardware", "h264_videotoolbox": "hardware", "": "", "libx264": ""} {
		if got := EncodeFamily(codec); got != want {
			t.Errorf("EncodeFamily(%q) = %q, want %q", codec, got, want)
		}
	}
}

func sample(worker string, ratio float64) EncodeSample {
	return EncodeSample{Worker: worker, Class: "uhd", Family: "x265", DurationSec: 1000 * ratio, EncodeSec: 1000}
}

func TestEstimateEncodeSecondsUsesTheMedianRealtimeRatio(t *testing.T) {
	// Ratios 0.5, 1 and 4: the median 1 ignores the outlier a mean would chase.
	got, ok := EstimateEncodeSeconds([]EncodeSample{sample("a", 4), sample("a", 0.5), sample("a", 1)}, "a", "uhd", "x265", 7200)
	if !ok || math.Abs(got-7200) > 1e-6 {
		t.Fatalf("odd count: %v %v", got, ok)
	}
	// An even count averages the two middle ratios (0.5 and 1.5 -> 1).
	got, ok = EstimateEncodeSeconds([]EncodeSample{sample("a", 0.5), sample("a", 9), sample("a", 1.5), sample("a", 0.1)}, "a", "uhd", "x265", 3600)
	if !ok || math.Abs(got-3600/1.0) > 1e-6 {
		// sorted 0.1, 0.5, 1.5, 9 -> (0.5+1.5)/2
		t.Fatalf("even count: %v %v", got, ok)
	}
}

func TestEstimateEncodeSecondsFallsBackToAnyWorkerOnlyForTooFewOwnSamples(t *testing.T) {
	samples := []EncodeSample{sample("a", 2), sample("b", 0.5), sample("b", 0.5), sample("c", 0.5)}
	// One sample on a is too few: the three others set the pace (ratio 0.5).
	got, ok := EstimateEncodeSeconds(samples, "a", "uhd", "x265", 1000)
	if !ok || math.Abs(got-2000) > 1e-6 {
		t.Fatalf("fallback: %v %v", got, ok)
	}
	// Two own samples win over the rest.
	samples = append([]EncodeSample{sample("a", 2)}, samples...)
	got, ok = EstimateEncodeSeconds(samples, "a", "uhd", "x265", 1000)
	if !ok || math.Abs(got-500) > 1e-6 {
		t.Fatalf("own samples: %v %v", got, ok)
	}
}

func TestEstimateEncodeSecondsNeedsEnoughMatchingGoodSamples(t *testing.T) {
	good := sample("a", 1)
	other := []EncodeSample{
		{Worker: "a", Class: "hd", Family: "x265", DurationSec: 1000, EncodeSec: 1000},
		{Worker: "a", Class: "uhd", Family: "hardware", DurationSec: 1000, EncodeSec: 1000},
		{Worker: "a", Class: "uhd", Family: "x265", DurationSec: 0, EncodeSec: 1000},
		{Worker: "a", Class: "uhd", Family: "x265", DurationSec: 1000, EncodeSec: 0},
		{Worker: "a", Class: "uhd", Family: "x265", DurationSec: -1, EncodeSec: -1},
	}
	if _, ok := EstimateEncodeSeconds(append([]EncodeSample{good}, other...), "a", "uhd", "x265", 1000); ok {
		t.Fatal("one usable sample must not make an estimate")
	}
	if _, ok := EstimateEncodeSeconds(nil, "a", "uhd", "x265", 1000); ok {
		t.Fatal("no history must not make an estimate")
	}
	two := []EncodeSample{good, good}
	for name, args := range map[string][3]any{"no duration": {"uhd", "x265", 0.0}, "no class": {"", "x265", 1000.0}, "no family": {"uhd", "", 1000.0}} {
		if _, ok := EstimateEncodeSeconds(two, "a", args[0].(string), args[1].(string), args[2].(float64)); ok {
			t.Fatalf("%s must not make an estimate", name)
		}
	}
	// Bad samples do not count against the window of the good ones.
	if got, ok := EstimateEncodeSeconds(append(other, good, good), "a", "uhd", "x265", 1000); !ok || math.Abs(got-1000) > 1e-6 {
		t.Fatalf("good samples behind bad ones: %v %v", got, ok)
	}
}

func TestEstimateEncodeSecondsUsesOnlyTheNewestWindow(t *testing.T) {
	var samples []EncodeSample
	for i := 0; i < encodeEstimateWindow; i++ {
		samples = append(samples, sample("a", 1))
	}
	for i := 0; i < 50; i++ {
		samples = append(samples, sample("a", 10))
	}
	if got, ok := EstimateEncodeSeconds(samples, "a", "uhd", "x265", 1000); !ok || math.Abs(got-1000) > 1e-6 {
		t.Fatalf("older samples leaked in: %v %v", got, ok)
	}
}
