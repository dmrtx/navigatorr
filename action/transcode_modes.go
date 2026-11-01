package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const ModePolicyVersion = "transcode-modes-v2"

// SizeModePolicyVersion is what size content was frozen under before v4, and
// the plan version of a size goal whose audio is compact_lossless (frozen
// before the audio levels). A size goal at an audio level plans under v5 (see
// workerVersion). Older frozen size policies keep their own version and
// behaviour.
const SizeModePolicyVersion = transcode.ModePolicyV3

// modeOperationBudgetSeconds stays in the snapshots of the other modes and in
// every snapshot already frozen, so their digests are unchanged. It caps
// nothing (see reserveModeSearch): a new size snapshot omits it.
const modeOperationBudgetSeconds = 3600
const modeSearchBudgetSeconds = 600

// Size v3 may run a source's search past modeSearchBudgetSeconds while it keeps
// finishing work, up to sizeSearchMaxSeconds from the search start, plus
// sizeSearchVerifySeconds for a search that already has a winner to verify. A
// worker shared with other encodes otherwise spends the base budget on
// contention, not on the source. Policies frozen before this carry no
// extension and keep the fixed budget.
//
// The numbers come from the timings of a batch of one-hour 1080p episodes of
// 2-4 GB searched two at a time: the base budget covered roughly 6-9 of 15-17
// planned sample encodes (each with its quality passes) of the largest source,
// and a search that also needs the fallback candidates and two verification
// attempts takes about three times as long. 2400 s covers that with margin; 600
// s covers the two holdout windows of a verification attempt under the same
// load. A single unit took about 10 s (encode) to 60 s (encode on the largest
// source, or one quality evaluation) there; the slower presets of the fallback
// family can take a few times that, so the steady window is 300 s.
const (
	sizeSearchMaxSeconds    = 2400
	sizeSearchSteadySeconds = 300
	sizeSearchVerifySeconds = 600
)

// ModePolicySnapshot is frozen with each operation. These thresholds are a
// bounded SDR sample policy, never a claim of perceptual transparency.
type ModePolicySnapshot struct {
	Version                string         `json:"version"`
	Mode                   string         `json:"mode"`
	MinSavingsPercent      float64        `json:"min_savings_percent"`
	MaxSizeIncreasePercent float64        `json:"max_size_increase_percent"`
	SearchBudgetSeconds    int            `json:"search_budget_seconds"`
	OperationBudgetSeconds int            `json:"operation_budget_seconds,omitempty"`
	Profile                recipe.Profile `json:"profile"`
	// Compact is set only for mode=compact, so other snapshots and their
	// digests are unchanged.
	Compact *ModeCompactPolicy `json:"compact,omitempty"`
	// Size is set only for size policy v3, so v2 snapshots and their digests
	// are unchanged.
	Size *ModeSizePolicy `json:"size,omitempty"`
	// Recipe is set only for policy v4: the fixed encodes the goal runs
	// instead of a search (see transcode_recipe.go). Earlier snapshots have
	// none, so their digests are unchanged.
	Recipe *ModeRecipe `json:"recipe,omitempty"`
	// Tracks is set only on size and compact snapshots frozen once track
	// selection existed: the languages whose audio and subtitle tracks the goal
	// keeps (and any manual stream picks). Earlier snapshots have none, keep
	// every track, and keep their digests.
	Tracks *transcode.TrackPolicy `json:"tracks,omitempty"`
	Digest string                 `json:"digest"`
}

// ModeSizePolicy freezes what size v3 adds on top of the v2 quality floor:
// Main10 output for 8-bit sources on workers that prove native 10-bit
// quality, an x265-only ladder, and final windows scaled with duration.
type ModeSizePolicy struct {
	EightBitMain10          bool  `json:"eight_bit_main10"`
	VideoToolboxCandidates  bool  `json:"videotoolbox_candidates"`
	FallbackQualityValues   []int `json:"fallback_quality_values"`
	FallbackSavingsValues   []int `json:"fallback_savings_values"`
	FinalWindowSeconds      int   `json:"final_window_seconds"`
	FinalWindowEverySeconds int   `json:"final_window_every_seconds"`
	FinalWindowsMinimum     int   `json:"final_windows_minimum"`
	FinalWindowsMaximum     int   `json:"final_windows_maximum"`
	// Hardware is set when the operation allows the media engine (the default
	// for new size operations; allow_hardware=false turns it off):
	// VideoToolbox candidates measured beside x265 that win within a frozen
	// size margin. It is absent from snapshots frozen before it existed and
	// from operations that turn it off, so their size v3 digests are unchanged.
	Hardware *transcode.HardwareSearchRules `json:"hardware_search,omitempty"`
	// AudioOnlyFallback lets a source whose video cannot shrink within the
	// floor still compact its lossless audio, with the video stream-copied
	// and proven bit-identical. The same minimum savings applies.
	AudioOnlyFallback bool `json:"audio_only_fallback,omitempty"`
	// LosslessObjectAudioToAAC converts lossless Atmos and DTS:X (TrueHD,
	// DTS-HD MA) to AAC on their channel bed, like any lossless track. Lossy
	// object audio (E-AC-3 JOC) follows the audio mode: the bed levels
	// convert it to its bed too, every other mode copies it.
	LosslessObjectAudioToAAC bool `json:"lossless_object_audio_to_aac"`
	// SearchExtension lets the source search run past the base budget while
	// work keeps finishing. It is absent from snapshots frozen before it
	// existed, so their digests and fixed budget are unchanged.
	SearchExtension *transcode.SearchExtension `json:"search_extension,omitempty"`
	// AutomaticSearch freezes the search rules that keep a size search from
	// ending with nothing while safer settings went unverified: a selection
	// margin over the VMAF p5 and worst-window minimums, quality-first
	// verification after a holdout fails on quality, verification on the whole
	// final validation grid until a setting passes or none is eligible, and a
	// medium-before-slow quality fallback. It is absent from snapshots frozen before it existed, so
	// their digests and search rules are unchanged. A snapshot frozen with
	// verification on the final grid carries no selection margin: a candidate
	// that passes the plain limits goes to grid verification, which measures
	// every window the final check re-measures. Snapshots frozen with a margin
	// keep it.
	AutomaticSearch *transcode.AutomaticSearchRules `json:"automatic_search,omitempty"`
	// SizeBudget is set when the operation gives a size budget
	// (size_budget_percent or size_budget_bytes): a search that ends with no
	// passing setting, or only one larger than the budget, encodes once at the
	// budget and reports the result as below the quality target. It is absent
	// otherwise, so every digest without a budget is unchanged.
	SizeBudget *SizeBudgetPolicy `json:"size_budget,omitempty"`
	// AudioOnly is the size goal "audio only" (input audio_only): the video is
	// copied unchanged, the audio converts at the goal's level and the kept
	// tracks follow the track selection, with no minimum saving beyond a
	// smaller file. Its recipe is RecipeAudioOnly and its plans policy v7. It
	// is absent from every other snapshot, so their digests are unchanged.
	AudioOnly bool `json:"audio_only,omitempty"`
	// SmallerSaving is a size goal that accepts a smaller saving (input
	// accept_smaller_saving): the minimum is transcode.SizeSmallerSavingPercent
	// instead of 15%, and its plans are policy v9. It is absent from every
	// other snapshot, so their digests are unchanged.
	SmallerSaving bool `json:"smaller_saving,omitempty"`
}

