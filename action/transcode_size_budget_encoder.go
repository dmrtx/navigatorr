package action

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// At the budget the bitrate is fixed, so the encoders can be compared on
// quality alone. When the operation allows the media engine, the fallback
// first measures two encodes at the budget's video bitrate on the search's own
// sample windows: VideoToolbox in average-bitrate mode and the two-pass x265
// encode it would otherwise run. VideoToolbox encodes a film in minutes where
// two-pass x265 medium takes hours, so it is used when it measures within a
// small tolerance of x265 on both the VMAF mean and p5; otherwise x265 is. A
// round that cannot run or fails leaves the fallback x265 only, as it is
// without the comparison.
//
// VideoToolbox has no two-pass mode and can miss its average over a whole
// file. A VideoToolbox encode over the budget is therefore encoded once more
// at the bitrate scaled by how far it overshot.

const (
	// sizeBudgetRoundKey holds the comparison round while it runs: the record
	// the fallback planned and the search's results set aside.
	sizeBudgetRoundKey = "size_budget_round"
	// sizeBudgetCorrectedFromKey names the encode a bitrate correction
	// replaced. Only the correction writes it, so a manual retry of the
	// corrected encode, which moves previous_job_id on, keeps it.
	sizeBudgetCorrectedFromKey = "size_budget_corrected_from"

	sizeBudgetVTCandidateID   = "budget-vt"
	sizeBudgetX265CandidateID = "budget-x265"

	// Why the fallback uses the encoder it does.
	SizeBudgetChoiceVTWithinTolerance = "videotoolbox_within_tolerance"
	SizeBudgetChoiceX265Better        = "x265_better"
	SizeBudgetChoiceNotCompared       = "not_compared"
	SizeBudgetChoiceHardwareOff       = "hardware_not_allowed"
	SizeBudgetChoiceAfterContinuation = "videotoolbox_failed_full_file"
	SizeBudgetChoiceWorkerUnsupported = "worker_cannot_compare"
	SizeBudgetChoiceRoundFailed       = "comparison_failed"
	SizeBudgetChoiceIncomplete        = "comparison_incomplete"
)

// SizeBudgetComparison is the frozen rule of the encoder comparison at the
// budget.
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

// sizeBudgetComparisonRules: a just-noticeable difference is about 6 VMAF, and
// two encodes of the same windows measured apart differ by about half a point
// on the mean and a point on the p5. One point is a sixth of a JND, within that
// noise: VideoToolbox's speed is taken only when no one could see what it
// costs. The round encodes each of the search's windows twice with x265 (two
// passes) and once with VideoToolbox; 20 minutes covers 4K windows on a busy
// worker.
var sizeBudgetComparisonRules = SizeBudgetComparison{VMAFMeanTolerance: 1, VMAFP5Tolerance: 1, SearchSeconds: 1200, MaxBitrateCorrections: 1}

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

// sizeBudgetRound is the comparison round in flight.
type sizeBudgetRound struct {
	Record *SizeBudgetRecord `json:"record"`
	// Search holds the search's results the round's own benchmark replaces
	// in state; they are put back when it ends.
	Search map[string]any `json:"search_state"`
	// Failure is a submit the worker refused outright.
	Failure string `json:"failure,omitempty"`
}

// sizeBudgetSearchKeys are what a benchmark writes to state that belongs to
// the search, not to the comparison round run after it.
var sizeBudgetSearchKeys = []string{
	"benchmark_job_id", "benchmark_request", "benchmark_request_digest", "benchmark_decision", "benchmark_quality", "benchmark_status", "benchmark_search_seconds",
	"benchmark_comparison_available", "benchmark_worker_completed_at", "benchmark_reconciled_at", "benchmark_reconcile_lag_ms", "benchmark_attempt", "benchmark_retry_count",
	"automatic_search", "decision_reason", "capability_fingerprint", "search_budget_seconds", "search_budget_max_seconds", "search_budget_verify_seconds",
	"samples_planned", "candidates_count", "progress_details", "phase_costs", benchmarkRejectedKey, "failure_classification", "previous_benchmark_job_id",
}

