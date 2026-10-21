package transcode

import (
	"testing"
)

func TestModeMinimumSavingsV3(t *testing.T) {
	for _, version := range []string{"", "transcode-modes-v1", ModePolicyV2, ModePolicyV3} {
		if min, ok := ModeMinimumSavings("size", version); !ok || min != 15 {
			t.Fatalf("size %q: %d %v", version, min, ok)
		}
	}
	for _, mode := range []string{"quality", "x265_preserve", "compact"} {
		if _, ok := ModeMinimumSavings(mode, ModePolicyV3); ok {
			t.Fatalf("%s accepted under v3", mode)
		}
	}
	if !ModeSizeAllowedVersion("size", 100, 85, ModePolicyV3) || ModeSizeAllowedVersion("size", 100, 86, ModePolicyV3) {
		t.Fatal("v3 size gate is not 15% savings")
	}
}