// sizeEdgeSelectionMarginVMAF is how far a size-mode winner's VMAF p5 and
// worst-window mean must clear their minimums (84 and 85) when its search does
// not verify on the final validation grid. Snapshots frozen with that margin
// keep it (the old edge holdouts measured two windows of the dozen the final
// check re-measures, so a thin pass tended to fail it), and a search that
// cannot verify on the grid (a worker without grid verification, a source too
// short for the grid) falls back to it. A grid-verifying snapshot freezes none:
// the margin did not predict final validation, and the grid does.
const sizeEdgeSelectionMarginVMAF = 1.5

// sizeFinalFreshWindows is how many windows final validation adds that the
// search never measured. They are reported beside the grid and do not gate it.
const sizeFinalFreshWindows = 2

// sizeFallbackQualityValues are the quality fallback rungs, tried most
// compressed first (a rung that passes ends the walk). They reach CRF 14 for a
// hard source on which every CRF 16 setting still failed the grid windows, and
// every step between 14 and the ladder's 22 so a source that misses CRF 22 by
// a little does not pay a whole step of 2.
var sizeFallbackQualityValues = []int{14, 15, 16, 17, 18, 19, 20, 21}

// sizeSavingsFallbackValues are the slow CRFs tried when quality passed but
// savings did not. Slow stays automatic here: it is how a source that passes
// quality at medium but does not save enough gets there.
var sizeSavingsFallbackValues = []int{24, 27, 30}

// sizeRefinementQualityValues are the x265 CRF rungs between the ladder's
// (22-30 in steps of 2), measured only between a rung that passed and the next,
// more compressed, one that did not.
var sizeRefinementQualityValues = []int{23, 25, 27, 29}

// sizeModeV3Policy is the frozen size v3 addition. The ladder is x265 only:
// medium CRF 22-30 first (with the in-between CRF measured next to the first
// failure), then one fallback family chosen from what failed: savings (slow) or
// quality (medium, walking up from CRF 21 to 14). The slow quality fallback is
// part of the policy only when the operation opts in: a slow full encode of an
// hour of 1080p video took 3.6-6.9 h.
func sizeModeV3Policy(allowSlow bool) *ModeSizePolicy {
	presets := []string{"medium"}
	if allowSlow {
		presets = append(presets, "slow")
	}
	return &ModeSizePolicy{EightBitMain10: true, FallbackQualityValues: slices.Clone(sizeFallbackQualityValues), FallbackSavingsValues: slices.Clone(sizeSavingsFallbackValues), FinalWindowSeconds: 10, FinalWindowEverySeconds: 600, FinalWindowsMinimum: 5, FinalWindowsMaximum: 12, AudioOnlyFallback: true, LosslessObjectAudioToAAC: true, SearchExtension: &transcode.SearchExtension{MaxSeconds: sizeSearchMaxSeconds, SteadySeconds: sizeSearchSteadySeconds, VerifySeconds: sizeSearchVerifySeconds},
		AutomaticSearch: &transcode.AutomaticSearchRules{HoldoutWindows: transcode.HoldoutWindowsFinalGrid, VerifyUntilExhausted: true, FinalFreshWindows: sizeFinalFreshWindows, QualityFallbackPresets: presets, QualityWalk: true, RefinementQualityValues: slices.Clone(sizeRefinementQualityValues), DeferBanding: true}}
}

// ModeCompactPolicy freezes the output shape of compact mode: SDR BT.709 no
// taller than MaxHeight, through the named worker chain. With KeepHDR an HDR
// or Dolby Vision 7/8 source keeps HDR instead (transcode.CompactHDRChainVersion:
// HDR10 or HLG with the source's static metadata, the dynamic layer dropped);
// an SDR source is planned as without it. Goals frozen before keep_hdr have
// no such field and keep their digest.
type ModeCompactPolicy struct {
	MaxHeight    int    `json:"max_height"`
	ChainVersion string `json:"chain_version"`
	KeepHDR      bool   `json:"keep_hdr,omitempty"`
}

// Hardware search (size mode, on unless the operation turns it off). The ladder
// straddles the quality gates on the measured clips (matched values 45-57 with
// a 240-frame GOP); the default margin is how much larger than the best x265
// result a hardware result may be and still win, for the speed it gives.
const (
	sizeHardwareDefaultMarginPercent = 15
	sizeHardwareGOPSize              = 240
	sizeHardwareBFrames              = 1
)

