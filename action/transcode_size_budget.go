package action

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// A size budget is the inverse of the size search. The search keeps quality
// fixed (VMAF mean 88, p5 84, worst 5 s window 85, CAMBI 6/16) and looks for
// the smallest file that meets it; a hard source (dark, grainy) can end with
// nothing that meets it, or with a passing file larger than the user accepts.
// With a budget the action then encodes the file once at the bitrate that
// lands on the budget (x265 two-pass ABR, which at an equal average bitrate
// matches CRF quality and, unlike CRF, hits a size), measures it on the same
// final validation windows, and reports it as below the quality target. A
// frozen floor refuses an outright bad result; the normal approval is still
// needed before anything replaces the original.

const (
	// sizeBudgetKey holds the SizeBudgetRecord of a fallback that started.
	sizeBudgetKey = "size_budget"
	// Why a frozen budget was not applied, when the search's outcome stood.
	sizeBudgetSkipReasonKey  = "size_budget_skip_reason"
	sizeBudgetSkipMessageKey = "size_budget_skip_message"
	// sizeBudgetPlanSource marks the encode plan as the budget fallback's.
	sizeBudgetPlanSource = "size_budget"

	sizeBudgetMinPercent = 5
	sizeBudgetMaxPercent = 85 // the size mode's 15% minimum savings
	// sizeBudgetMinBytes keeps an absolute budget from being a typo for MB.
	sizeBudgetMinBytes = 50_000_000

	// sizeBudgetFillPercent is the share of the budget the encode aims at.
	// Two-pass x265 lands within a few percent of its average bitrate over a
	// long file; the rest is headroom so the result stays inside the budget.
	sizeBudgetFillPercent = 97

	// The fallback's trigger, as recorded.
	sizeBudgetTriggerNoWinner   = "no_passing_candidate"
	sizeBudgetTriggerOverBudget = "passing_candidate_over_budget"
	// sizeBudgetTriggerUnreachable marks a budget the other streams alone
	// use up, ended before any encode or search.
	sizeBudgetTriggerUnreachable = "budget_unreachable"

	// Outcomes after final validation.
	SizeBudgetBelowTarget = "below_quality_target_within_budget"
	SizeBudgetMeetsTarget = "meets_quality_target_within_budget"
	SizeBudgetBelowFloor  = "below_quality_floor"
	SizeBudgetExceeded    = "size_budget_exceeded"
)

// sizeBudgetFloor is the quality floor of a budget fallback, in VMAF
// (v1_1080p_3h). VMAF maps roughly linearly onto the five-point ACR scale, about
// 20 points per category (80 good, 60 fair, 40 poor). The floor refuses a result
// worse than "fair": every 10 s window and the pooled mean at least 70 (between
// fair and good), no 5 s stretch below 65 and the worst 5% of frames no lower
// than 60. That is about four just-noticeable differences (about 6 VMAF each)
// under the size mode's limits of 88/84/85, low enough that a dark, grainy
// source whose VMAF is harsh on grain can still meet a budget of a third of its
// size, and high enough to refuse a broken encode. Banding is the artefact a
// low bitrate produces on dark content, so the CAMBI limits are not lowered.
var sizeBudgetFloor = SizeBudgetFloor{VMAFMean: 70, VMAFP5: 60, VMAFWorstWindow: 65}

// sizeBudgetX265 are the bounded two-pass settings of the fallback encode:
// x265 medium's reference and B-frame counts with a longer lookahead for the
// rate control, aq-mode 3 (variance AQ biased to dark scenes, which the default
// mode starves of bits) and psy-rdoq 1 to keep grain from being smoothed away.
var sizeBudgetX265 = transcode.X265Settings{TwoPass: true, Lookahead: 40, BFrames: 4, RefFrames: 3, AQMode: 3, AQStrength: 1, PsyRD: 2, PsyRDOQ: 1}

const sizeBudgetPreset = "medium"

// SizeBudgetFloor is the minimum a budget fallback must measure on the final
// validation windows to be offered at all.
type SizeBudgetFloor struct {
	VMAFMean        float64 `json:"vmaf_mean_minimum"`
	VMAFP5          float64 `json:"vmaf_p5_minimum"`
	VMAFWorstWindow float64 `json:"vmaf_worst_window_minimum"`
}

// SizeBudgetPolicy is frozen in a size snapshot when the operation sets a
// budget. Exactly one of Percent (of the source file), Bytes and Siblings (the
// rules that derive it from the files beside the source) is set.
type SizeBudgetPolicy struct {
	Percent     float64                `json:"percent,omitempty"`
	Bytes       int64                  `json:"bytes,omitempty"`
	Siblings    *SiblingBudgetRules    `json:"siblings,omitempty"`
	FillPercent float64                `json:"fill_percent"`
	Preset      string                 `json:"preset"`
	X265        transcode.X265Settings `json:"x265"`
	Floor       SizeBudgetFloor        `json:"quality_floor"`
	// Comparison is set when the operation allows the media engine: the
	// fallback then measures VideoToolbox beside two-pass x265 at the budget
	// and encodes with the one it picks. Absent, the fallback is x265 only,
	// which is what snapshots frozen before the comparison carry.
	Comparison *SizeBudgetComparison `json:"encoder_comparison,omitempty"`
	// VTLadder is set instead of Comparison when the operation allows the media
	// engine: the budget then skips the search and encodes the whole file with
	// VideoToolbox at each of these constant-quality values in turn (highest
	// first), then at the budget's average bitrate. The first encode within the
	// budget is the one validated.
	VTLadder []int `json:"vt_quality_ladder,omitempty"`
	// Audio is set by policy v5, which every new budget freezes: the audio
	// levels the plan may use and what counts as good video (see
	// transcode_size_budget_audio.go). Absent, the budget keeps the one audio
	// policy snapshots frozen before it carry: only lossless audio converted.
	Audio *SizeBudgetAudio `json:"audio,omitempty"`
}