func sizeBudgetRoundActive(state map[string]any) bool { return state[sizeBudgetRoundKey] != nil }

func sizeBudgetRoundID(instanceID string) string { return fmt.Sprintf("bench-%s-budget", instanceID) }

// failSizeBudgetRound records why the comparison round cannot finish. The
// wait step then plans x265, as for a round the worker reported failed. It
// reports false without a round.
func failSizeBudgetRound(ec *ExecutionContext, why string) bool {
	round := sizeBudgetRoundOf(ec.State)
	if round == nil {
		return false
	}
	round.Failure = why
	ec.State[sizeBudgetRoundKey] = *round
	return true
}

func sizeBudgetRoundOf(state map[string]any) *sizeBudgetRound {
	if state[sizeBudgetRoundKey] == nil {
		return nil
	}
	var r sizeBudgetRound
	if !reencode(state[sizeBudgetRoundKey], &r) || r.Record == nil {
		return nil
	}
	return &r
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
	p.AverageBitrateKbps, p.MaxBitrateKbps, p.ConstantBitrate = v.AverageBitrateKbps, v.MaxBitrateKbps, cloneBenchmarkBool(v.ConstantBitrate)
	p.QMin, p.QMax, p.GOPSize, p.BFrames = cloneBenchmarkInt(v.QMin), cloneBenchmarkInt(v.QMax), cloneBenchmarkInt(v.GOPSize), cloneBenchmarkInt(v.BFrames)
	p.ClosedGOP, p.PowerEfficient, p.MaxRefFrames = cloneBenchmarkBool(v.ClosedGOP), cloneBenchmarkBool(v.PowerEfficient), cloneBenchmarkInt(v.MaxRefFrames)
	p.PrioritizeSpeed, p.SpatialAQ, p.Realtime = cloneBenchmarkBool(v.PrioritizeSpeed), cloneBenchmarkBool(v.SpatialAQ), cloneBenchmarkBool(v.Realtime)
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

// sizeBudgetVideo is the budget encode's video for the record's choice.
// base is the plan the search started from.
func sizeBudgetVideo(snapshot *ModePolicySnapshot, rep *mediainspect.DetailedReport, base *transcode.Plan, enc sizeBudgetEncode, rec *SizeBudgetRecord) (sizeBudgetVideoSettings, error) {
	policy := snapshot.Size.SizeBudget
	if sizeBudgetChosenCodec(rec) != transcode.VideoCodecHEVCVideoToolbox {
		x := enc.X265
		return sizeBudgetVideoSettings{VideoCodec: transcode.VideoCodecLibX265, Preset: policy.Preset, X265: &x}, nil
	}
	if policy.Comparison == nil || snapshot.Size.Hardware == nil || base == nil {
		return sizeBudgetVideoSettings{}, fmt.Errorf("the frozen policy does not allow VideoToolbox at the budget")
	}
	kbps := enc.X265.BitrateKbps
	if rec.Correction != nil {
		kbps = rec.Correction.BitrateKbps
	}
	return sizeBudgetVTVideo(snapshot.Size.Hardware, max(sourceVideoBitDepth(rep), base.ExpectedBitDepth), kbps), nil
}

// startSizeBudgetComparison starts the comparison round when the frozen policy
// and the worker allow it. It records the x265 choice and why when they do
// not, and reports false: the caller then plans x265 directly.
func (e *Engine) startSizeBudgetComparison(ec *ExecutionContext, snapshot *ModePolicySnapshot, caps transcode.WorkerCapabilities, rep *mediainspect.DetailedReport, enc sizeBudgetEncode, rec *SizeBudgetRecord, outputs map[string]any) (StepResult, bool) {
	policy := snapshot.Size.SizeBudget
	x265Only := func(reason, message string) (StepResult, bool) {
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecLibX265, Reason: reason, Message: message}
		return StepResult{}, false
	}
	switch {
	case snapshot.Size.Hardware == nil:
		return x265Only(SizeBudgetChoiceHardwareOff, "This goal does not allow the media engine (allow_hardware=false), so the budget encode is two-pass x265.")
	case policy.Comparison == nil:
		return x265Only(SizeBudgetChoiceNotCompared, "This goal's budget was frozen before VideoToolbox could be compared at the budget, so the budget encode is two-pass x265.")
	case continuationActive(ec.State):
		// The sample windows already overrated VideoToolbox on this source.
		return x265Only(SizeBudgetChoiceAfterContinuation, "A VideoToolbox encode of this file already missed the quality limits on the full file, so the budget encode is two-pass x265.")
	}
	var search transcode.BenchmarkRequest
	if !reencode(ec.State["benchmark_request"], &search) || len(search.Samples) == 0 {
		return x265Only(SizeBudgetChoiceRoundFailed, "The search's sample windows are not recorded, so VideoToolbox could not be compared; the budget encode is two-pass x265.")
	}
	base := getPlan(ec.State["plan"])
	srcDepth := sourceVideoBitDepth(rep)
	qualityDepth := srcDepth
	if base != nil {
		qualityDepth = max(srcDepth, base.ExpectedBitDepth)
	}
	if !caps.Encoders[transcode.VideoCodecHEVCVideoToolbox] || validateWorkerCapabilitiesForBenchmark(caps, search.Metric, qualityDepth, transcode.VideoCodecHEVCVideoToolbox) != nil {
		return x265Only(SizeBudgetChoiceWorkerUnsupported, "The worker cannot measure VideoToolbox encodes, so the budget encode is two-pass x265.")
	}
	req, err := sizeBudgetRoundRequest(ec, snapshot, &search, base, srcDepth, enc)
	if err != nil {
		return x265Only(SizeBudgetChoiceRoundFailed, fmt.Sprintf("VideoToolbox could not be compared (%v); the budget encode is two-pass x265.", err))
	}
	digest, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		return x265Only(SizeBudgetChoiceRoundFailed, fmt.Sprintf("VideoToolbox could not be compared (%v); the budget encode is two-pass x265.", err))
	}
	round := sizeBudgetRound{Record: rec, Search: map[string]any{}}
	for _, key := range sizeBudgetSearchKeys {
		if v, ok := ec.State[key]; ok {
			var cp any
			if !reencode(v, &cp) {
				return x265Only(SizeBudgetChoiceRoundFailed, "The search's results could not be kept aside, so VideoToolbox was not compared; the budget encode is two-pass x265.")
			}
			round.Search[key] = cp
		}
	}
	rec.Choice = &SizeBudgetChoice{Reason: "measuring", BenchmarkJobID: req.ID, VMAFMeanTolerance: policy.Comparison.VMAFMeanTolerance, VMAFP5Tolerance: policy.Comparison.VMAFP5Tolerance,
		Message: fmt.Sprintf("Measuring VideoToolbox and two-pass x265 at %d kbps on the search's windows before encoding at the budget.", enc.X265.BitrateKbps)}
	ec.State[sizeBudgetRoundKey] = round
	for _, key := range []string{"benchmark_submitted", "benchmark_reconcile", "benchmark_done", "benchmark_retry_not_before", benchmarkRejectedKey, "benchmark_search_review_required", "benchmark_search_review"} {
		delete(ec.State, key)
	}
	ec.State["benchmark_job_id"], ec.State["benchmark_request"], ec.State["benchmark_request_digest"] = req.ID, req, digest
	ec.State["benchmark_attempt"], ec.State["benchmark_retry_count"] = 1, 0
	outputs[sizeBudgetRoundKey] = map[string]any{"benchmark_job_id": req.ID, "message": rec.Choice.Message}
	outputs["plan_note"] = rec.Choice.Message
	return StepResult{Status: StepCompleted, RewindTo: "submit_benchmark", Outputs: outputs}, true
}

