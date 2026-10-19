package action

import (
	"encoding/json"

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
	var automatic transcode.BenchmarkAutomaticEvidence
	data, _ := json.Marshal(read("automatic_search"))
	_ = json.Unmarshal(data, &automatic)
	e.Stages = automatic.Stages
	e.VerifiedSamples = automatic.VerifiedSamples
	ids := map[string]bool{}
	for _, stage := range e.Stages {
		for _, id := range stage.Candidates {
			ids[id] = true
		}
	}
	e.TestedCandidates = len(ids)
	var decision transcode.BenchmarkDecision
	data, _ = json.Marshal(read("benchmark_decision"))
	if json.Unmarshal(data, &decision) == nil && decision.Winner != nil {
		e.Encoder = transcode.BenchmarkCandidateVideoCodec(transcode.BenchmarkCandidate{VideoCodec: decision.Winner.VideoCodec})
		e.Hardware = e.Encoder == transcode.VideoCodecHEVCVideoToolbox
	}
	reason := getString(res.State, "reason_code")
	if reason == "" {
		reason = getString(res.Outputs, "reason_code")
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
		return e
	case reason == "no_suitable_candidate":
		e.Outcome = "no_suitable_candidate"
		e.Message = "None of the tested configurations met this goal. Originals preserved."
	case reason == "insufficient_savings" || reason == "size_growth" || reason == "size_growth_exceeded":
		e.Outcome = "size_rejected"
		e.Message = "The full conversion did not meet this goal's size limit. Original preserved."
	case reason == "quality_not_met":
		e.Outcome = "quality_rejected"
		e.Message = "The encoded result did not meet the quality requirements. Original preserved."
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
	case getBool(res.State, "skip_transcode") || getBool(res.Outputs, "skip_transcode") || getBool(res.State, "skipped") || getBool(res.Outputs, "skipped"):
		e.Outcome = "no_changes"
		e.Message = "No candidate was accepted. Inspect the recorded skip reason."
	case getBool(res.Outputs, "original_intact") && getBool(res.State, "size_audio_only"):
		e.Outcome = "candidate_ready"
		e.Message = "Candidate verified; original preserved. The video is copied bit-identically and only the lossless audio was compacted."
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
	return e
}
