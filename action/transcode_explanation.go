package action

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// The same projection is used by the web API and MCP. A completed workflow is
// not necessarily an accepted conversion; sampling and final results differ.
type TranscodeExplanation struct {
	Goal             string                              `json:"goal"`
	Outcome          string                              `json:"outcome"`
	Message          string                              `json:"message"`
	TestedCandidates int                                 `json:"tested_candidates"`
	VerifiedSamples  int                                 `json:"verified_samples"`
	Stages           []transcode.BenchmarkAutomaticStage `json:"stages,omitempty"`
	// Encoder is the winning candidate's encoder (libx265 or
	// hevc_videotoolbox) once a search selected one. Hardware is true for the
	// VideoToolbox media engine, so a hardware result is told apart from x265
	// before anything is replaced.
	Encoder  string `json:"encoder,omitempty"`
	Hardware bool   `json:"hardware,omitempty"`
	// Verification lists each candidate the search checked on the verification
	// windows and says whether the time limit ended it. Windows puts the
	// winner's sample score and its final score side by side per window.
	Verification *transcode.BenchmarkVerificationEvidence `json:"verification,omitempty"`
	Windows      []transcode.WindowComparison             `json:"windows,omitempty"`
	// FinalQuality is the full-file quality check of a result that failed it:
	// which checks failed, by how much, and the numbers behind them.
	FinalQuality *FinalQualitySummary `json:"final_quality,omitempty"`
	// HardwareContinuation is set when a VideoToolbox result failed that check
	// and the action went on (or could not go on) with x265 alone. It keeps the
	// failed attempt's numbers after the x265 job replaces it.
	HardwareContinuation *HardwareContinuation `json:"hardware_continuation,omitempty"`
	// SizeBudget is set when the operation's size budget replaced the search's
	// outcome with one encode at the budget: what it planned, the measured
	// rate-quality points, and how the result compares with the target.
	SizeBudget *SizeBudgetRecord `json:"size_budget,omitempty"`
	// SizeBudgetSiblings is what a sibling budget measured beside the source,
	// including why it gave no budget.
	SizeBudgetSiblings *SiblingBudgetEvidence `json:"size_budget_siblings,omitempty"`
	// QualityLadder is set when the goal's recipe encoded the whole file at a
	// ladder of fixed settings instead of searching: the rungs, the one
	// encoding or kept, and what each earlier rung measured.
	QualityLadder *QualityLadderRecord `json:"quality_ladder,omitempty"`
}