// sizeEdgeHardwareExtraMarginVMAF is how much more than the x265 selection
// margin (sizeEdgeSelectionMarginVMAF) a VideoToolbox result must clear the
// VMAF p5 and worst-window minimums by: 3.5 in all. It applies to snapshots
// frozen with it and to a search that cannot verify on the final grid; a
// grid-verifying snapshot freezes none, since every final window is measured
// before the full encode. On a hard 1080p episode a hardware
// result won its sample search at p5 87.1 and worst window 89.2 against limits
// of 84 and 85, then failed the full-file check. The experiment that justified
// the ladder (docs/transcode/reviews/2026-10-19-hardware-media-engine-experiment.md)
// used synthetic clips and 1.5 of margin, and says the margin must be checked
// on real content; 2 more points rejects that result on p5 and still lets a
// hardware result with real headroom win.
const sizeEdgeHardwareExtraMarginVMAF = 2

var sizeHardwareQualityValues = []int{45, 50, 55, 60}

// sizeHardwareFallbackQualityValues are the higher-quality rungs tried when
// every rung above fails quality. On a hard 1080p episode q60 missed the VMAF
// p5 limit by six points, so the ladder's best rung can be too low; the
// fallback adds headroom without making the common case pay for it. File
// sizes grow steeply with q, which the size margin and the savings minimum
// bound.
var sizeHardwareFallbackQualityValues = []int{65, 70, 75}

// modeHardwareInput reads allow_hardware and hardware_margin_percent for a
// mode. Size mode allows the media engine unless allow_hardware is false, so
// an operation that says nothing gets the default margin; the other modes
// accept neither input. It returns nil rules when the media engine is not
// allowed.
func modeHardwareInput(mode string, inputs map[string]any) (*transcode.HardwareSearchRules, error) {
	allowRaw, hasAllow := inputs["allow_hardware"]
	marginRaw, hasMargin := inputs["hardware_margin_percent"]
	if hasAllow && allowRaw == nil {
		hasAllow = false
	}
	if hasMargin && marginRaw == nil {
		hasMargin = false
	}
	allow := mode == "size"
	if hasAllow {
		b, ok := allowRaw.(bool)
		if !ok {
			return nil, inputErr("allow_hardware", "allow_hardware must be true or false")
		}
		if b && mode != "size" {
			return nil, inputErr("allow_hardware", "allow_hardware is only supported with mode=size")
		}
		allow = b
	}
	if hasMargin && mode != "size" {
		return nil, inputErr("hardware_margin_percent", "hardware_margin_percent is only supported with mode=size")
	}
	if !allow {
		if hasMargin {
			return nil, inputErr("hardware_margin_percent", "hardware_margin_percent requires allow_hardware=true")
		}
		return nil, nil
	}
	margin := float64(sizeHardwareDefaultMarginPercent)
	if hasMargin {
		switch n := marginRaw.(type) {
		case float64:
			margin = n
		case int:
			margin = float64(n)
		case int64:
			margin = float64(n)
		default:
			return nil, inputErr("hardware_margin_percent", "hardware_margin_percent must be a number")
		}
	}
	rules := &transcode.HardwareSearchRules{MarginPercent: margin, QualityValues: append([]int(nil), sizeHardwareQualityValues...), FallbackQualityValues: append([]int(nil), sizeHardwareFallbackQualityValues...), GOPSize: sizeHardwareGOPSize, BFrames: sizeHardwareBFrames}
	if err := rules.Validate(); err != nil {
		return nil, attributeInput("hardware_margin_percent", err)
	}
	return rules, nil
}

// ValidateHardwareInputs reports whether allow_hardware,
// hardware_margin_percent, allow_slow_preset, audio_only,
// accept_smaller_saving and the size budget are acceptable for a goal, as
// the policy freeze reads them. Callers that admit a goal change use it to fail
// early.
func ValidateHardwareInputs(mode string, inputs map[string]any) error {
	if _, err := modeHardwareInput(mode, inputs); err != nil {
		return err
	}
	if _, err := modeAudioOnlyInput(mode, inputs); err != nil {
		return err
	}
	if _, err := modeSmallerSavingInput(mode, inputs); err != nil {
		return err
	}
	if _, err := modeKeepHDRInput(mode, inputs); err != nil {
		return err
	}
	if _, err := modeSlowPresetInput(mode, inputs); err != nil {
		return err
	}
	_, err := modeSizeBudgetInput(mode, inputs)
	return err
}

// modePolicyFor freezes a mode. maxHeight applies only to compact mode, where
// zero selects the 1080p default.
func modePolicyFor(mode string, anime bool, maxHeight int) (ModePolicySnapshot, error) {
	return modePolicyWithHardware(mode, anime, maxHeight, nil)
}

// modeSlowPresetInput reads allow_slow_preset for a mode: size mode only, off
// unless the operation says true. It is the opt-in for the slow preset in the
// quality fallback, whose full encodes took hours on a hard 1080p source.
func modeSlowPresetInput(mode string, inputs map[string]any) (bool, error) {
	raw, has := inputs["allow_slow_preset"]
	if !has || raw == nil {
		return false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, inputErr("allow_slow_preset", "allow_slow_preset must be true or false")
	}
	if b && mode != "size" {
		return false, inputErr("allow_slow_preset", "allow_slow_preset is only supported with mode=size")
	}
	return b, nil
}

// modePolicyWithHardware also freezes the size-mode hardware search rules
// when the operation allows the media engine; they apply to mode=size only.
func modePolicyWithHardware(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules) (ModePolicySnapshot, error) {
	return modePolicyWithOptions(mode, anime, maxHeight, hardware, false)
}

// modePolicyWithOptions also freezes whether a size search may fall back to the
// slow preset for quality (allowSlow, size mode only).
func modePolicyWithOptions(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool) (ModePolicySnapshot, error) {
	return modePolicyWithBudget(mode, anime, maxHeight, hardware, allowSlow, nil)
}

// modePolicyWithBudget also freezes the size budget (size mode only). Every
// new operation is frozen under policy v4: the content v2 and v3 froze for the
// mode, plus the recipe that replaces their search.
func modePolicyWithBudget(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool, budget *SizeBudgetPolicy) (ModePolicySnapshot, error) {
	return modePolicyWithTracks(mode, anime, maxHeight, hardware, allowSlow, budget, nil)
}

