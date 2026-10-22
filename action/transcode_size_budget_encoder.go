package action

import (
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// A size budget with the media engine encodes the whole file with VideoToolbox
// (see transcode_size_budget_ladder.go); without it, once with two-pass x265
// at the budget's bitrate. Records and policies that carry the removed encoder
// comparison, or the bitrate correction it came with, still decode.

const (
	// Why the budget encodes with the encoder it does.
	SizeBudgetChoiceHardwareOff       = "hardware_not_allowed"
	SizeBudgetChoiceWorkerUnsupported = "worker_cannot_compare"
)

// SizeBudgetComparison is the frozen rule of the encoder comparison earlier
// size policies carried. Nothing freezes it now; it stays so those snapshots
// decode and validate.
type SizeBudgetComparison struct {
	// VideoToolbox is used when its sampled VMAF mean and p5 are each no more
	// than this below x265's at the same bitrate.
	VMAFMeanTolerance float64 `json:"vmaf_mean_tolerance"`
	VMAFP5Tolerance   float64 `json:"vmaf_p5_tolerance"`
	// SearchSeconds bounds the comparison round on the worker.
	SearchSeconds int `json:"search_seconds"`
	// MaxBitrateCorrections is how many times a VideoToolbox encode over the
	// budget is encoded again at a lower bitrate.
	MaxBitrateCorrections int `json:"max_bitrate_corrections"`
}

func (c *SizeBudgetComparison) validate() error {
	if c == nil {
		return nil
	}
	for _, v := range []float64{c.VMAFMeanTolerance, c.VMAFP5Tolerance} {
		if math.IsNaN(v) || v < 0 || v > 6 {
			return fmt.Errorf("invalid size budget encoder comparison tolerance")
		}
	}
	if c.SearchSeconds < 60 || c.SearchSeconds > transcode.MaxSearchBudgetSeconds || c.MaxBitrateCorrections < 0 || c.MaxBitrateCorrections > 1 {
		return fmt.Errorf("invalid size budget encoder comparison bounds")
	}
	return nil
}

// SizeBudgetChoice is the encoder a budget fallback uses and why.
type SizeBudgetChoice struct {
	VideoCodec string `json:"video_codec"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	// BenchmarkJobID is the comparison round's worker identity.
	BenchmarkJobID    string  `json:"benchmark_job_id,omitempty"`
	VMAFMeanTolerance float64 `json:"vmaf_mean_tolerance,omitempty"`
	VMAFP5Tolerance   float64 `json:"vmaf_p5_tolerance,omitempty"`
}

// SizeBudgetCorrection is a VideoToolbox encode that came out over the budget
// and the bitrate it was encoded again at.
type SizeBudgetCorrection struct {
	FirstJobID          string `json:"first_job_id"`
	FirstCandidateBytes int64  `json:"first_candidate_bytes"`
	FirstBitrateKbps    int    `json:"first_bitrate_kbps"`
	BitrateKbps         int    `json:"bitrate_kbps"`
}

// sizeBudgetVideoSettings are the video fields a budget plan sets over the
// search's starting plan. Validation rebuilds them and compares exactly.
type sizeBudgetVideoSettings struct {
	VideoCodec         string                  `json:"video_codec"`
	Quality            int                     `json:"quality"`
	Preset             string                  `json:"preset,omitempty"`
	X265               *transcode.X265Settings `json:"x265,omitempty"`
	AverageBitrateKbps int                     `json:"average_bitrate_kbps,omitempty"`
	MaxBitrateKbps     int                     `json:"max_bitrate_kbps,omitempty"`
	ConstantBitrate    *bool                   `json:"constant_bitrate,omitempty"`
	QMin               *int                    `json:"qmin,omitempty"`
	QMax               *int                    `json:"qmax,omitempty"`
	GOPSize            *int                    `json:"gop_size,omitempty"`
	BFrames            *int                    `json:"b_frames,omitempty"`
	ClosedGOP          *bool                   `json:"closed_gop,omitempty"`
	PowerEfficient     *bool                   `json:"power_efficient,omitempty"`
	MaxRefFrames       *int                    `json:"max_ref_frames,omitempty"`
	PrioritizeSpeed    *bool                   `json:"prioritize_speed,omitempty"`
	SpatialAQ          *bool                   `json:"spatial_aq,omitempty"`
	Realtime           *bool                   `json:"realtime,omitempty"`
	// VideoToolbox only: x265 keeps the starting plan's tune (animation for
	// anime), profile and pixel format.
	Tune         string `json:"tune,omitempty"`
	VideoProfile string `json:"video_profile,omitempty"`
	PixelFormat  string `json:"pixel_format,omitempty"`
}

func (v sizeBudgetVideoSettings) hardware() bool {
	return v.VideoCodec == transcode.VideoCodecHEVCVideoToolbox
}

func (v sizeBudgetVideoSettings) apply(p *transcode.Plan) {
	p.VideoCodec, p.Quality, p.Preset, p.X265 = v.VideoCodec, v.Quality, v.Preset, v.X265.Clone()
	p.AverageBitrateKbps, p.MaxBitrateKbps, p.ConstantBitrate = v.AverageBitrateKbps, v.MaxBitrateKbps, cloneBool(v.ConstantBitrate)
	p.QMin, p.QMax, p.GOPSize, p.BFrames = cloneInt(v.QMin), cloneInt(v.QMax), cloneInt(v.GOPSize), cloneInt(v.BFrames)
	p.ClosedGOP, p.PowerEfficient, p.MaxRefFrames = cloneBool(v.ClosedGOP), cloneBool(v.PowerEfficient), cloneInt(v.MaxRefFrames)
	p.PrioritizeSpeed, p.SpatialAQ, p.Realtime = cloneBool(v.PrioritizeSpeed), cloneBool(v.SpatialAQ), cloneBool(v.Realtime)
	if v.hardware() {
		p.Tune, p.VideoProfile, p.PixelFormat = v.Tune, v.VideoProfile, v.PixelFormat
	}
}

// sizeBudgetVideoOf reads a plan's budget video fields back.
func sizeBudgetVideoOf(p *transcode.Plan) sizeBudgetVideoSettings {
	v := sizeBudgetVideoSettings{VideoCodec: transcode.NormalizeVideoCodec(p.VideoCodec), Quality: p.Quality, Preset: p.Preset, X265: p.X265.Clone(),
		AverageBitrateKbps: p.AverageBitrateKbps, MaxBitrateKbps: p.MaxBitrateKbps, ConstantBitrate: p.ConstantBitrate, QMin: p.QMin, QMax: p.QMax, GOPSize: p.GOPSize, BFrames: p.BFrames,
		ClosedGOP: p.ClosedGOP, PowerEfficient: p.PowerEfficient, MaxRefFrames: p.MaxRefFrames, PrioritizeSpeed: p.PrioritizeSpeed, SpatialAQ: p.SpatialAQ, Realtime: p.Realtime}
	if v.hardware() {
		v.Tune, v.VideoProfile, v.PixelFormat = p.Tune, p.VideoProfile, p.PixelFormat
	}
	return v
}

// sizeBudgetVTVideo is VideoToolbox at an average bitrate with the frozen
// hardware search's keyframe interval and B-frames, in Main10 for a 10-bit
// output.
func sizeBudgetVTVideo(hw *transcode.HardwareSearchRules, bitDepth, kbps int) sizeBudgetVideoSettings {
	gop, bframes := hw.GOPSize, hw.BFrames
	v := sizeBudgetVideoSettings{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, AverageBitrateKbps: kbps, GOPSize: &gop, BFrames: &bframes, VideoProfile: "main", PixelFormat: "yuv420p"}
	if bitDepth == 10 {
		v.VideoProfile, v.PixelFormat = "main10", "p010le"
	}
	return v
}

func sizeBudgetPlanKbps(p *transcode.Plan) int {
	if p.X265 != nil {
		return p.X265.BitrateKbps
	}
	return p.AverageBitrateKbps
}

func sourceVideoBitDepth(rep *mediainspect.DetailedReport) int {
	if rep == nil {
		return 0
	}
	if video := rep.MainVideo(); len(video) == 1 {
		return video[0].BitDepth
	}
	return 0
}

// sizeBudgetChosenCodec is the encoder a record's choice names, x265 without one.
func sizeBudgetChosenCodec(rec *SizeBudgetRecord) string {
	if rec != nil && rec.Choice != nil && rec.Choice.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
		return transcode.VideoCodecHEVCVideoToolbox
	}
	return transcode.VideoCodecLibX265
}

// sizeBudgetVideo is the budget encode's video: the ladder's rung, or two-pass
// x265 at the budget's bitrate. base is the plan the goal started from.
func sizeBudgetVideo(snapshot *ModePolicySnapshot, rep *mediainspect.DetailedReport, base *transcode.Plan, enc sizeBudgetEncode, rec *SizeBudgetRecord) (sizeBudgetVideoSettings, error) {
	policy := snapshot.Size.SizeBudget
	if sizeBudgetIsLadder(rec) {
		if base == nil {
			return sizeBudgetVideoSettings{}, fmt.Errorf("the frozen size plan is missing")
		}
		return sizeBudgetLadderVideo(policy, snapshot.Size.Hardware, max(sourceVideoBitDepth(rep), base.ExpectedBitDepth), enc, rec.Rung)
	}
	x := enc.X265
	return sizeBudgetVideoSettings{VideoCodec: transcode.VideoCodecLibX265, Preset: policy.Preset, X265: &x}, nil
}

// sizeBudgetPlanMatches rebuilds a budget plan's video from the frozen policy,
// the source and the record, and reports whether the plan carries exactly that.
func sizeBudgetPlanMatches(snapshot *ModePolicySnapshot, state map[string]any, plan *transcode.Plan, rec *SizeBudgetRecord) bool {
	policy := snapshot.Size.SizeBudget
	if policy == nil || rec == nil || plan == nil {
		return false
	}
	original := getInt64(state, "original_size")
	budget := policy.BudgetBytes(original, siblingEvidenceOf(state))
	rep := getSourceReport(state["source_report"])
	enc := planSizeBudgetEncode(policy, rep, original, budget)
	if enc.Reason != "" || rec.BudgetBytes != budget || rec.TargetBytes != enc.TargetBytes || rec.VideoBitrateKbps != enc.X265.BitrateKbps {
		return false
	}
	// The audio level is chosen again from the source and must be the plan's and
	// the record's, so a plan cannot carry a level the policy would not choose.
	if plan.AudioMode != enc.AudioMode || sizeBudgetAudioRecordJSON(rec.Audio) != sizeBudgetAudioRecordJSON(enc.Audio) {
		return false
	}
	if sizeBudgetIsLadder(rec) {
		return sizeBudgetLadderPlanMatches(snapshot, rep, plan, rec, enc, budget)
	}
	if rec.Correction != nil || sizeBudgetChosenCodec(rec) == transcode.VideoCodecHEVCVideoToolbox {
		return false
	}
	x := enc.X265
	want := sizeBudgetVideoSettings{VideoCodec: transcode.VideoCodecLibX265, Preset: policy.Preset, X265: &x}
	return sizeBudgetVideoJSON(want) == sizeBudgetVideoJSON(sizeBudgetVideoOf(plan))
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