func ExplainTranscode(res *ActionResult) *TranscodeExplanation {
	// Batches have their own library outcome (preview, candidates, promotion,
	// partial results). Per-file search evidence cannot describe that outcome.
	if res == nil || res.ActionName != "transcode_media" || getString(res.Inputs, "mode") == "" {
		return nil
	}
	e := &TranscodeExplanation{Goal: getString(res.Inputs, "mode"), Outcome: "processing", Message: "Automatic testing and verification; originals are preserved."}
	read := func(key string) any {
		if value := res.Outputs[key]; value != nil {
			return value
		}
		return res.State[key]
	}
	recipe := policyRecipe(res.State["mode_policy"]) != nil
	if recipe {
		e.Message = "Encoding the whole file and checking it against this goal's quality limits; originals are preserved."
	}
	var automatic transcode.BenchmarkAutomaticEvidence
	data, _ := json.Marshal(read("automatic_search"))
	_ = json.Unmarshal(data, &automatic)
	e.Stages = automatic.Stages
	e.VerifiedSamples = automatic.VerifiedSamples
	e.Verification = automatic.Verification
	ids := map[string]bool{}
	for _, stage := range e.Stages {
		for _, id := range stage.Candidates {
			ids[id] = true
		}
	}
	e.TestedCandidates = len(ids)
	var decision transcode.BenchmarkDecision
	data, _ = json.Marshal(read("benchmark_decision"))
	var budget SizeBudgetRecord
	if data, _ := json.Marshal(read(sizeBudgetKey)); json.Unmarshal(data, &budget) == nil && budget.BudgetBytes > 0 {
		e.SizeBudget = &budget
		e.Encoder = sizeBudgetChosenCodec(&budget)
		e.Hardware = e.Encoder == transcode.VideoCodecHEVCVideoToolbox
	}
	e.SizeBudgetSiblings = siblingEvidenceOf(res.State)
	if ladder := QualityLadderOf(map[string]any{qualityLadderKey: read(qualityLadderKey)}); ladder != nil {
		e.QualityLadder = ladder
		e.Encoder = ladder.VideoCodec
		e.Hardware = e.Encoder == transcode.VideoCodecHEVCVideoToolbox
		if ladder.Message != "" {
			e.Message = ladder.Message
		}
	} else if e.SizeBudget != nil && recipe && e.SizeBudget.Message != "" {
		e.Message = e.SizeBudget.Message
	}
	if json.Unmarshal(data, &decision) == nil && decision.Winner != nil && e.SizeBudget == nil {
		e.Encoder = transcode.BenchmarkCandidateVideoCodec(transcode.BenchmarkCandidate{VideoCodec: decision.Winner.VideoCodec})
		e.Hardware = e.Encoder == transcode.VideoCodecHEVCVideoToolbox
	}
	var evidence transcode.FinalQualityEvidence
	data, _ = json.Marshal(read("quality_evidence"))
	if json.Unmarshal(data, &evidence) == nil {
		e.Windows = transcode.CompareWindows(automatic.WinnerWindows, evidence.Windows)
	}
	reason := getString(res.State, "reason_code")
	if reason == "" {
		reason = getString(res.Outputs, "reason_code")
	}
	var continuation HardwareContinuation
	if data, _ := json.Marshal(read(hardwareContinuationKey)); json.Unmarshal(data, &continuation) == nil && continuation.Outcome != "" {
		e.HardwareContinuation = &continuation
	}
	finish := func() *TranscodeExplanation {
		if e.HardwareContinuation != nil {
			e.Message = strings.TrimSpace(continuationNote(e.HardwareContinuation) + " " + e.Message)
		}
		if b := e.SizeBudgetSiblings; b != nil && b.Reason != "" && res.Status == StatusCompleted {
			e.Message = strings.TrimSpace(e.Message + " " + b.Message)
		}
		// A frozen budget the search's outcome stood without says why.
		if note := getString(res.State, sizeBudgetSkipMessageKey); note != "" && e.SizeBudget == nil && res.Status == StatusCompleted {
			e.Message = strings.TrimSpace(e.Message + " " + note)
		}
		return e
	}
	switch {
	case res.Status == StatusCancelled:
		e.Outcome = "cancelled"
		e.Message = "Stopped; originals preserved."
	case res.Status == StatusFailed:
		e.Outcome = "execution_error"
		e.Message = "Processing could not finish reliably. Inspect the execution error and recorded integrity checks."
	case res.Status == StatusWaitingDecision:
		e.Outcome = "attention"
		e.Message = res.WaitingReason
	case res.Status != StatusCompleted:
		return finish()
	case reason == "no_suitable_candidate":
		e.Outcome = "no_suitable_candidate"
		e.Message = "None of the tested configurations met this goal. Originals preserved."
		if e.Verification != nil && e.Verification.StoppedBy == transcode.VerificationStoppedTimeCap {
			e.Message = fmt.Sprintf("The search ran out of time before every promising configuration was checked (%d never verified). Nothing was proven unsuitable; retry to continue. Originals preserved.", len(e.Verification.Unverified))
		}
	case (reason == SizeBudgetExceeded || reason == "insufficient_savings") && audioOnlyBudget(res, read) > 0:
		e.Outcome = "size_rejected"
		e.Message = audioOnlyOvershootMessage(audioOnlyBudget(res, read), getInt64(map[string]any{"size": read("candidate_size_bytes")}, "size"))
	case (reason == "insufficient_savings" || reason == "size_growth" || reason == "size_growth_exceeded" || reason == reasonQualityNotMet) && e.QualityLadder != nil && getString(res.State, "plan_source") == qualityLadderPlanSource && e.QualityLadder.Outcome != QualityLadderEncoding && e.QualityLadder.Outcome != QualityLadderPassed:
		e.Outcome = "size_rejected"
		if reason == reasonQualityNotMet {
			e.Outcome = "quality_rejected"
			if n := len(e.QualityLadder.Tried); n > 0 {
				e.FinalQuality = e.QualityLadder.Tried[n-1].Final
			}
		}
		e.Message = e.QualityLadder.Message
	case reason == "insufficient_savings" || reason == "size_growth" || reason == "size_growth_exceeded":
		e.Outcome = "size_rejected"
		e.Message = "The full conversion did not meet this goal's size limit. Original preserved."
	case reason == "quality_not_met" && e.SizeBudget != nil:
		e.Outcome = "quality_rejected"
		e.FinalQuality = e.SizeBudget.Final
		e.Message = e.SizeBudget.Message
	case reason == SizeBudgetExceeded && e.SizeBudget != nil:
		e.Outcome = "size_rejected"
		e.Message = e.SizeBudget.Message
	case reason == "quality_not_met":
		e.Outcome = "quality_rejected"
		e.Message = "The encoded result did not meet the quality requirements. Original preserved."
		var evidence transcode.FinalQualityEvidence
		if data, _ := json.Marshal(read("quality_evidence")); json.Unmarshal(data, &evidence) == nil && evidence.Verdict != "" {
			var request struct {
				Quality *transcode.BenchmarkQualityConfig `json:"quality"`
			}
			if data, _ := json.Marshal(read("benchmark_request")); json.Unmarshal(data, &request) == nil {
				e.FinalQuality = summarizeFinalQuality(&evidence, qualityLimitsOf(request.Quality))
			} else {
				e.FinalQuality = summarizeFinalQuality(&evidence, nil)
			}
			if len(e.FinalQuality.Failures) > 0 {
				e.Message = "The encoded result did not meet the quality requirements on the whole file: " + strings.Join(e.FinalQuality.Failures, "; ") + ". Original preserved."
			}
		}
	case reason == "preservation_not_met":
		e.Outcome = "preservation_rejected"
		e.Message = "The encoded result did not preserve the required tracks or video properties. Original preserved."
	case reason == "audio_compaction_insufficient":
		e.Outcome = "size_rejected"
		e.Message = "No video encode met this goal, and compacting the lossless audio alone would not save enough. Original preserved."
	case reason == "audio_estimate_unavailable":
		e.Outcome = "no_changes"
		e.Message = "No video encode met this goal, and the size of the lossless audio could not be read, so the savings from compacting it alone could not be estimated. Original preserved."
	case reason == "audio_only_worker_unsupported":
		e.Outcome = "no_changes"
		e.Message = "No video encode met this goal. Compacting the lossless audio needs a worker that can prove the copied video unchanged; upgrade the worker and retry. Original preserved."
	case reason == "already_target_codec":
		e.Outcome = "no_changes"
		e.Message = "The source already uses the target codec. Original preserved."
	case recipe && getString(res.State, sizeBudgetSkipMessageKey) != "" && e.SizeBudget == nil:
		// A budget the recipe could not encode; finish adds why.
		e.Outcome = "no_changes"
		e.Message = "No candidate was made."
	case getBool(res.State, "skip_transcode") || getBool(res.Outputs, "skip_transcode") || getBool(res.State, "skipped") || getBool(res.Outputs, "skipped"):
		e.Outcome = "no_changes"
		e.Message = "No candidate was accepted. Inspect the recorded skip reason."
	case getBool(res.Outputs, "original_intact") && getBool(res.State, "size_audio_only"):
		e.Outcome = "candidate_ready"
		e.Message = "Candidate verified; original preserved. The video is copied bit-identically and only the lossless audio was compacted."
	case getBool(res.Outputs, "original_intact") && e.SizeBudget != nil && e.SizeBudget.Outcome != "":
		e.Outcome = "candidate_ready"
		e.Message = e.SizeBudget.Message
	case getBool(res.Outputs, "original_intact") && e.QualityLadder != nil && e.QualityLadder.Outcome == QualityLadderPassed:
		e.Outcome = "candidate_ready"
		e.FinalQuality = e.QualityLadder.Final
		e.Message = "Candidate verified; original preserved. " + e.QualityLadder.Message
	case getBool(res.Outputs, "original_intact"):
		e.Outcome = "candidate_ready"
		e.Message = "Candidate verified; original preserved. Quality was measured on samples."
		if e.Hardware {
			e.Message += " Encoded with the VideoToolbox media engine (hardware), not x265."
		}
	default:
		e.Outcome = "completed"
		e.Message = "Workflow finished. Inspect the recorded result before replacing any file."
	}
	return finish()
}

// audioOnlyBudget is the byte ceiling an audio-only encode of a size budget
// goal carried, zero for any other plan.
func audioOnlyBudget(res *ActionResult, read func(string) any) int64 {
	if getString(res.State, "plan_source") != "audio_only" {
		return 0
	}
	plan := getPlan(read("plan"))
	if plan == nil || plan.SizePolicy == nil {
		return 0
	}
	return plan.SizePolicy.MaxCandidateBytes
}

// audioOnlyOvershootMessage says how far a compacted-audio-only file landed
// over the budget, as the x265 encode's does: the budget, the size it came
// out at and the gap.
func audioOnlyOvershootMessage(budget, candidate int64) string {
	if candidate <= 0 {
		return fmt.Sprintf("Copying the video and compacting the lossless audio alone came out over the %s budget. Original preserved.", formatBudgetBytes(budget))
	}
	gap := candidate - budget
	return fmt.Sprintf("Copying the video and compacting the lossless audio alone came out at %s, over the %s budget by %s (%.1f%%). The video was copied unchanged, so only a larger budget or fewer tracks can fit. Original preserved.", formatBudgetBytes(candidate), formatBudgetBytes(budget), formatBudgetBytes(gap), float64(gap)/float64(budget)*100)
}