// sizeBudgetVTLadder is the quality ladder new snapshots freeze: VideoToolbox
// -q:v values tried from the best picture down before the average-bitrate rung.
var sizeBudgetVTLadder = []int{65, 58, 50}

func (b *SizeBudgetPolicy) Clone() *SizeBudgetPolicy {
	if b == nil {
		return nil
	}
	cp := *b
	if b.Siblings != nil {
		s := *b.Siblings
		cp.Siblings = &s
	}
	if b.Comparison != nil {
		c := *b.Comparison
		cp.Comparison = &c
	}
	cp.VTLadder = slices.Clone(b.VTLadder)
	if b.Audio != nil {
		a := *b.Audio
		a.Modes = slices.Clone(a.Modes)
		cp.Audio = &a
	}
	return &cp
}

// Validate checks a frozen budget, which comes back from stored state.
func (b *SizeBudgetPolicy) Validate() error {
	if b == nil {
		return nil
	}
	set := 0
	for _, on := range []bool{b.Percent > 0, b.Bytes > 0, b.Siblings != nil} {
		if on {
			set++
		}
	}
	if set != 1 || b.Percent < 0 || b.Bytes < 0 {
		return fmt.Errorf("size budget needs exactly one of a percent, a byte count and the sibling files")
	}
	if err := b.Siblings.validate(); err != nil {
		return err
	}
	if err := b.Comparison.validate(); err != nil {
		return err
	}
	if err := b.Audio.validate(); err != nil {
		return err
	}
	if len(b.VTLadder) > 0 {
		if b.Comparison != nil {
			return fmt.Errorf("size budget cannot have both a VideoToolbox ladder and an encoder comparison")
		}
		if len(b.VTLadder) > 4 {
			return fmt.Errorf("invalid size budget VideoToolbox ladder")
		}
		for i, q := range b.VTLadder {
			if q < 1 || q > 100 || (i > 0 && q >= b.VTLadder[i-1]) {
				return fmt.Errorf("invalid size budget VideoToolbox ladder")
			}
		}
	}
	if b.Percent > 0 && (b.Percent < sizeBudgetMinPercent || b.Percent > sizeBudgetMaxPercent || math.IsNaN(b.Percent)) {
		return fmt.Errorf("size_budget_percent must be between %d and %d", sizeBudgetMinPercent, sizeBudgetMaxPercent)
	}
	if b.Bytes > 0 && b.Bytes < sizeBudgetMinBytes {
		return fmt.Errorf("size_budget_bytes must be at least %d", sizeBudgetMinBytes)
	}
	if b.FillPercent <= 50 || b.FillPercent > 100 || !transcode.IsValidLibX265Preset(b.Preset) {
		return fmt.Errorf("invalid size budget encode settings")
	}
	x := b.X265
	x.BitrateKbps = 64
	if err := x.Validate(transcode.VideoCodecLibX265, 0); err != nil {
		return err
	}
	f := b.Floor
	for _, v := range []float64{f.VMAFMean, f.VMAFP5, f.VMAFWorstWindow} {
		if math.IsNaN(v) || v <= 0 || v > 100 {
			return fmt.Errorf("invalid size budget quality floor")
		}
	}
	return nil
}

// BudgetBytes is the budget for a source of sourceBytes: the absolute budget,
// the percent of the source or what the siblings allow, never more than the
// size mode's minimum saving allows (the worker enforces that minimum on every
// size plan). It is zero, no budget, when the siblings gave none.
func (b *SizeBudgetPolicy) BudgetBytes(sourceBytes int64, siblings *SiblingBudgetEvidence) int64 {
	if b == nil || sourceBytes <= 0 {
		return 0
	}
	ceiling := sizeBudgetCeiling(sourceBytes)
	budget := b.Bytes
	switch {
	case b.Percent > 0:
		budget = int64(math.Floor(float64(sourceBytes) * b.Percent / 100))
	case b.Siblings != nil:
		if siblings == nil || siblings.Reason != "" || siblings.BudgetBytes <= 0 || siblings.Multiplier != b.Siblings.Multiplier {
			return 0
		}
		budget = siblings.BudgetBytes
	}
	return min(budget, ceiling)
}

// sizeBudgetCeiling is the largest budget a source can have: the size mode
// must save at least 15%.
func sizeBudgetCeiling(sourceBytes int64) int64 {
	return (sourceBytes/100)*sizeBudgetMaxPercent + ((sourceBytes%100)*sizeBudgetMaxPercent)/100
}

