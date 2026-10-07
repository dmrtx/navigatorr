package transcode

import (
	"math"
	"testing"
)

func TestFinalSizeFrozenModeBoundary(t *testing.T) {
	for _, tc := range []struct {
		source, candidate int64
		minimum           float64
		pass              bool
	}{{100, 85, 15, true}, {100, 86, 15, false}, {100, 95, 15, false}, {100, 95, 0, true}, {100, 110, 0, false}, {101, 86, 15, false}, {1000000000000000000, 850000000000000000, 15, true}, {1000000000000000000, 850000000000000001, 15, false}, {0, 85, 15, false}, {100, 85, math.NaN(), false}} {
		err := ValidateFinalSize(tc.source, tc.candidate, &SizePolicy{MinSavingsPercent: tc.minimum, MaxSizeIncreasePercent: 0})
		if (err == nil) != tc.pass {
			t.Fatalf("source=%d candidate=%d minimum=%v pass=%v: %v", tc.source, tc.candidate, tc.minimum, tc.pass, err)
		}
	}
	if err := ValidateFinalSize(0, 0, nil); err != nil {
		t.Fatal("legacy behavior changed", err)
	}
}
