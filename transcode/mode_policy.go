package transcode

import "strings"

// ModePolicyV2 is the earlier mode policy. Historical plans keep it.
const ModePolicyV2 = "transcode-modes-v2"

// ModePolicyV3 is the size-mode policy that compacts lossless audio, encodes
// 8-bit sources as Main10 on proven workers and validates more windows on
// long sources. Only mode=size is frozen under it; its quality floor is v2's.
const ModePolicyV3 = "transcode-modes-v3"

// ModePolicyV5 is size policy v3 plus the size goal's audio levels
// (AudioModeSizeAAC and AudioModeSizeHEAAC, see audio.go): the plan carries one
// of them instead of compact_lossless. A size budget plans the level it chose
// for the source; a size goal without a budget, frozen once level 1 became the
// default, plans level 1. Only mode=size plans under it; the worker applies
// v3's rules to everything else in the plan.
const ModePolicyV5 = "transcode-modes-v5"

// ModePolicyV6 is the plan version of a size or compact plan that keeps only
// some of the source's audio and subtitle tracks (Plan.Tracks). The plan
// otherwise follows the rules of the version it extends: a size plan carries
// v3's compact_lossless audio mode or v5's levels, a compact plan v2's compact
// audio. A plan that removes nothing keeps its earlier version, so only the
// plans that need the track capability require it, and a worker that predates
// it refuses a v6 plan at its first gate.
const ModePolicyV6 = "transcode-modes-v6"

// ModePolicyV7 is the plan version of the size goal "audio only": the video is
// stream-copied and proven unchanged, the audio converts to one of v5's levels,
// and the plan may keep only some tracks (Plan.Tracks) without becoming v6.
// It has no minimum savings beyond the output not being larger than the
// source, so a worker that predates it refuses it at its first gate.
const ModePolicyV7 = "transcode-modes-v7"

// IsSizePolicyVersion reports a size mode policy version: v3, v5 which adds
// the audio levels, v6 which adds the track selection, or v7 the audio-only
// goal.
func IsSizePolicyVersion(version string) bool {
	return version == ModePolicyV3 || version == ModePolicyV5 || version == ModePolicyV6 || version == ModePolicyV7
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
	if version == ModePolicyV7 {
		return 0, mode == "size"
	}
	if version == ModePolicyV6 {
		switch mode {
		case "size":
			return 15, true
		case "compact":
			return CompactMinimumSavingsPercent, true
		}
		return 0, false
	}
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