// modeSizeBudgetInput reads size_budget_percent or size_budget_bytes for a
// mode: size mode only, off unless one is set.
func modeSizeBudgetInput(mode string, inputs map[string]any) (*SizeBudgetPolicy, error) {
	number := func(key string) (float64, bool, error) {
		raw, has := inputs[key]
		if !has || raw == nil {
			return 0, false, nil
		}
		switch n := raw.(type) {
		case float64:
			return n, true, nil
		case int:
			return float64(n), true, nil
		case int64:
			return float64(n), true, nil
		case json.Number:
			f, err := n.Float64()
			return f, err == nil, err
		}
		return 0, false, inputErr(key, "%s must be a number", key)
	}
	percent, hasPercent, err := number("size_budget_percent")
	if err != nil {
		return nil, err
	}
	bytes, hasBytes, err := number("size_budget_bytes")
	if err != nil {
		return nil, err
	}
	multiplier, hasMultiplier, err := number("size_budget_sibling_multiplier")
	if err != nil {
		return nil, err
	}
	source, hasSource := inputs["size_budget_source"]
	if source == nil {
		hasSource = false
	}
	if !hasPercent && !hasBytes && !hasSource {
		if hasMultiplier {
			return nil, inputErr("size_budget_sibling_multiplier", "size_budget_sibling_multiplier requires size_budget_source=siblings")
		}
		return nil, nil
	}
	if mode != "size" {
		return nil, inputErr(firstPresent(inputs, "size_budget_percent", "size_budget_bytes", "size_budget_source"), "size_budget_percent, size_budget_bytes and size_budget_source are only supported with mode=size")
	}
	if (hasPercent && hasBytes) || (hasSource && (hasPercent || hasBytes)) {
		return nil, inputErr(firstPresent(inputs, "size_budget_bytes", "size_budget_source"), "set only one of size_budget_percent, size_budget_bytes and size_budget_source")
	}
	if hasMultiplier && !hasSource {
		return nil, inputErr("size_budget_sibling_multiplier", "size_budget_sibling_multiplier requires size_budget_source=siblings")
	}
	b := &SizeBudgetPolicy{FillPercent: sizeBudgetFillPercent, Preset: sizeBudgetPreset, X265: sizeBudgetX265, Floor: sizeBudgetFloor, Audio: newSizeBudgetAudio()}
	if hasSource {
		if source != "siblings" {
			return nil, inputErr("size_budget_source", `size_budget_source must be "siblings"`)
		}
		if !hasMultiplier {
			multiplier = siblingBudgetDefaultMultiplier
		}
		b.Siblings = &SiblingBudgetRules{Multiplier: multiplier, MinimumSiblings: siblingBudgetMinimum, MaxProbed: siblingBudgetMaxProbed}
	} else if hasPercent {
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < sizeBudgetMinPercent || percent > sizeBudgetMaxPercent {
			return nil, inputErr("size_budget_percent", "size_budget_percent must be between %d and %d (size mode saves at least 15%%)", sizeBudgetMinPercent, sizeBudgetMaxPercent)
		}
		b.Percent = percent
	} else {
		if math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes != math.Trunc(bytes) || bytes < sizeBudgetMinBytes || bytes > math.MaxInt64/2 {
			return nil, inputErr("size_budget_bytes", "size_budget_bytes must be a whole number of bytes, at least %d", sizeBudgetMinBytes)
		}
		b.Bytes = int64(bytes)
	}
	if err := b.Validate(); err != nil {
		return nil, attributeInput(b.field(), err)
	}
	return b, nil
}

// SizeBudgetPoint is one point of the rate-quality curve a budget fallback
// shows: a setting the search measured (its sampled size estimate and VMAF) or
// the fallback itself (its planned or actual size and its final VMAF).
type SizeBudgetPoint struct {
	// Kind is "searched", "budget_measured" (an encoder the fallback measured
	// at the budget on the search's windows), "budget_overshoot" (a
	// VideoToolbox encode over the budget, re-encoded at a lower bitrate),
	// "budget_rung_over" (a VideoToolbox ladder step over the budget, the next
	// step encodes instead) or "budget_fallback".
	Kind         string   `json:"kind"`
	CandidateID  string   `json:"candidate_id,omitempty"`
	VideoCodec   string   `json:"video_codec,omitempty"`
	Quality      int      `json:"quality,omitempty"`
	Preset       string   `json:"preset,omitempty"`
	BitrateKbps  int      `json:"bitrate_kbps,omitempty"`
	Bytes        int64    `json:"bytes"`
	Measured     bool     `json:"measured"` // actual bytes and final windows, not an estimate
	Passed       bool     `json:"passed"`   // met the size mode's quality limits
	WithinBudget bool     `json:"within_budget"`
	VMAFMean     *float64 `json:"vmaf_mean,omitempty"`
	VMAFP5       *float64 `json:"vmaf_p5,omitempty"`
	VMAFWorst    *float64 `json:"vmaf_worst_window,omitempty"`
	// Chosen marks the measured encoder the fallback encodes with.
	Chosen bool `json:"chosen,omitempty"`
}

// SizeBudgetRecord is what a budget fallback planned and, after the encode,
// what it measured.
type SizeBudgetRecord struct {
	Trigger       string  `json:"trigger"`
	SourceBytes   int64   `json:"source_bytes"`
	BudgetBytes   int64   `json:"budget_bytes"`
	BudgetPercent float64 `json:"budget_percent,omitempty"`
	// Siblings is the evidence a sibling budget came from.
	Siblings *SiblingBudgetEvidence `json:"siblings,omitempty"`
	// TargetBytes is what the encode aims at (FillPercent of the budget) and
	// NonVideoBytes what the audio, subtitles, attachments and container take.
	TargetBytes      int64   `json:"target_bytes"`
	NonVideoBytes    int64   `json:"non_video_bytes"`
	VideoBitrateKbps int     `json:"video_bitrate_kbps"`
	DurationSeconds  float64 `json:"duration_seconds"`
	// Audio is the level the plan uses and each track's planned codec and
	// bitrate; absent for a budget frozen before the levels.
	Audio *SizeBudgetAudioRecord `json:"audio,omitempty"`
	// OverBudgetCandidateID is the passing setting the budget ruled out.
	OverBudgetCandidateID    string `json:"over_budget_candidate_id,omitempty"`
	OverBudgetEstimatedBytes int64  `json:"over_budget_estimated_bytes,omitempty"`

	Target QualityLimits     `json:"quality_target"`
	Floor  QualityLimits     `json:"quality_floor"`
	Curve  []SizeBudgetPoint `json:"curve,omitempty"`
	// Choice is the encoder the fallback uses and why; Correction a
	// VideoToolbox encode that came out over the budget and was encoded again
	// at a lower bitrate.
	Choice     *SizeBudgetChoice     `json:"encoder_choice,omitempty"`
	Correction *SizeBudgetCorrection `json:"bitrate_correction,omitempty"`
	// Rung is the VideoToolbox ladder step being encoded (see
	// transcode_size_budget_ladder.go); zero, and omitted, otherwise.
	Rung int `json:"vt_rung,omitempty"`

	// Set after the encode.
	Outcome        string               `json:"outcome,omitempty"`
	CandidateBytes int64                `json:"candidate_bytes,omitempty"`
	Final          *FinalQualitySummary `json:"final,omitempty"`
	// BelowTarget lists the size mode's limits the result misses, in words.
	BelowTarget []string `json:"below_target,omitempty"`
	// RetryWithoutHardware is set when a VideoToolbox result missed the floor:
	// the same goal with allow_hardware off encodes with x265, which is slower
	// and usually keeps more quality at the same size.
	RetryWithoutHardware bool   `json:"retry_without_hardware,omitempty"`
	Message              string `json:"message"`
}