// sizeBudgetRoundRequest is the comparison round: the two encodes at the
// budget's bitrate on the search's windows, measured against the floor, with
// nothing searched or pruned.
func sizeBudgetRoundRequest(ec *ExecutionContext, snapshot *ModePolicySnapshot, search *transcode.BenchmarkRequest, base *transcode.Plan, srcDepth int, enc sizeBudgetEncode) (*transcode.BenchmarkRequest, error) {
	if base == nil {
		return nil, fmt.Errorf("the search's starting plan is missing")
	}
	floor := sizeBudgetFloorQuality(snapshot)
	if floor == nil {
		return nil, fmt.Errorf("the quality floor is missing")
	}
	policy := snapshot.Size.SizeBudget
	xbase := *base
	xbase.VideoCodec, xbase.Preset, xbase.X265 = transcode.VideoCodecLibX265, policy.Preset, nil
	x265, err := buildBenchmarkCandidates(&xbase, srcDepth, &recipe.SearchPolicy{MaxCandidates: 1, QualityValues: []int{transcode.LibX265CRFMin}})
	if err != nil {
		return nil, err
	}
	x := enc.X265
	x265[0].ID, x265[0].Quality, x265[0].X265 = sizeBudgetX265CandidateID, 0, &x
	v := sizeBudgetVTVideo(snapshot.Size.Hardware, max(srcDepth, base.ExpectedBitDepth), enc.X265.BitrateKbps)
	vt := transcode.BenchmarkCandidate{ID: sizeBudgetVTCandidateID, VideoCodec: v.VideoCodec, AverageBitrateKbps: v.AverageBitrateKbps, GOPSize: v.GOPSize, BFrames: v.BFrames, VideoProfile: v.VideoProfile, PixelFormat: v.PixelFormat}
	quality := *floor
	req := &transcode.BenchmarkRequest{
		ProtocolVersion: search.ProtocolVersion, ID: sizeBudgetRoundID(ec.InstanceID), Mode: search.Mode, ModePolicyVersion: search.ModePolicyVersion,
		SearchBudgetSeconds: policy.Comparison.SearchSeconds, Allow8BitTo10Bit: search.Allow8BitTo10Bit, AudioMode: search.AudioMode,
		SourcePath: search.SourcePath, SourceDuration: search.SourceDuration, SourceSHA256: search.SourceSHA256, Metric: "vmaf",
		Samples: append([]transcode.BenchmarkSampleWindow(nil), search.Samples...), Candidates: []transcode.BenchmarkCandidate{vt, x265[0]},
		Quality: &quality, Concurrency: search.Concurrency, FallbackAudioBitrateBps: search.FallbackAudioBitrateBps, FallbackSubtitleSizeBytes: search.FallbackSubtitleSizeBytes,
		DeclaredVideoBitrateBps: search.DeclaredVideoBitrateBps, AttachmentBytes: search.AttachmentBytes,
	}
	if err := transcode.ValidateBenchmarkJobID(req.ID); err != nil {
		return nil, err
	}
	if err := transcode.ValidateBenchmarkRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// finishSizeBudgetComparison ends the comparison round: the search's results
// go back into state, the measured encodes join the curve and the encoder they
// pick is planned. st is nil when the worker refused the round's submit.
func (e *Engine) finishSizeBudgetComparison(ec *ExecutionContext, round *sizeBudgetRound, st *transcode.BenchmarkStatus) (StepResult, error) {
	rec := round.Record
	for _, key := range sizeBudgetSearchKeys {
		if v, ok := round.Search[key]; ok {
			ec.State[key] = v
		} else {
			delete(ec.State, key)
		}
		if _, ok := ec.Outputs[key]; ok {
			if v, kept := round.Search[key]; kept {
				ec.Outputs[key] = v
			} else {
				delete(ec.Outputs, key)
			}
		}
	}
	delete(ec.State, sizeBudgetRoundKey)
	ec.State["benchmark_done"], ec.State["benchmark_submitted"], ec.State["has_winner"] = true, true, false
	if st != nil && st.SearchSeconds != nil && *st.SearchSeconds > 0 {
		// The round's worker time is the action's search time too, so the
		// operation's finite search budget is charged for it.
		ec.State["benchmark_search_seconds"] = getFloat(ec.State, "benchmark_search_seconds") + *st.SearchSeconds
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil {
		return StepResult{Status: StepFailed, Error: "frozen size budget is missing after the encoder comparison (fail closed)"}, nil
	}
	policy := snapshot.Size.SizeBudget
	if st != nil {
		mirrorWorkerPhaseCosts(ec, "worker_size_budget_", st.PhaseCosts)
	}
	jobID := ""
	if rec.Choice != nil {
		jobID = rec.Choice.BenchmarkJobID
	}
	x265 := func(reason, message string) {
		rec.Choice = &SizeBudgetChoice{VideoCodec: transcode.VideoCodecLibX265, Reason: reason, Message: message, BenchmarkJobID: jobID}
	}
	switch {
	case st == nil:
		x265(SizeBudgetChoiceRoundFailed, fmt.Sprintf("The VideoToolbox comparison could not run (%s), so the budget encode is two-pass x265.", round.Failure))
	case st.Status != transcode.StatusCompleted || st.Decision == nil:
		why := st.Error
		if st.ReasonCode == "search_budget_exhausted" {
			why = "it ran out of time"
		}
		if why == "" {
			why = st.Status
		}
		x265(SizeBudgetChoiceRoundFailed, fmt.Sprintf("The VideoToolbox comparison did not finish (%s), so the budget encode is two-pass x265.", why))
	default:
		transcode.AttachVMAFEvidence(st.Decision, st.Quality)
		vt, x := sizeBudgetMeasuredPoints(st.Decision, rec.BudgetBytes, rec.VideoBitrateKbps)
		codec, reason := sizeBudgetPick(policy.Comparison, vt, x)
		rec.Choice = &SizeBudgetChoice{VideoCodec: codec, Reason: reason, BenchmarkJobID: jobID, Message: sizeBudgetChoiceMessage(reason, vt, x, policy.Comparison)}
		if policy.Comparison != nil {
			rec.Choice.VMAFMeanTolerance, rec.Choice.VMAFP5Tolerance = policy.Comparison.VMAFMeanTolerance, policy.Comparison.VMAFP5Tolerance
		}
		for _, p := range []*SizeBudgetPoint{vt, x} {
			if p != nil {
				p.Chosen = p.VideoCodec == codec
				rec.Curve = append(rec.Curve, *p)
			}
		}
	}
	original := getInt64(ec.State, "original_size")
	enc := planSizeBudgetEncode(policy, getSourceReport(ec.State["source_report"]), original, rec.BudgetBytes)
	if enc.Reason != "" {
		return StepResult{Status: StepFailed, Error: "size budget encode can no longer be planned after the encoder comparison (fail closed)"}, nil
	}
	outputs := map[string]any{"benchmark_done": true, "has_winner": false, "benchmark_decision": ec.State["benchmark_decision"], "benchmark_job_id": ec.State["benchmark_job_id"], "decision_reason": ec.State["decision_reason"]}
	result, _ := e.commitSizeBudgetPlan(ec, snapshot, enc, rec, outputs)
	return result, nil
}

// sizeBudgetMeasuredPoints reads the round's two encodes from its decision.
// A point without VMAF is nil.
// Both were encoded at kbps; x265's evaluation does not report it.
func sizeBudgetMeasuredPoints(d *transcode.BenchmarkDecision, budget int64, kbps int) (vt, x *SizeBudgetPoint) {
	for _, ev := range d.Evaluations {
		if ev.VMAF == nil || (ev.CandidateID != sizeBudgetVTCandidateID && ev.CandidateID != sizeBudgetX265CandidateID) {
			continue
		}
		mean, p5 := ev.VMAF.Mean, ev.VMAF.P5
		p := &SizeBudgetPoint{Kind: "budget_measured", CandidateID: ev.CandidateID, VideoCodec: transcode.BenchmarkCandidateVideoCodec(transcode.BenchmarkCandidate{VideoCodec: ev.VideoCodec}), Preset: ev.Preset,
			BitrateKbps: kbps, Bytes: ev.EstimatedBytes, WithinBudget: ev.EstimatedBytes > 0 && ev.EstimatedBytes <= budget, VMAFMean: &mean, VMAFP5: &p5}
		if ev.VMAF.WorstWindowMean > 0 {
			worst := ev.VMAF.WorstWindowMean
			p.VMAFWorst = &worst
		}
		if ev.CandidateID == sizeBudgetVTCandidateID {
			p.VideoCodec = transcode.VideoCodecHEVCVideoToolbox
			vt = p
		} else {
			p.VideoCodec = transcode.VideoCodecLibX265
			x = p
		}
	}
	return vt, x
}

// sizeBudgetMeasured finds the round's points in a record's curve, so
// validation can recompute the choice.
func sizeBudgetMeasured(rec *SizeBudgetRecord) (vt, x *SizeBudgetPoint) {
	for i := range rec.Curve {
		p := &rec.Curve[i]
		if p.Kind != "budget_measured" {
			continue
		}
		switch p.CandidateID {
		case sizeBudgetVTCandidateID:
			vt = p
		case sizeBudgetX265CandidateID:
			x = p
		}
	}
	return vt, x
}

// sizeBudgetPick is VideoToolbox when both encodes were measured and its VMAF
// mean and p5 are each within the frozen tolerance of x265's; otherwise x265.
func sizeBudgetPick(rules *SizeBudgetComparison, vt, x *SizeBudgetPoint) (codec, reason string) {
	measured := func(p *SizeBudgetPoint) bool { return p != nil && p.VMAFMean != nil && p.VMAFP5 != nil }
	switch {
	case rules == nil:
		return transcode.VideoCodecLibX265, SizeBudgetChoiceNotCompared
	case !measured(vt) || !measured(x):
		return transcode.VideoCodecLibX265, SizeBudgetChoiceIncomplete
	case *vt.VMAFMean >= *x.VMAFMean-rules.VMAFMeanTolerance && *vt.VMAFP5 >= *x.VMAFP5-rules.VMAFP5Tolerance:
		return transcode.VideoCodecHEVCVideoToolbox, SizeBudgetChoiceVTWithinTolerance
	}
	return transcode.VideoCodecLibX265, SizeBudgetChoiceX265Better
}

func sizeBudgetChoiceMessage(reason string, vt, x *SizeBudgetPoint, rules *SizeBudgetComparison) string {
	numbers := func(name string, p *SizeBudgetPoint) string {
		return fmt.Sprintf("%s measured VMAF %.1f (p5 %.1f)", name, *p.VMAFMean, *p.VMAFP5)
	}
	switch reason {
	case SizeBudgetChoiceVTWithinTolerance:
		return fmt.Sprintf("At the budget's bitrate %s and two-pass x265 %.1f (p5 %.1f): within %.1f VMAF, so the budget encode uses the much faster VideoToolbox.", numbers("VideoToolbox", vt), *x.VMAFMean, *x.VMAFP5, max(rules.VMAFMeanTolerance, rules.VMAFP5Tolerance))
	case SizeBudgetChoiceX265Better:
		return fmt.Sprintf("At the budget's bitrate %s and VideoToolbox %.1f (p5 %.1f): more than %.1f VMAF apart, so the budget encode is two-pass x265.", numbers("two-pass x265", x), *vt.VMAFMean, *vt.VMAFP5, max(rules.VMAFMeanTolerance, rules.VMAFP5Tolerance))
	}
	return "The comparison did not measure both encoders, so the budget encode is two-pass x265."
}

// sizeBudgetCorrectedKbps scales a VideoToolbox bitrate by how far its encode
// overshot: the video it should have taken over the video it took, with the
// other streams' share as planned. Zero when no lower bitrate would help.
func sizeBudgetCorrectedKbps(kbps int, candidateBytes int64, enc sizeBudgetEncode, budget int64) int {
	if kbps <= 0 || candidateBytes <= budget || candidateBytes <= enc.NonVideoBytes || enc.TargetBytes <= enc.NonVideoBytes {
		return 0
	}
	corrected := int(math.Floor(float64(kbps) * float64(enc.TargetBytes-enc.NonVideoBytes) / float64(candidateBytes-enc.NonVideoBytes)))
	if corrected < 1 || corrected >= kbps {
		return 0
	}
	return corrected
}

// retrySizeBudgetBitrate encodes a VideoToolbox budget candidate that came
// out over the budget once more at a corrected bitrate. It reports false when
// that does not apply: x265, a second overshoot, or no lower bitrate to use.
// evidence is the overshooting encode's final validation, when it ran.
func (e *Engine) retrySizeBudgetBitrate(ec *ExecutionContext, plan *transcode.Plan, candidateBytes int64, evidence *transcode.FinalQualityEvidence) (StepResult, bool) {
	rec := sizeBudgetOf(ec.State)
	if rec == nil || plan == nil || transcode.NormalizeVideoCodec(plan.VideoCodec) != transcode.VideoCodecHEVCVideoToolbox || rec.Correction != nil || candidateBytes <= rec.BudgetBytes {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil || snapshot.Size.SizeBudget.Comparison == nil || snapshot.Size.SizeBudget.Comparison.MaxBitrateCorrections < 1 {
		return StepResult{}, false
	}
	enc := planSizeBudgetEncode(snapshot.Size.SizeBudget, getSourceReport(ec.State["source_report"]), getInt64(ec.State, "original_size"), rec.BudgetBytes)
	corrected := sizeBudgetCorrectedKbps(plan.AverageBitrateKbps, candidateBytes, enc, rec.BudgetBytes)
	if enc.Reason != "" || corrected == 0 {
		return StepResult{}, false
	}
	first := getString(ec.State, "job_id")
	rec.Correction = &SizeBudgetCorrection{FirstJobID: first, FirstCandidateBytes: candidateBytes, FirstBitrateKbps: plan.AverageBitrateKbps, BitrateKbps: corrected}
	over := SizeBudgetPoint{Kind: "budget_overshoot", VideoCodec: transcode.VideoCodecHEVCVideoToolbox, BitrateKbps: plan.AverageBitrateKbps, Bytes: candidateBytes, Measured: true}
	if evidence != nil && evidence.VMAF != nil {
		n := qualityNumbersOf(evidence.VMAF, evidence.CAMBI, nil)
		over.VMAFMean, over.VMAFP5, over.VMAFWorst = n.VMAFMean, n.VMAFP5, n.VMAFWorstWindow
	}
	curve := rec.Curve[:0:0]
	for _, p := range rec.Curve {
		if p.Kind != "budget_fallback" {
			curve = append(curve, p)
		}
	}
	rec.Curve = append(curve, over, SizeBudgetPoint{Kind: "budget_fallback", VideoCodec: transcode.VideoCodecHEVCVideoToolbox, BitrateKbps: corrected, Bytes: rec.TargetBytes, WithinBudget: true})
	p := *plan
	p.AverageBitrateKbps = corrected
	p.PlanDigest = ""
	digest, err := transcode.DigestPlan(&p)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building the corrected size budget plan: %v", err)}, true
	}
	p.PlanDigest = digest
	rec.Message = fmt.Sprintf("The VideoToolbox encode at %d kbps came out at %s, over the %s budget (it has no second pass to correct itself). Encoding it once more at %d kbps.", plan.AverageBitrateKbps, formatBudgetBytes(candidateBytes), formatBudgetBytes(rec.BudgetBytes), corrected)
	appendFailureHistory(ec, "size_budget", "policy_rejected", rec.Message)
	for _, key := range transcodeAttemptKeys {
		delete(ec.State, key)
		delete(ec.Outputs, key)
	}
	delete(ec.State, "skip_transcode")
	delete(ec.State, "reason_code")
	jobID := fmt.Sprintf("job-%s-budget-rate", ec.InstanceID)
	ec.State[sizeBudgetKey] = rec
	ec.State["plan"], ec.State["plan_digest"] = &p, digest
	ec.State["previous_job_id"], ec.State["job_id"], ec.State["external_reference"] = first, jobID, jobID
	ec.State[sizeBudgetCorrectedFromKey] = first
	ec.State["attempt"], ec.State["retry_count"], ec.State["fallback_count"] = 1, 0, 0
	mergeMap(ec.Outputs, map[string]any{"plan": &p, "plan_digest": digest, "job_id": jobID, "external_reference": jobID, "previous_job_id": first})
	return StepResult{Status: StepCompleted, RewindTo: "submit_transcode", Outputs: map[string]any{sizeBudgetKey: rec, "plan_note": rec.Message}}, true
}

// sizeBudgetSearchDepth is the output bit depth the search's candidates were
// encoded at. The request must be the one the plan's final validation names.
func sizeBudgetSearchDepth(state map[string]any, plan *transcode.Plan) (int, bool) {
	var req transcode.BenchmarkRequest
	if plan.QualityValidation == nil || !reencode(state["benchmark_request"], &req) || len(req.Candidates) == 0 {
		return 0, false
	}
	if digest, err := transcode.DigestBenchmarkRequest(&req); err != nil || digest != plan.QualityValidation.BenchmarkRequestDigest {
		return 0, false
	}
	depth := 0
	for _, c := range req.Candidates {
		d := 8
		if c.PixelFormat == "p010le" || c.PixelFormat == "yuv420p10le" {
			d = 10
		}
		if depth != 0 && d != depth {
			return 0, false
		}
		depth = d
	}
	return depth, true
}

// sizeBudgetPlanMatches rebuilds a budget plan's video from the frozen policy,
// the source and the record's choice, and reports whether the plan carries
// exactly that. A VideoToolbox plan also needs a policy that allows it, a
// comparison whose recorded numbers pick it, and, after a correction, the
// bitrate that correction gives.
func sizeBudgetPlanMatches(snapshot *ModePolicySnapshot, instanceID string, state map[string]any, plan *transcode.Plan, rec *SizeBudgetRecord) bool {
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
	var want sizeBudgetVideoSettings
	if sizeBudgetChosenCodec(rec) == transcode.VideoCodecHEVCVideoToolbox {
		if policy.Comparison == nil || snapshot.Size.Hardware == nil {
			return false
		}
		// The choice must come from this action's own round, both encodes
		// measured at the budget's bitrate, and its numbers must pick
		// VideoToolbox under the frozen tolerance.
		vt, x := sizeBudgetMeasured(rec)
		if rec.Choice.BenchmarkJobID != sizeBudgetRoundID(instanceID) || vt == nil || x == nil || vt.BitrateKbps != enc.X265.BitrateKbps || x.BitrateKbps != enc.X265.BitrateKbps {
			return false
		}
		if codec, _ := sizeBudgetPick(policy.Comparison, vt, x); codec != transcode.VideoCodecHEVCVideoToolbox {
			return false
		}
		kbps := enc.X265.BitrateKbps
		if c := rec.Correction; c != nil {
			kbps = sizeBudgetCorrectedKbps(kbps, c.FirstCandidateBytes, enc, budget)
			if policy.Comparison.MaxBitrateCorrections < 1 || c.FirstBitrateKbps != enc.X265.BitrateKbps || kbps == 0 || c.BitrateKbps != kbps || c.FirstJobID == "" || c.FirstJobID != getString(state, sizeBudgetCorrectedFromKey) {
				return false
			}
		}
		// The output depth is the one the search encoded at, read from the
		// search request the plan's final validation is pinned to.
		depth, ok := sizeBudgetSearchDepth(state, plan)
		if !ok || plan.ExpectedBitDepth != depth || depth < sourceVideoBitDepth(rep) {
			return false
		}
		want = sizeBudgetVTVideo(snapshot.Size.Hardware, depth, kbps)
	} else {
		if rec.Correction != nil {
			return false
		}
		x := enc.X265
		want = sizeBudgetVideoSettings{VideoCodec: transcode.VideoCodecLibX265, Preset: policy.Preset, X265: &x}
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(sizeBudgetVideoOf(plan))
	return string(a) == string(b)
}
