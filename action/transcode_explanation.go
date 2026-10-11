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
}

func ExplainTranscode(res *ActionResult) *TranscodeExplanation {
	if res == nil || getString(res.Inputs, "mode") == "" {
		return nil
	}
	e := &TranscodeExplanation{Goal: getString(res.Inputs, "mode"), Outcome: "processing", Message: "Automatic testing and verification; originals are preserved."}
	if goal := getString(res.State, "batch_goal"); goal != "" {
		e.Goal = goal
	}
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
	reason := getString(res.State, "reason_code")
	if reason == "" {
		reason = getString(res.Outputs, "reason_code")
	}
	switch {
	case res.Status == StatusCancelled:
		e.Outcome = "cancelled"
		e.Message = "Stopped; originals preserved."
	case reason == "no_suitable_candidate":
		e.Outcome = "no_suitable_candidate"
		e.Message = "None of the tested configurations met this goal. Originals preserved."
	case reason == "insufficient_savings":
		e.Outcome = "size_rejected"
		e.Message = "The full conversion did not meet this goal's size limit. Original preserved."
	case res.Status == StatusFailed:
		e.Outcome = "execution_error"
		e.Message = "Processing could not finish reliably. Original preserved; inspect the execution error."
	case res.Status == StatusWaitingDecision:
		e.Outcome = "attention"
		e.Message = res.WaitingReason
	case res.ActionName == "transcode_media" && res.Status == StatusCompleted && !getBool(res.State, "skip_transcode") && getBool(res.Outputs, "original_intact"):
		e.Outcome = "candidate_ready"
		e.Message = "Candidate verified; original preserved. Quality was measured on samples."
	}
	return e
}