func sizeBudgetOf(state map[string]any) *SizeBudgetRecord {
	if state[sizeBudgetKey] == nil {
		return nil
	}
	var r SizeBudgetRecord
	if !reencode(state[sizeBudgetKey], &r) || r.BudgetBytes <= 0 {
		return nil
	}
	return &r
}

// sizeBudgetActive reports a plan built by the budget fallback.
func sizeBudgetActive(state map[string]any) bool {
	return getString(state, "plan_source") == sizeBudgetPlanSource && sizeBudgetOf(state) != nil
}

// sizeBudgetNonVideoBytes is what everything but the main video takes in the
// output under an audio mode: the source minus its video stream, minus what
// converting the audio saves. A converted track whose source size is unknown
// saves nothing here, which errs towards a smaller file. It is unknown (false)
// when the video stream's own size cannot be read and a copied stream states
// no size either.
func sizeBudgetNonVideoBytes(rep *mediainspect.DetailedReport, sourceBytes int64, audioMode string) (int64, bool) {
	if rep == nil || sourceBytes <= 0 || rep.DurationSec <= 0 {
		return 0, false
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return 0, false
	}
	videoBytes := audioStreamBytes(video[0], rep.DurationSec, sourceBytes)
	if videoBytes <= 0 {
		// Files muxed without Matroska statistics give no video size. The
		// output's other streams are sized directly instead: converted audio
		// is re-encoded at a known bitrate, whatever its source size.
		return sizeBudgetOtherStreamBytes(rep, sourceBytes, audioMode)
	}
	if videoBytes >= sourceBytes {
		return 0, false
	}
	return estimateAudioOnly(rep, sourceBytes, audioMode).CandidateBytes - videoBytes, true
}

// Upper bounds for streams that state no size of their own: cover art is a
// few megabytes, and the container's own overhead (cues, cluster headers) is
// under half a percent of a Matroska file. Text subtitles use the estimator's
// bound.
const (
	sizeBudgetCoverArtBound    = 5_000_000
	sizeBudgetOverheadPerMille = 5
)

// sizeBudgetOtherStreamBytes is what everything but the main video takes in
// the output, rounded up where a stream states no size. Lossless audio is
// converted at the compact bitrate, so its source size does not matter (FLAC
// in Matroska often states none). Copied audio and bitmap subtitles must state
// one (they can be gigabytes); text subtitles and cover art are bounded and
// attachments carry their size. Overestimating these streams plans a smaller
// video, never a larger one.
func sizeBudgetOtherStreamBytes(rep *mediainspect.DetailedReport, sourceBytes int64, audioMode string) (int64, bool) {
	total := sourceBytes * sizeBudgetOverheadPerMille / 1000
	for _, a := range rep.Audio {
		target, kbps := transcode.AudioTarget(audioMode, audioTrackOf(a, rep.DurationSec))
		if target != "copy" {
			total += int64(kbps) * 1000 * int64(math.Ceil(rep.DurationSec)) / 8
			continue
		}
		n := audioTrackBytes(a, rep.DurationSec, sourceBytes)
		if n <= 0 {
			return 0, false
		}
		total += n
	}
	for _, s := range rep.Subtitles {
		n := audioStreamBytes(s, rep.DurationSec, sourceBytes)
		if n <= 0 {
			if !isTextSubtitleCodec(s.Codec) {
				return 0, false
			}
			n = defaultTextSubtitleSizeBytes
		}
		total += n
	}
	for _, a := range rep.Attachments {
		total += max(a.ExtraDataSize, 0)
	}
	total += int64(len(rep.CoverArt())) * sizeBudgetCoverArtBound
	return total, total < sourceBytes
}

// defaultTextSubtitleSizeBytes is a conservative 2 MiB allowance per text
// subtitle stream when ffprobe has no byte count. This is an estimate, not a
// bound. Bitmap subtitles and unknown codecs need a measured size because
// their payload can be significant.
const defaultTextSubtitleSizeBytes int64 = 2 * 1024 * 1024

// isTextSubtitleCodec reports a text subtitle codec, whose size is bounded
// by defaultTextSubtitleSizeBytes when the file does not state it.
func isTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "srt", "subrip", "mov_text", "ass", "ssa", "webvtt":
		return true
	default:
		return false
	}
}

// sizeBudgetVideoKbps is the average video bitrate that lands the whole file
// on target bytes.
func sizeBudgetVideoKbps(targetBytes, nonVideoBytes int64, duration float64) int {
	if targetBytes <= nonVideoBytes || duration <= 0 {
		return 0
	}
	return int(math.Floor(float64(targetBytes-nonVideoBytes) * 8 / duration / 1000))
}

// sizeBudgetFloorQuality is the final validation config of a budget fallback:
// the frozen size config with the VMAF minimums lowered to the floor. Banding,
// the model and the window rules are unchanged.
func sizeBudgetFloorQuality(snapshot *ModePolicySnapshot) *transcode.BenchmarkQualityConfig {
	if snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil {
		return nil
	}
	qc := ensureFinalQualityMetric(buildFinalQualityConfig(snapshot.Profile.Optimization.Quality), "vmaf")
	if qc.VMAF == nil {
		return nil
	}
	v := *qc.VMAF
	f := snapshot.Size.SizeBudget.Floor
	p5, worst := f.VMAFP5, f.VMAFWorstWindow
	v.Minimum, v.P5Minimum, v.WorstWindowMinimum = f.VMAFMean, &p5, &worst
	qc.VMAF = &v
	return qc
}