// modeAudioOnlyInput reads audio_only, the size goal that copies the video and
// converts only the audio: size mode only, off unless the operation says true.
// It takes no size budget and no slow preset; the media engine has nothing to
// encode, so allow_hardware is ignored.
func modeAudioOnlyInput(mode string, inputs map[string]any) (bool, error) {
	raw, has := inputs["audio_only"]
	if !has || raw == nil {
		return false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, inputErr("audio_only", "audio_only must be true or false")
	}
	if !b {
		return false, nil
	}
	if mode != "size" {
		return false, inputErr("audio_only", "audio_only is only supported with mode=size")
	}
	for _, key := range []string{"size_budget_percent", "size_budget_bytes", "size_budget_source"} {
		if v, ok := inputs[key]; ok && v != nil {
			return false, inputErr(key, "audio_only cannot be combined with a size budget")
		}
	}
	if slow, _ := inputs["allow_slow_preset"].(bool); slow {
		return false, inputErr("allow_slow_preset", "audio_only cannot be combined with allow_slow_preset")
	}
	return true, nil
}

// modeKeepHDRInput reads keep_hdr, the Shrink goal that keeps HDR and Dolby
// Vision 7/8 sources as HDR10 or HLG instead of converting them to SDR:
// compact mode only, off unless the operation says true.
func modeKeepHDRInput(mode string, inputs map[string]any) (bool, error) {
	raw, has := inputs["keep_hdr"]
	if !has || raw == nil {
		return false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, inputErr("keep_hdr", "keep_hdr must be true or false")
	}
	if b && mode != "compact" {
		return false, inputErr("keep_hdr", "keep_hdr is only supported with mode=compact")
	}
	return b, nil
}

// modeSmallerSavingInput reads accept_smaller_saving, the size goal that keeps
// a file saving transcode.SizeSmallerSavingPercent instead of 15%: size mode
// only, off unless the operation says true, and not with audio_only (which
// keeps any smaller file) or a size budget (which picks its own size).
func modeSmallerSavingInput(mode string, inputs map[string]any) (bool, error) {
	raw, has := inputs["accept_smaller_saving"]
	if !has || raw == nil {
		return false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, inputErr("accept_smaller_saving", "accept_smaller_saving must be true or false")
	}
	if !b {
		return false, nil
	}
	if mode != "size" {
		return false, inputErr("accept_smaller_saving", "accept_smaller_saving is only supported with mode=size")
	}
	if only, _ := inputs["audio_only"].(bool); only {
		return false, inputErr("accept_smaller_saving", "accept_smaller_saving cannot be combined with audio_only")
	}
	for _, key := range []string{"size_budget_percent", "size_budget_bytes", "size_budget_source"} {
		if v, ok := inputs[key]; ok && v != nil {
			return false, inputErr(key, "accept_smaller_saving cannot be combined with a size budget")
		}
	}
	return true, nil
}

// modePolicyWithTracks also freezes the track selection (size and compact
// only). It is frozen with the goal, so a later change of the global list never
// reaches a job already planned.
func modePolicyWithTracks(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool, budget *SizeBudgetPolicy, tracks *transcode.TrackPolicy) (ModePolicySnapshot, error) {
	return modePolicyGoal(mode, anime, maxHeight, hardware, allowSlow, budget, tracks, false, false, false)
}

