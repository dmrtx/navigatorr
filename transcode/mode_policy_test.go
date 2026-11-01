package transcode

import "testing"

func TestModeMinimumSavingsByPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode, version string
		savings       int
		valid         bool
	}{
		{"size", "", 15, true},
		{"quality", "", 15, true},
		{"quality", ModePolicyV2, 0, true},
		{"x265_preserve", "", 0, true},
		{"compact", ModePolicyV2, CompactMinimumSavingsPercent, true},
		{"compact", "", 0, false},
		{"size", ModePolicyV3, 15, true},
		{"quality", ModePolicyV3, 0, false},
		{"compact", ModePolicyV8, CompactMinimumSavingsPercent, true},
		{"size", ModePolicyV8, 0, false},
		{"size", ModePolicyV9, SizeSmallerSavingPercent, true},
		{"compact", ModePolicyV9, 0, false},
		{"size", "transcode-modes-v99", 0, false},
		{"unknown", "", 0, false},
	} {
		got, valid := ModeMinimumSavings(tc.mode, tc.version)
		if got != tc.savings || valid != tc.valid {
			t.Errorf("%s/%s: got %d,%v want %d,%v", tc.mode, tc.version, got, valid, tc.savings, tc.valid)
		}
	}
}

func TestModeSizeBoundaryDoesNotOverflow(t *testing.T) {
	source := int64(9007199254740999)
	maximum := (source/100)*85 + (source%100)*85/100
	if !ModeSizeAllowedVersion("quality", source, maximum, "") || ModeSizeAllowedVersion("quality", source, maximum+1, "") {
		t.Fatal("unrounded byte boundary lost")
	}
	if !ModeSizeAllowedVersion("x265_preserve", 100, 100, "") || ModeSizeAllowedVersion("x265_preserve", 100, 101, "") {
		t.Fatal("preserve must allow an equal-size result and no larger")
	}
}

func TestHardwareReasonStripsTheSelectionNote(t *testing.T) {
	for _, tc := range []struct {
		notes []string
		want  string
	}{
		{nil, ""},
		{[]string{"other note"}, ""},
		{[]string{HardwareSelectedNote}, ""},
		{[]string{"other", HardwareSelectedNote + ": no software candidate qualified"}, "no software candidate qualified"},
	} {
		if got := HardwareReason(tc.notes); got != tc.want {
			t.Errorf("%v: got %q want %q", tc.notes, got, tc.want)
		}
	}
}