// sizeBudgetEncode is the encode the frozen budget gives a source: the size it
// aims at, what the other streams take, and the x265 settings at the bitrate
// that leaves. Reason is set when none can be planned. Validation rebuilds it
// to check the plan it accepts.
type sizeBudgetEncode struct {
	TargetBytes   int64
	NonVideoBytes int64
	X265          transcode.X265Settings
	// AudioMode is the audio level the encode plans with; Audio records it and
	// each track's plan (nil for a budget frozen before the levels).
	AudioMode string
	Audio     *SizeBudgetAudioRecord
	Reason    string
	Message   string
}

// planSizeBudgetEncode chooses the audio level (see
// transcode_size_budget_audio.go) and plans the video bitrate that leaves.
func planSizeBudgetEncode(policy *SizeBudgetPolicy, rep *mediainspect.DetailedReport, sourceBytes, budget int64) sizeBudgetEncode {
	if policy == nil || rep == nil || budget <= 0 || rep.DurationSec <= 0 {
		return sizeBudgetEncode{Reason: "size_budget_estimate_unavailable", Message: "The source's size or duration could not be read, so no bitrate for the size budget could be planned."}
	}
	modes := policy.audioModes()
	good := policy.Audio.goodVideoKbps(rep, sourceBytes)
	var last, usable sizeBudgetEncode
	previous := 0
	for i, mode := range modes {
		enc := planSizeBudgetEncodeAt(policy, rep, sourceBytes, budget, mode)
		if policy.Audio != nil {
			enc.Audio = sizeBudgetAudioRecord(rep, mode, i+1, good, previous)
		}
		if enc.Reason == "" {
			if i == len(modes)-1 || enc.X265.BitrateKbps >= good {
				return enc
			}
			usable, previous = enc, enc.X265.BitrateKbps
		}
		last = enc
	}
	if usable.AudioMode != "" {
		return usable
	}
	return last
}

// planSizeBudgetEncodeAt is the encode at one audio mode: the size it aims at,
// what the other streams take and the x265 settings at the bitrate that leaves.
func planSizeBudgetEncodeAt(policy *SizeBudgetPolicy, rep *mediainspect.DetailedReport, sourceBytes, budget int64, audioMode string) sizeBudgetEncode {
	enc := sizeBudgetEncode{AudioMode: audioMode}
	nonVideo, known := sizeBudgetNonVideoBytes(rep, sourceBytes, audioMode)
	if !known {
		enc.Reason, enc.Message = "size_budget_estimate_unavailable", "The size of the source's video stream could not be read, so no bitrate for the size budget could be planned."
		return enc
	}
	enc.NonVideoBytes = nonVideo
	enc.TargetBytes = int64(math.Floor(float64(budget) * policy.FillPercent / 100))
	enc.X265 = policy.X265
	// A budget roomier than two-pass x265 can use is met at its ceiling; the
	// file then simply comes out smaller than the budget.
	enc.X265.BitrateKbps = min(sizeBudgetVideoKbps(enc.TargetBytes, nonVideo, rep.DurationSec), transcode.X265MaxBitrateKbps)
	if enc.X265.Validate(transcode.VideoCodecLibX265, 0) != nil {
		enc.Reason, enc.Message = "size_budget_unreachable", fmt.Sprintf("The audio, subtitles and other streams already take %s of the %s budget, which leaves no usable video bitrate (%d kbps).", formatBudgetBytes(nonVideo), formatBudgetBytes(budget), enc.X265.BitrateKbps)
	}
	return enc
}

// sizeBudgetMinimumBudget is the smallest budget the encoder can plan a video
// bitrate for: the other streams plus the encoder's lowest bitrate over the
// duration, scaled up by the share of the budget the encode aims at.
func sizeBudgetMinimumBudget(policy *SizeBudgetPolicy, nonVideoBytes int64, duration float64) int64 {
	video := math.Ceil(float64(transcode.X265MinBitrateKbps) * 1000 / 8 * duration)
	return int64(math.Ceil((float64(nonVideoBytes) + video) * 100 / policy.FillPercent))
}

// endUnreachableSizeBudget ends a size goal whose budget cannot hold the file's
// audio, subtitles and other streams plus a usable video bitrate. No encode at
// that budget exists, so neither the ladder nor the search (which can only
// find a larger file) could meet it: the action ends here with no candidate
// and nothing submitted, reported as a size rejection. Any other reason a
// budget cannot be planned leaves the search's own outcome standing.
//
// Preflight asks before the source is hashed, with the budgets that need no
// sibling evidence (a sibling budget has none yet and is asked again by the
// benchmark submit step, which has it).
func endUnreachableSizeBudget(ec *ExecutionContext, snapshot *ModePolicySnapshot, rep *mediainspect.DetailedReport, original int64, siblings *SiblingBudgetEvidence) (StepResult, bool) {
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" {
		return StepResult{}, false
	}
	if snapshot == nil || !transcode.IsSizePolicyVersion(snapshot.workerVersion()) || snapshot.Size == nil || snapshot.Size.SizeBudget == nil {
		return StepResult{}, false
	}
	policy := snapshot.Size.SizeBudget
	if policy.Validate() != nil {
		return StepResult{Status: StepFailed, Error: "frozen size budget is invalid (fail closed)"}, true
	}
	budget := policy.BudgetBytes(original, siblings)
	if rep == nil || budget <= 0 {
		return StepResult{}, false
	}
	keptRep, keptBytes := keptViewOf(ec.State, rep, original)
	enc := planSizeBudgetEncode(policy, keptRep, keptBytes, budget)
	if enc.Reason != "size_budget_unreachable" {
		return StepResult{}, false
	}
	// No video bitrate fits, but copying the video and compacting the lossless
	// audio may: that answer is still the search's to give.
	if snapshot.Size.AudioOnlyFallback {
		if _, _, fits := sizeBudgetAudioOnlyLevel(policy, keptRep, keptBytes, budget); fits {
			return StepResult{}, false
		}
	}
	rec := newSizeBudgetRecord(snapshot, rep, original, budget, siblings, enc)
	rec.Trigger, rec.Outcome = sizeBudgetTriggerUnreachable, SizeBudgetExceeded
	rec.Message = fmt.Sprintf("The audio, subtitles and other streams alone take %s, which leaves no usable video bitrate within the %s budget. ", formatBudgetBytes(enc.NonVideoBytes), formatBudgetBytes(budget))
	// BudgetBytes never exceeds the size mode's ceiling, so a larger minimum
	// cannot be fixed by raising the budget.
	if minimum := sizeBudgetMinimumBudget(policy, enc.NonVideoBytes, rep.DurationSec); minimum > sizeBudgetCeiling(original) {
		rec.Message += "No budget can work for this file in size mode; remove audio or subtitle tracks first. Original preserved."
	} else {
		rec.Message += fmt.Sprintf("At least %s would be needed for this file; raise the budget, or remove audio or subtitle tracks first. Original preserved.", formatBudgetBytesUp(minimum))
	}
	ec.State[sizeBudgetKey] = rec
	ec.State["skip_transcode"] = true
	ec.State["reason_code"] = SizeBudgetExceeded
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": SizeBudgetExceeded, "mode": snapshot.Mode, "policy_digest": snapshot.Digest, sizeBudgetKey: rec}}, true
}