// modePolicyGoal also freezes the audio-only size goal (audioOnly), which
// drops the media engine and the minimum saving and plans one audio-only
// encode (RecipeAudioOnly), the size goal that accepts a smaller saving
// (smallerSaving), and the Shrink goal that keeps HDR (keepHDR).
func modePolicyGoal(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool, budget *SizeBudgetPolicy, tracks *transcode.TrackPolicy, audioOnly, smallerSaving, keepHDR bool) (ModePolicySnapshot, error) {
	if keepHDR && mode != "compact" {
		return ModePolicySnapshot{}, inputErr("keep_hdr", "keep_hdr is only supported with mode=compact")
	}
	if smallerSaving {
		switch {
		case mode != "size":
			return ModePolicySnapshot{}, inputErr("accept_smaller_saving", "accept_smaller_saving is only supported with mode=size")
		case audioOnly:
			return ModePolicySnapshot{}, inputErr("accept_smaller_saving", "accept_smaller_saving cannot be combined with audio_only")
		case budget != nil:
			return ModePolicySnapshot{}, inputErr(budget.field(), "accept_smaller_saving cannot be combined with a size budget")
		}
	}
	if audioOnly {
		if mode != "size" {
			return ModePolicySnapshot{}, inputErr("audio_only", "audio_only is only supported with mode=size")
		}
		if budget != nil {
			return ModePolicySnapshot{}, inputErr(budget.field(), "audio_only cannot be combined with a size budget")
		}
		if allowSlow {
			return ModePolicySnapshot{}, inputErr("allow_slow_preset", "audio_only cannot be combined with allow_slow_preset")
		}
		hardware = nil
	}
	snapshot, err := modePolicyContent(mode, anime, maxHeight, hardware, allowSlow, budget)
	if err != nil {
		return snapshot, err
	}
	if keepHDR {
		snapshot.Compact.KeepHDR = true
	}
	if tracks != nil {
		if mode != "size" && mode != "compact" {
			return snapshot, inputErr(inputKeepLanguages, "track selection is only supported with mode=size or mode=compact")
		}
		if err := tracks.Validate(); err != nil {
			return snapshot, attributeInput(inputKeepLanguages, err)
		}
		snapshot.Tracks = tracks.Clone()
	}
	if mode == "size" && budget == nil {
		// Every new size goal plans its audio at level 1 at least: a lossy
		// track well above its AAC-LC target is converted too, not only a
		// lossless one. A budget picks its own level (modePolicyContent).
		// Level 0, compact_lossless, stays in the snapshots frozen before
		// this, which keep their content, digest and plan version (v3). New
		// goals freeze the bed pair's level 1, which converts lossy Atmos too;
		// those frozen with size_aac keep it.
		snapshot.Profile.Audio.Mode = transcode.AudioModeSizeAACBed
	}
	if mode == "compact" {
		// Every new compact goal (Shrink) converts its audio at the size
		// goal's level 1 too, which plans the audio per track instead of
		// compact audio's one rule; its plans are policy v8. Goals frozen
		// before keep compact audio, their digest and v2.
		snapshot.Profile.Audio.Mode = transcode.AudioModeSizeAACBed
	}
	if smallerSaving {
		snapshot.MinSavingsPercent = transcode.SizeSmallerSavingPercent
		snapshot.Size.SmallerSaving = true
	}
	if audioOnly {
		// Nothing is re-encoded, so no Main10 output and no minimum saving:
		// the goal keeps a file that came out smaller at all.
		snapshot.MinSavingsPercent = 0
		snapshot.Size.EightBitMain10 = false
		snapshot.Size.AudioOnly = true
	}
	snapshot.Version = ModePolicyV4
	// Nothing searches under v4, so nothing is reserved against an operation
	// budget. SearchBudgetSeconds stays: the request the first encode's final
	// windows come from is never submitted, but is validated like any other.
	snapshot.OperationBudgetSeconds = 0
	snapshot.Recipe = modeRecipeFor(&snapshot, allowSlow)
	if err := snapshot.Recipe.validate(&snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, digestModePolicy(&snapshot)
}

// digestModePolicy sets the snapshot's digest over its content.
func digestModePolicy(snapshot *ModePolicySnapshot) error {
	snapshot.Digest = ""
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	snapshot.Digest = "sha256:" + hex.EncodeToString(h[:])
	return nil
}

// automaticBatchSearchProfile is the base profile policies v2 and v3 froze for
// a mode. It stays because snapshots frozen under them, and the v4 snapshots
// that carry the same content, are digested over it: changing a value changes
// every digest. Nothing searches with it any more.
func automaticBatchSearchProfile(isAnime bool) recipe.Profile {
	ptr := func(v float64) *float64 { return &v }
	p := recipe.Profile{
		Container:  "mkv",
		Video:      recipe.VideoProfile{Codec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", Profile: "main", PixelFormat: "yuv420p"},
		Audio:      recipe.AudioProfile{Mode: "copy"},
		Subtitles:  recipe.SubtitleProfile{Mode: "preserve", ConvertIncompatible: true},
		Preserve:   recipe.PreserveProfile{Metadata: true, Chapters: true, Attachments: true},
		Resilience: recipe.ResilienceProfile{MaxAttempts: 1},
		Optimization: &recipe.OptimizationPolicy{
			Enabled:  true,
			Sampling: &recipe.SamplingPolicy{Strategy: "distributed", SampleCount: 3, SampleSeconds: 10, Positions: []float64{0.2, 0.5, 0.8}},
			Search:   &recipe.SearchPolicy{MaxCandidates: 3, QualityValues: []int{20, 23, 26}},
			Quality: &recipe.QualityPolicy{
				PreferredMetric: "vmaf",
				VMAF:            &recipe.MetricTarget{Model: "v1_1080p_3h", Target: 92, Minimum: 88, MarginalTolerance: ptr(2), GuardrailEnforcement: "reject", P5Minimum: ptr(84), WorstWindowMinimum: ptr(85), WorstWindowSeconds: 5},
				Banding:         &recipe.BandingPolicy{Enabled: true, Metric: "cambi", Mode: "full_ref", Enforcement: "reject", MaxMean: ptr(6), MaxPeak: ptr(16)},
				FinalValidation: &recipe.FinalValidationPolicy{Mode: "sampled"},
			},
		},
	}
	if isAnime {
		p.Video.Tune = "animation"
	}
	return p
}

// modePolicyContent is what policies v2 (quality, x265_preserve, compact) and
// v3 (size) freeze for a mode, without a digest. Snapshots already frozen
// under them keep exactly this content.
func modePolicyContent(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool, budget *SizeBudgetPolicy) (ModePolicySnapshot, error) {
	if budget != nil && mode != "size" {
		return ModePolicySnapshot{}, inputErr(budget.field(), "size_budget_percent and size_budget_bytes are only supported with mode=size")
	}
	if err := budget.Validate(); err != nil {
		return ModePolicySnapshot{}, attributeInput(budget.field(), err)
	}
	if hardware != nil && mode != "size" {
		return ModePolicySnapshot{}, inputErr("allow_hardware", "allow_hardware is only supported with mode=size")
	}
	if allowSlow && mode != "size" {
		return ModePolicySnapshot{}, inputErr("allow_slow_preset", "allow_slow_preset is only supported with mode=size")
	}
	if mode != "size" && mode != "quality" && mode != "x265_preserve" && mode != "compact" {
		return ModePolicySnapshot{}, inputErr("mode", "mode must be size, quality, x265_preserve, or compact")
	}
	if mode != "compact" && maxHeight != 0 {
		return ModePolicySnapshot{}, inputErr("max_height", "max_height is only supported with mode=compact")
	}
	if mode == "compact" {
		if maxHeight == 0 {
			maxHeight = transcode.CompactDefaultMaxHeight
		}
		if !transcode.ValidCompactMaxHeight(maxHeight) {
			return ModePolicySnapshot{}, inputErr("max_height", "max_height must be one of %v", transcode.CompactMaxHeights)
		}
	}
	p := automaticBatchSearchProfile(anime)
	p.Optimization.Search.MaxCandidates = 12
	if mode == "compact" {
		// Compact keeps the size-mode quality floor: the visible change is
		// the intended SDR/resolution conversion, measured separately.
		p.Audio.Mode = "compact"
	} else if mode == "size" {
		// Same quality floor as v2; v3 converts only lossless audio. A size
		// budget with audio levels starts from the first one, and its plan
		// takes the level chosen for the source. A new size goal without a
		// budget is at level 1 too (modePolicyWithTracks).
		p.Audio.Mode = transcode.AudioModeCompactLossless
		if budget != nil && budget.Audio != nil {
			p.Audio.Mode = budget.Audio.Modes[0]
		}
		p.Optimization.Search.QualityValues = []int{22, 24, 26, 28, 30}
	} else {
		p.Optimization.Quality.VMAF.Target = 96
		p.Optimization.Quality.VMAF.Minimum = 95
		p5, worst := 93.0, 92.0
		p.Optimization.Quality.VMAF.P5Minimum = &p5
		p.Optimization.Quality.VMAF.WorstWindowMinimum = &worst
	}
	snapshot := ModePolicySnapshot{Version: ModePolicyVersion, Mode: mode, MinSavingsPercent: 15, SearchBudgetSeconds: modeSearchBudgetSeconds, OperationBudgetSeconds: modeOperationBudgetSeconds, Profile: p}
	if mode == "x265_preserve" || mode == "quality" {
		snapshot.MinSavingsPercent = 0
	}
	if mode == "size" {
		snapshot.Version = SizeModePolicyVersion
		snapshot.OperationBudgetSeconds = 0
		snapshot.Size = sizeModeV3Policy(allowSlow)
		snapshot.Size.Hardware = hardware.Clone()
		snapshot.Size.SizeBudget = budget.Clone()
		if snapshot.Size.SizeBudget != nil && snapshot.Size.Hardware != nil {
			// The media engine is allowed, so the budget skips the search and
			// walks the VideoToolbox ladder (see transcode_size_budget_ladder.go).
			snapshot.Size.SizeBudget.VTLadder = slices.Clone(sizeBudgetVTLadder)
		}
	}
	if mode == "compact" {
		snapshot.MinSavingsPercent = transcode.CompactMinimumSavingsPercent
		snapshot.Compact = &ModeCompactPolicy{MaxHeight: maxHeight, ChainVersion: transcode.CompactChainVersion}
	}
	return snapshot, nil
}

func validateModeInputs(inputs map[string]any) error {
	raw, present := inputs["mode"]
	if !present {
		if _, has := inputs["max_height"]; has {
			return inputErr("max_height", "max_height is only supported with mode=compact")
		}
		if _, err := modeHardwareInput("", inputs); err != nil {
			return err
		}
		if _, err := modeSlowPresetInput("", inputs); err != nil {
			return err
		}
		if _, err := modeSizeBudgetInput("", inputs); err != nil {
			return err
		}
		if _, err := modeTrackInput("", inputs); err != nil {
			return err
		}
		if _, err := modeAudioOnlyInput("", inputs); err != nil {
			return err
		}
		if _, err := modeSmallerSavingInput("", inputs); err != nil {
			return err
		}
		if _, err := modeKeepHDRInput("", inputs); err != nil {
			return err
		}
		return nil
	}
	mode, ok := raw.(string)
	if !ok {
		return inputErr("mode", "mode must be a string")
	}
	maxHeight, err := modeMaxHeightInput(inputs)
	if err != nil {
		return err
	}
	hardware, err := modeHardwareInput(mode, inputs)
	if err != nil {
		return err
	}
	slow, err := modeSlowPresetInput(mode, inputs)
	if err != nil {
		return err
	}
	budget, err := modeSizeBudgetInput(mode, inputs)
	if err != nil {
		return err
	}
	tracks, err := modeTrackInput(mode, inputs)
	if err != nil {
		return err
	}
	audioOnly, err := modeAudioOnlyInput(mode, inputs)
	if err != nil {
		return err
	}
	smallerSaving, err := modeSmallerSavingInput(mode, inputs)
	if err != nil {
		return err
	}
	keepHDR, err := modeKeepHDRInput(mode, inputs)
	if err != nil {
		return err
	}
	// Whether the global list or the job's own is in force is decided when the
	// goal freezes; here the inputs only have to be well formed.
	trackPolicy, err := trackPolicyFor(mode, tracks, nil)
	if err != nil {
		return attributeInput(inputKeepLanguages, err)
	}
	if _, err := modePolicyGoal(mode, getBool(inputs, "is_anime") || getString(inputs, "media_type") == "anime", maxHeight, hardware, slow, budget, trackPolicy, audioOnly, smallerSaving, keepHDR); err != nil {
		return attributeInput("mode", err)
	}
	for _, key := range []string{"profile", "profile_config", "priority", "metric", "min_savings_percent", "max_size_increase_percent", "preserve_source_bit_depth", "expected_video_codec", "shared_calibration", "calibration_items", "batch_fixed_quality", "batch_calibration_digest"} {
		if _, present := inputs[key]; present {
			return inputErr(key, "mode cannot be combined with %s; its quality, preservation and size policy is immutable", key)
		}
	}
	return nil
}

// modeMaxHeightInput reads the optional compact max_height input.
func modeMaxHeightInput(inputs map[string]any) (int, error) {
	raw, present := inputs["max_height"]
	if !present || raw == nil {
		return 0, nil
	}
	var v float64
	switch n := raw.(type) {
	case float64:
		v = n
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	default:
		return 0, inputErr("max_height", "max_height must be a number")
	}
	if v != math.Trunc(v) || !transcode.ValidCompactMaxHeight(int(v)) {
		return 0, inputErr("max_height", "max_height must be one of %v", transcode.CompactMaxHeights)
	}
	return int(v), nil
}

func freezeModePolicy(ec *ExecutionContext) (*ModePolicySnapshot, error) {
	mode := getString(ec.Inputs, "mode")
	if mode == "" {
		return nil, nil
	}
	if raw := ec.State["mode_policy"]; raw != nil {
		b, err := json.Marshal(raw)
		var p ModePolicySnapshot
		if err != nil || json.Unmarshal(b, &p) != nil || p.Mode != mode || p.Digest == "" {
			return nil, fmt.Errorf("frozen mode policy is unreadable or mismatched")
		}
		digest := p.Digest
		p.Digest = ""
		canonical, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(canonical)
		p.Digest = digest
		if "sha256:"+hex.EncodeToString(sum[:]) != digest {
			return nil, fmt.Errorf("frozen mode policy digest does not match its content")
		}
		if (p.Version == ModePolicyV4) != (p.Recipe != nil) || p.Recipe.validate(&p) != nil {
			return nil, fmt.Errorf("frozen mode policy recipe is missing or invalid")
		}
		if p.Tracks != nil && (p.Mode != "size" && p.Mode != "compact" || p.Tracks.Validate() != nil) {
			return nil, fmt.Errorf("frozen mode policy track selection is invalid")
		}
		// Only a v4 recipe carries the audio-only goal (validate checks the
		// recipe matches it).
		if p.Size != nil && p.Size.AudioOnly && p.Recipe == nil {
			return nil, fmt.Errorf("frozen audio-only goal has no recipe")
		}
		ec.State["policy_digest"] = p.Digest
		ec.State["mode"] = p.Mode
		ec.State["search_budget_seconds"] = p.SearchBudgetSeconds
		ec.State["operation_search_budget_seconds"] = p.OperationBudgetSeconds
		return &p, nil
	}
	maxHeight, err := modeMaxHeightInput(ec.Inputs)
	if err != nil {
		return nil, err
	}
	hardware, err := modeHardwareInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	slow, err := modeSlowPresetInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	budget, err := modeSizeBudgetInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	tracks, err := trackPolicyOf(ec, mode)
	if err != nil {
		return nil, err
	}
	audioOnly, err := modeAudioOnlyInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	smallerSaving, err := modeSmallerSavingInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	keepHDR, err := modeKeepHDRInput(mode, ec.Inputs)
	if err != nil {
		return nil, err
	}
	p, err := modePolicyGoal(mode, getBool(ec.State, "resolved_is_anime") || getBool(ec.Inputs, "is_anime") || getString(ec.Inputs, "media_type") == "anime", maxHeight, hardware, slow, budget, tracks, audioOnly, smallerSaving, keepHDR)
	if err != nil {
		return nil, err
	}
	ec.State["mode_policy"] = p
	ec.State["mode"] = mode
	ec.State["policy_digest"] = p.Digest
	ec.State["search_budget_seconds"] = p.SearchBudgetSeconds
	ec.State["operation_search_budget_seconds"] = p.OperationBudgetSeconds
	return &p, nil
}

// modeSourceReason classifies a source for an automatic mode. hfrSupported
// must come from the worker's proven decimated-VMAF capability; without it
// progressive 45-61 FPS sources are skipped as hfr_quality_unavailable (a new
// goal will not help; a capable worker will), and interlaced HFR-class
// sources and rates above that class stay unsupported. Compact mode accepts
// HDR and Dolby Vision; compactSourcePlan decides them from stream and frame
// evidence.
func modeSourceReason(mode string, rep *mediainspect.DetailedReport, hfrSupported bool) string {
	video := rep.MainVideo()
	if len(video) != 1 || video[0].BitDepth != 8 && video[0].BitDepth != 10 {
		return "unsupported_source"
	}
	// Only the compact goal converts HDR and Dolby Vision; a goal change can
	// pick these files up, unlike unsupported_source.
	if mode != "compact" && isSourceHDRorDV(rep) {
		return "hdr_requires_compact"
	}
	factor, err := quality.TemporalDecimationForStream(video[0].FPS, video[0].FieldOrder)
	if err != nil {
		return "unsupported_source"
	}
	if factor > 1 && !hfrSupported {
		return "hfr_quality_unavailable"
	}
	if mode == "x265_preserve" && strings.EqualFold(video[0].Codec, "hevc") {
		return "already_target_codec"
	}
	return ""
}

// audioOnlySourceReason classifies a source for the audio-only size goal. The
// video is copied, not encoded or measured, so HDR, Dolby Vision and any
// frame rate are fine; the source needs one programme video of a bit depth the
// copy proof reads (8 or 10).
func audioOnlySourceReason(rep *mediainspect.DetailedReport) string {
	video := rep.MainVideo()
	if len(video) != 1 || video[0].BitDepth != 8 && video[0].BitDepth != 10 {
		return "unsupported_source"
	}
	return ""
}

// hfrQualitySupport asks the worker at most once per step, and only when a
// source actually needs decimation. A failed lookup is returned, never
// cached as "unsupported": callers wait on retryable errors and fail closed
// on the rest.
//
// worker is the action's own worker. Without one (a batch classifying its
// files before any child is assigned a worker) every configured worker must
// support it, since a child may be assigned any of them.
type hfrQualitySupport struct {
	e         *Engine
	worker    transcode.Executor
	checked   bool
	supported bool
}

func (h *hfrQualitySupport) forSource(ctx context.Context, rep *mediainspect.DetailedReport) (bool, error) {
	if rep == nil {
		return false, nil
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return false, nil
	}
	if factor, err := quality.TemporalDecimationForStream(video[0].FPS, video[0].FieldOrder); err != nil || factor == 1 {
		return false, nil
	}
	if h.checked {
		return h.supported, nil
	}
	if h.e == nil || !h.e.workersConfigured() {
		h.checked = true
		return false, nil
	}
	workers := []transcode.Executor{h.worker}
	if h.worker == nil {
		workers = workers[:0]
		for _, w := range h.e.assignableWorkers(ctx) {
			workers = append(workers, w.Executor)
		}
	}
	supported := true
	for _, w := range workers {
		caps, err := w.Capabilities(ctx)
		if err != nil {
			return false, err
		}
		supported = supported && caps.SupportsHFRQuality()
	}
	h.checked, h.supported = true, supported
	return h.supported, nil
}

// hfrCapabilityLookupResult turns a failed HFR capability lookup into the
// step result: a transient worker error waits, anything else fails closed.
func hfrCapabilityLookupResult(err error) StepResult {
	return workerCapabilityLookupResult(err, "HFR quality")
}

func workerCapabilityLookupResult(err error, what string) StepResult {
	if isRetryableWorkerPollError(err) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_unreachable", WaitingReason: fmt.Sprintf("Worker capabilities temporarily unavailable: %v", err)}
	}
	return StepResult{Status: StepFailed, Error: fmt.Sprintf("querying worker %s capability: %v (fail closed)", what, err)}
}

// sizeMain10Supported reports a worker that can encode and measure an 8-bit
// source as Main10 under size v3: x265, explicit Main10 benchmarks and the
// proven native 10-bit VMAF and CAMBI path. Otherwise the source stays 8-bit.
func sizeMain10Supported(caps transcode.WorkerCapabilities) bool {
	return caps.SizeModeV3 && caps.ExplicitMain10Benchmarks && caps.SupportsNative10BitQuality() && caps.Encoders[transcode.VideoCodecLibX265]
}

// setMain10 switches a resolved plan to 10-bit HEVC output.
func setMain10(plan *transcode.Plan) error {
	if plan.VideoCodec != transcode.VideoCodecLibX265 {
		return fmt.Errorf("size Main10 requires a libx265 plan (fail closed)")
	}
	plan.VideoProfile, plan.PixelFormat, plan.ExpectedBitDepth = "main10", "p010le", 10
	plan.PlanDigest = ""
	digest, err := transcode.DigestPlan(plan)
	plan.PlanDigest = digest
	return err
}

func modeAttention(ec *ExecutionContext, reason, message string) StepResult {
	ec.State["reason_code"] = reason
	if ec.Decision == "cancel" || ec.Decision == "reject" {
		return StepResult{Status: StepCancelled, Outputs: map[string]any{"reason_code": "user_cancelled"}}
	}
	return StepResult{Status: StepWaitingDecision, WaitingCondition: reason, WaitingReason: message, WaitingOptions: []WaitingOption{{Decision: "cancel", Description: "Stop this operation and keep the original"}}, Outputs: map[string]any{"mode": getString(ec.Inputs, "mode"), "policy_digest": getString(ec.State, "policy_digest"), "reason_code": reason}}
}

func applyModePlan(plan *transcode.Plan, p *ModePolicySnapshot) error {
	plan.Mode = p.Mode
	plan.ModePolicyVersion = p.workerVersion()
	plan.PolicyDigest = p.Digest
	plan.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: p.MinSavingsPercent, MaxSizeIncreasePercent: p.MaxSizeIncreasePercent}
	plan.PlanDigest = ""
	d, err := transcode.DigestPlan(plan)
	plan.PlanDigest = d
	return err
}

