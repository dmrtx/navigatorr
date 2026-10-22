package transcode

import "strings"

// ModePolicyV2 is the earlier mode policy. Historical plans keep it.
const ModePolicyV2 = "transcode-modes-v2"

// ModePolicyV3 is the size-mode policy that compacts lossless audio, encodes
// 8-bit sources as Main10 on proven workers and validates more windows on
// long sources. Only mode=size is frozen under it; its quality floor is v2's.
const ModePolicyV3 = "transcode-modes-v3"

// ModePolicyV5 is size policy v3 plus the audio levels of a size budget goal
// (AudioModeSizeAAC and AudioModeSizeHEAAC, see audio.go): the plan carries one
// of them instead of compact_lossless. Only mode=size with a byte budget is
// frozen under it; the worker applies v3's rules to everything else in the plan.
const ModePolicyV5 = "transcode-modes-v5"

// IsSizePolicyVersion reports a size mode policy version: v3, or v5 which adds
// the audio levels.
func IsSizePolicyVersion(version string) bool {
	return version == ModePolicyV3 || version == ModePolicyV5
}

// CompactMinimumSavingsPercent is the measured savings compact mode requires.
// Converting 4K HDR to SDR 1080p normally saves far more; a smaller saving
// means the source was already efficient and the original is kept.
const CompactMinimumSavingsPercent = 30

// ModeSizeAllowedVersion compares a mode policy's minimum savings with exact
// integer bytes. Splitting the quotient and remainder avoids overflow even at
// MaxInt64.
func ModeSizeAllowedVersion(mode string, source, candidate int64, version string) bool {
	minimum, valid := ModeMinimumSavings(mode, version)
	if !valid || source <= 0 || candidate <= 0 {
		return false
	}
	if minimum == 0 {
		return candidate <= source
	}
	keep := int64(100 - minimum)
	maximum := (source/100)*keep + ((source%100)*keep)/100
	return candidate <= maximum
}

// ModeMinimumSavings is shared by the planner and full-plan admission,
// including historical plans.
func ModeMinimumSavings(mode, version string) (int, bool) {
	if IsSizePolicyVersion(version) {
		if mode != "size" {
			return 0, false
		}
		return 15, true
	}
	if version != "" && version != "transcode-modes-v1" && version != ModePolicyV2 {
		return 0, false
	}
	switch mode {
	case "x265_preserve":
		return 0, true
	case "quality":
		if version == ModePolicyV2 {
			return 0, true
		}
		return 15, true
	case "size":
		return 15, true
	case "compact":
		// Compact mode was introduced with policy v2; it never ran under v1
		// or the unversioned legacy policy, so only v2 is accepted.
		if version != ModePolicyV2 {
			return 0, false
		}
		return CompactMinimumSavingsPercent, true
	default:
		return 0, false
	}
}

// HardwareSelectedNote begins the uncertainty a decision recorded when the
// hardware margin chose a media-engine winner. The selector that wrote it is
// gone, but stored decisions still carry it.
const HardwareSelectedNote = "hardware candidate selected"

// HardwareReason returns why a media-engine winner was chosen, taken from the
// uncertainty that begins with HardwareSelectedNote and stripped of that
// prefix, or "" when the winner carries no such note.
func HardwareReason(uncertainties []string) string {
	for _, u := range uncertainties {
		if strings.HasPrefix(u, HardwareSelectedNote) {
			return strings.TrimPrefix(strings.TrimPrefix(u, HardwareSelectedNote), ": ")
		}
	}
	return ""
}