// commitSizeBudgetPlan builds the budget encode with the encoder rec.Choice
// names (x265 without one) and makes it the action's plan.
func (e *Engine) commitSizeBudgetPlan(ec *ExecutionContext, snapshot *ModePolicySnapshot, enc sizeBudgetEncode, rec *SizeBudgetRecord, outputs map[string]any) (StepResult, bool) {
	video, err := sizeBudgetVideo(snapshot, getSourceReport(ec.State["source_report"]), getPlan(ec.State["plan"]), enc, rec)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the size budget plan: %v", err)}, true
	}
	plan, err := e.sizeBudgetPlan(ec, snapshot, video, rec)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the size budget plan: %v", err)}, true
	}
	planned := SizeBudgetPoint{Kind: "budget_fallback", VideoCodec: transcode.NormalizeVideoCodec(plan.VideoCodec), Quality: plan.Quality, Preset: plan.Preset, BitrateKbps: sizeBudgetPlanKbps(plan), Bytes: rec.TargetBytes, WithinBudget: true}
	rec.Curve = append(rec.Curve, planned)
	rec.Message = sizeBudgetPlannedMessage(rec, snapshot.Size.SizeBudget, plan)
	ec.State[sizeBudgetKey] = rec
	outputs[sizeBudgetKey] = rec
	ec.State["plan"] = plan
	ec.State["plan_source"] = sizeBudgetPlanSource
	ec.State["plan_digest"] = plan.PlanDigest
	outputs["plan"] = plan
	outputs["plan_source"] = sizeBudgetPlanSource
	outputs["plan_digest"] = plan.PlanDigest
	outputs["plan_note"] = rec.Message
	return StepResult{Status: StepCompleted, Outputs: outputs}, true
}

// sizeBudgetPlan is the search's starting plan encoded with the budget's video
// settings (two-pass x265, or VideoToolbox at an average bitrate or a constant
// quality), with final validation on the size mode's windows held to the floor.
// A ladder rung also carries the budget as the worker's byte ceiling.
func (e *Engine) sizeBudgetPlan(ec *ExecutionContext, snapshot *ModePolicySnapshot, video sizeBudgetVideoSettings, rec *SizeBudgetRecord) (*transcode.Plan, error) {
	base := getPlan(ec.State["plan"])
	if base == nil || base.Mode != "size" || base.PolicyDigest != snapshot.Digest {
		return nil, fmt.Errorf("the frozen size plan is missing")
	}
	request, digest, samples, freshWindows, err := finalValidationInputs(ec)
	if err != nil {
		return nil, err
	}
	floor := sizeBudgetFloorQuality(snapshot)
	if floor == nil {
		return nil, fmt.Errorf("the size budget quality floor is missing")
	}
	p := *base
	video.apply(&p)
	if rec.Audio != nil {
		p.AudioMode = rec.Audio.Mode
	}
	if sizeBudgetIsLadder(rec) && base.SizePolicy != nil {
		sp := *base.SizePolicy
		sp.MaxCandidateBytes = rec.BudgetBytes
		p.SizePolicy = &sp
	}
	p.QualityValidation = &transcode.QualityValidationPlan{Metric: request.Metric, Samples: samples, FreshSamples: freshWindows, Quality: *floor, BenchmarkRequestDigest: digest}
	p.PlanDigest = ""
	d, err := transcode.DigestPlan(&p)
	if err != nil {
		return nil, err
	}
	p.PlanDigest = d
	return &p, nil
}