func (e *Engine) bindModeParent(ec *ExecutionContext) error {
	if getString(ec.Inputs, "mode") == "" || getString(ec.Inputs, "parent_action_id") == "" {
		return nil
	}
	if e.deps.Store == nil {
		return fmt.Errorf("mode parent store is unavailable")
	}
	parent, err := e.deps.Store.GetActionInstance(getString(ec.Inputs, "parent_action_id"))
	if err != nil || parent == nil || parent.ActionName != "transcode_batch" {
		return fmt.Errorf("mode parent is unavailable")
	}
	var inputs, states map[string]any
	if json.Unmarshal([]byte(parent.InputsJSON), &inputs) != nil || json.Unmarshal([]byte(parent.StateJSON), &states) != nil {
		return fmt.Errorf("mode does not match frozen parent")
	}
	item, err := e.deps.Store.GetTranscodeBatchItem(parent.ID, getString(ec.Inputs, "batch_item_key"))
	if err != nil || item == nil || item.FilePath != getString(ec.Inputs, "path") || item.Decision != "transcode" {
		return fmt.Errorf("mode source is not a member of parent inventory")
	}
	mode, policy := getString(inputs, "mode"), states["mode_policy"]
	settings := stateObject(states, "batch_item_settings")
	if row, ok := settings[getString(ec.Inputs, "batch_item_key")].(map[string]any); ok && getString(row, "mode") != "" {
		mode = getString(row, "mode")
		policy = row["mode_policy"]
	}
	if mode != getString(ec.Inputs, "mode") {
		return fmt.Errorf("mode does not match frozen parent attempt")
	}
	ec.State["mode_policy"] = policy
	ec.State["policy_digest"] = getString(states, "policy_digest")
	ec.State["mode"] = mode
	ec.State["search_budget_seconds"] = getInt(states, "search_budget_seconds")
	ec.State["operation_search_budget_seconds"] = getInt(states, "operation_search_budget_seconds")
	return nil
}