// finishSizeBudget records a validated budget candidate: its size against the
// budget and its final numbers against the size mode's limits. It reports
// false when the candidate is larger than the budget, which ends the action
// with the original kept.
func finishSizeBudget(ec *ExecutionContext, candidateBytes int64) (*SizeBudgetRecord, bool) {
	rec := sizeBudgetOf(ec.State)
	if rec == nil {
		return nil, true
	}
	rec.CandidateBytes = candidateBytes
	var evidence transcode.FinalQualityEvidence
	if reencode(ec.State["quality_evidence"], &evidence) && evidence.Verdict != "" {
		rec.Final = summarizeFinalQuality(&evidence, &rec.Target)
		rec.BelowTarget = sizeBudgetMissedTarget(rec.Final.Measured, rec.Target)
	}
	point := sizeBudgetEncodedPoint(ec, rec)
	point.Bytes, point.Measured, point.WithinBudget = candidateBytes, true, candidateBytes <= rec.BudgetBytes
	if rec.Final != nil {
		point.VMAFMean, point.VMAFP5, point.VMAFWorst = rec.Final.Measured.VMAFMean, rec.Final.Measured.VMAFP5, rec.Final.Measured.VMAFWorstWindow
		point.Passed = len(rec.BelowTarget) == 0
	}
	curve := rec.Curve[:0:0]
	for _, p := range rec.Curve {
		if p.Kind != "budget_fallback" {
			curve = append(curve, p)
		}
	}
	rec.Curve = append(curve, point)
	fits := candidateBytes > 0 && candidateBytes <= rec.BudgetBytes
	switch {
	case !fits:
		rec.Outcome = SizeBudgetExceeded
		rec.Message = fmt.Sprintf("The budget encode came out at %s, over the %s budget. Original preserved.", formatBudgetBytes(candidateBytes), formatBudgetBytes(rec.BudgetBytes))
		if sizeBudgetIsLadder(rec) {
			// Two-pass x265 lands on its average bitrate where VideoToolbox,
			// with one pass, can miss it.
			rec.RetryWithoutHardware = true
			rec.Message = fmt.Sprintf("All %d VideoToolbox steps came out over the %s budget (the last, %s, at %s). Original preserved. Try again with x265 (allow_hardware off): its two passes land on the budget.", rec.Rung+1, formatBudgetBytes(rec.BudgetBytes), sizeBudgetRungDescription(getPlan(ec.State["plan"])), formatBudgetBytes(candidateBytes))
		}
	case rec.Final != nil && len(rec.BelowTarget) == 0:
		rec.Outcome = SizeBudgetMeetsTarget
		rec.Message = fmt.Sprintf("Within your size budget: %s of %s, and it measured within this goal's quality limits after all. Replacing the original still needs your approval.", formatBudgetBytes(candidateBytes), formatBudgetBytes(rec.BudgetBytes))
	default:
		rec.Outcome = SizeBudgetBelowTarget
		missed := "the quality target"
		if len(rec.BelowTarget) > 0 {
			missed = joinWords(rec.BelowTarget)
		}
		rec.Message = fmt.Sprintf("Below the quality target, within your size budget: %s of %s. %s. It met the floor. Compare it with the original before approving the replacement.", formatBudgetBytes(candidateBytes), formatBudgetBytes(rec.BudgetBytes), capitalize(missed))
	}
	ec.State[sizeBudgetKey] = rec
	return rec, fits
}

// sizeBudgetEncodedPoint is the budget encode's point before its size and
// numbers are filled in: the encoder and bitrate its plan ran.
func sizeBudgetEncodedPoint(ec *ExecutionContext, rec *SizeBudgetRecord) SizeBudgetPoint {
	point := SizeBudgetPoint{Kind: "budget_fallback", VideoCodec: sizeBudgetChosenCodec(rec), BitrateKbps: rec.VideoBitrateKbps}
	if plan := getPlan(ec.State["plan"]); plan != nil {
		point.VideoCodec, point.Quality, point.Preset, point.BitrateKbps = transcode.NormalizeVideoCodec(plan.VideoCodec), plan.Quality, plan.Preset, sizeBudgetPlanKbps(plan)
	}
	return point
}

// overshotSizeBudget records a budget encode refused for its size before the
// budget itself was checked: the 15% minimum saving, which every budget sits
// under, so it is over the budget too.
func overshotSizeBudget(ec *ExecutionContext, candidateBytes int64) *SizeBudgetRecord {
	if candidateBytes > 0 {
		rec, _ := finishSizeBudget(ec, candidateBytes)
		return rec
	}
	rec := sizeBudgetOf(ec.State)
	if rec == nil {
		return nil
	}
	rec.Outcome = SizeBudgetExceeded
	rec.Message = fmt.Sprintf("The budget encode came out above 85%% of the source, over the %s budget. Original preserved.", formatBudgetBytes(rec.BudgetBytes))
	if sizeBudgetIsLadder(rec) {
		rec.RetryWithoutHardware = true
		rec.Message = fmt.Sprintf("All %d VideoToolbox steps came out over the %s budget. Original preserved. Try again with x265 (allow_hardware off): its two passes land on the budget.", rec.Rung+1, formatBudgetBytes(rec.BudgetBytes))
	}
	ec.State[sizeBudgetKey] = rec
	return rec
}

// failSizeBudget records a budget encode that missed the floor on final
// validation.
func failSizeBudget(ec *ExecutionContext, evidence *transcode.FinalQualityEvidence) *SizeBudgetRecord {
	rec := sizeBudgetOf(ec.State)
	if rec == nil {
		return nil
	}
	rec.Outcome = SizeBudgetBelowFloor
	rec.Final = summarizeFinalQuality(evidence, &rec.Floor)
	rec.BelowTarget = nil
	if rec.Final != nil {
		rec.BelowTarget = sizeBudgetMissedTarget(rec.Final.Measured, rec.Target)
		point := sizeBudgetEncodedPoint(ec, rec)
		point.Bytes, point.Measured, point.WithinBudget = evidence.CandidateSizeBytes, evidence.CandidateSizeBytes > 0, evidence.CandidateSizeBytes > 0 && evidence.CandidateSizeBytes <= rec.BudgetBytes
		point.VMAFMean, point.VMAFP5, point.VMAFWorst = rec.Final.Measured.VMAFMean, rec.Final.Measured.VMAFP5, rec.Final.Measured.VMAFWorstWindow
		if point.Bytes <= 0 {
			point.Bytes = rec.TargetBytes
		}
		curve := rec.Curve[:0:0]
		for _, p := range rec.Curve {
			if p.Kind != "budget_fallback" {
				curve = append(curve, p)
			}
		}
		rec.Curve = append(curve, point)
	}
	rec.Message = "The encode at the size budget measured below the quality floor"
	if sizeBudgetIsLadder(rec) {
		rec.Message = fmt.Sprintf("The VideoToolbox encode at %s, the first to fit the budget, measured below the quality floor", sizeBudgetRungDescription(getPlan(ec.State["plan"])))
	}
	if rec.Final != nil && len(rec.Final.Failures) > 0 {
		rec.Message += ": " + joinWords(rec.Final.Failures)
	}
	if sizeBudgetIsLadder(rec) {
		// x265 is slower but keeps more quality at the same size; the user
		// can run the goal again with the media engine off.
		rec.RetryWithoutHardware = true
		rec.Message += ". No candidate is offered; original preserved. Try again with x265 (allow_hardware off): slower, but it usually keeps more quality at the same size."
	} else {
		rec.Message += ". No candidate is offered; original preserved. A larger budget would give it more bits."
	}
	ec.State[sizeBudgetKey] = rec
	return rec
}