// compactSourcePlan classifies the source from stream and decoded-frame
// evidence and derives its frozen compact plan. A non-empty reason is a
// user-facing skip, never a hard failure: a source whose frames cannot be
// probed is skipped so one bad file does not fail a whole batch.
func compactSourcePlan(ctx context.Context, ffprobe, path string, rep *mediainspect.DetailedReport, p *ModePolicySnapshot) (*transcode.CompactPlan, string) {
	return compactSourcePlanWith(ctx, rep, p, func(ctx context.Context, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
		return mediainspect.ClassifySource(ctx, ffprobe, path, rep)
	})
}

// compactSourcePlanWith is compactSourcePlan with the source classifier
// supplied, so a read-only preview can use the exact gate the job uses.
func compactSourcePlanWith(ctx context.Context, rep *mediainspect.DetailedReport, p *ModePolicySnapshot, classify func(context.Context, *mediainspect.DetailedReport) (mediainspect.DynamicRange, error)) (*transcode.CompactPlan, string) {
	if p == nil || p.Compact == nil || p.Compact.ChainVersion != transcode.CompactChainVersion {
		return nil, "compact_policy_unavailable"
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return nil, "hdr_single_video_stream_required"
	}
	d, err := classify(ctx, rep)
	if err != nil {
		return nil, "compact_frame_evidence_unavailable"
	}
	// Keep HDR applies to an HDR or Dolby Vision source only; an SDR one is
	// planned exactly as without it.
	chain := transcode.CompactChainVersion
	if p.Compact.KeepHDR && d.IsHDR() {
		chain = transcode.CompactHDRChainVersion
	}
	plan, err := transcode.NewCompactPlanForChain(chain, d, video[0], p.Compact.MaxHeight)
	if err != nil {
		return nil, err.Error()
	}
	return plan, ""
}

// compactInventoryReason screens a batch source from stream evidence alone.
// SDR sources are decided now (for example compact_not_needed); HDR and Dolby
// Vision need decoded-frame evidence, which each child preflight collects.
func compactInventoryReason(ec *ExecutionContext, rep *mediainspect.DetailedReport) string {
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil || p.Compact == nil {
		return "compact_policy_unavailable"
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return "hdr_single_video_stream_required"
	}
	d := mediainspect.ClassifyDynamicRange(rep, nil)
	if d.IsHDR() {
		return ""
	}
	if _, err := transcode.NewCompactPlan(d, video[0], p.Compact.MaxHeight); err != nil {
		return err.Error()
	}
	return ""
}