// sizeBudgetMissedTarget names each size-mode limit the measured numbers miss.
func sizeBudgetMissedTarget(m QualityNumbers, l QualityLimits) []string {
	var missed []string
	below := func(name string, v, limit *float64) {
		if v != nil && limit != nil && *v < *limit {
			missed = append(missed, fmt.Sprintf("%s %.1f is below the target %.1f", name, *v, *limit))
		}
	}
	above := func(name string, v, limit *float64) {
		if v != nil && limit != nil && *v > *limit {
			missed = append(missed, fmt.Sprintf("%s %.1f is above the target %.1f", name, *v, *limit))
		}
	}
	below("VMAF mean", m.VMAFMean, l.VMAFMean)
	below("VMAF p5", m.VMAFP5, l.VMAFP5)
	below(fmt.Sprintf("VMAF worst %.0f-second window", l.WorstWindowSeconds), m.VMAFWorstWindow, l.VMAFWorstWindow)
	above("CAMBI mean", m.CAMBIMean, l.CAMBIMean)
	above("CAMBI peak", m.CAMBIPeak, l.CAMBIPeak)
	return missed
}

func sizeBudgetPlannedMessage(r *SizeBudgetRecord, policy *SizeBudgetPolicy, plan *transcode.Plan) string {
	if sizeBudgetIsLadder(r) {
		return sizeBudgetLadderMessage(r, policy, plan)
	}
	from := ""
	if r.Siblings != nil {
		from = " " + r.Siblings.Message
	}
	if r.Trigger == sizeBudgetTriggerBudgetFirst {
		// Policy v4: nothing was searched; the budget's one encode is the
		// recipe.
		return fmt.Sprintf("Encoding the whole file once with two-pass x265 at %d kbps of video for about %s of the %s budget. Its quality is checked on the final validation windows against the floor; below this goal's target it is reported as such.%s%s", r.VideoBitrateKbps, formatBudgetBytes(r.TargetBytes), formatBudgetBytes(r.BudgetBytes), sizeBudgetAudioSentence(r.Audio), from)
	}
	why := "No tested setting met this goal's quality limits"
	if r.Trigger == sizeBudgetTriggerOverBudget {
		why = fmt.Sprintf("The setting that met this goal's quality limits (%s, about %s) is larger than your size budget", r.OverBudgetCandidateID, formatBudgetBytes(r.OverBudgetEstimatedBytes))
	}
	encoder := "two-pass x265"
	if sizeBudgetChosenCodec(r) == transcode.VideoCodecHEVCVideoToolbox {
		encoder = "VideoToolbox"
	}
	choice := ""
	if r.Choice != nil && r.Choice.Message != "" {
		choice = " " + r.Choice.Message
	}
	return fmt.Sprintf("%s, so the file is encoded once at the budget instead: %s at %d kbps of video for about %s of %s. Its quality is measured on the final validation windows and held only to the floor; it will be reported as below the quality target.%s%s", why, encoder, r.VideoBitrateKbps, formatBudgetBytes(r.TargetBytes), formatBudgetBytes(r.BudgetBytes), choice, from)
}

// formatBudgetBytesUp rounds up, so a floor shown to the user is itself enough.
func formatBudgetBytesUp(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.2f GB", math.Ceil(float64(n)/1e7)/100)
	}
	return fmt.Sprintf("%.0f MB", math.Ceil(float64(n)/1e6))
}

func formatBudgetBytes(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%.0f MB", float64(n)/1e6)
}

func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1 : len(parts)-1] {
		out += ", " + p
	}
	return out + " and " + parts[len(parts)-1]
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}

// batchPromotionBelowTarget counts the batch's candidates that a size budget
// encode produced below the quality target, so the approval says so wherever
// it is answered, not only in the web review.
func (e *Engine) batchPromotionBelowTarget(plan *batchPromotionPlan) int {
	n := 0
	for _, m := range plan.Members {
		child, err := e.deps.Store.GetActionInstance(m.SourceAction)
		if err != nil || child == nil {
			continue
		}
		if rec := sizeBudgetOf(parseExecutionContext(child, e).State); rec != nil && rec.Outcome == SizeBudgetBelowTarget {
			n++
		}
	}
	return n
}

// BatchItemFrozenSizeBudget is the size budget one batch item runs under, as
// frozen (nil without one): its own goal-change policy when it has one, else
// the batch's. state is the batch's decoded state. The encoder comparison and
// the VideoToolbox ladder and the audio levels are left out: they follow
// allow_hardware and the version the budget was frozen with, not a budget
// setting the user chose.
func BatchItemFrozenSizeBudget(state map[string]any, itemKey string) map[string]any {
	holder := state
	if row := objectMap(objectMap(state["batch_item_settings"])[itemKey]); objectMap(row["mode_policy"]) != nil {
		holder = row
	}
	frozen := objectMap(objectMap(objectMap(holder["mode_policy"])["size"])["size_budget"])
	if frozen == nil {
		return nil
	}
	budget := make(map[string]any, len(frozen))
	for k, v := range frozen {
		if k != "encoder_comparison" && k != "vt_quality_ladder" && k != "audio" {
			budget[k] = v
		}
	}
	return budget
}
