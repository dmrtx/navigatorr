package maintenanceui

import (
	"fmt"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// replacementFacts are the numbers a person needs to decide on one
// replacement: how big the file is now and would be, how good the result
// measured, and what happened to its tracks. They are display facts read from
// the transcode job's own record; no digest or approval covers them.
type replacementFacts struct {
	SourceBytes       int64    `json:"source_bytes,omitempty"`
	CandidateBytes    int64    `json:"candidate_bytes,omitempty"`
	SavedBytes        int64    `json:"saved_bytes"`
	VMAFMean          *float64 `json:"vmaf_mean,omitempty"`
	VMAFP5            *float64 `json:"vmaf_p5,omitempty"`
	VMAFWorstWindow   *float64 `json:"vmaf_worst_window,omitempty"`
	SizeBudgetOutcome string   `json:"size_budget_outcome,omitempty"`
	TracksMessage     string   `json:"tracks_message,omitempty"`
	AudioSummary      string   `json:"audio_summary,omitempty"`
}

var audioTargetNames = map[string]string{"aac": "AAC", "ac3": "AC-3", "eac3": "E-AC-3", "opus": "Opus", "flac": "FLAC"}

// replacementFactsOf reads the facts from a transcode_media record.
func replacementFactsOf(r operationRecord) replacementFacts {
	if r.savings.SourceBytes == nil && r.savings.CandidateBytes == nil {
		r.savings = projectOperationSavings(r, time.Now())
	}
	var facts replacementFacts
	if r.savings.SourceBytes != nil {
		facts.SourceBytes = *r.savings.SourceBytes
	}
	if r.savings.CandidateBytes != nil {
		facts.CandidateBytes = *r.savings.CandidateBytes
	}
	if facts.SourceBytes > 0 && facts.CandidateBytes > 0 {
		facts.SavedBytes = facts.SourceBytes - facts.CandidateBytes
	}
	vmaf := operationMap(operationMap(operationValue(r, "quality_evidence"))["vmaf"])
	number := func(key string) *float64 {
		if v, ok := vmaf[key].(float64); ok {
			return &v
		}
		return nil
	}
	facts.VMAFMean, facts.VMAFP5, facts.VMAFWorstWindow = number("mean"), number("p5"), number("worst_window_mean")
	facts.SizeBudgetOutcome = operationSizeBudgetOutcome(r)
	facts.TracksMessage = operationString(operationValue(r, "tracks_message"))
	converted, target := 0, ""
	conversions, _ := operationValue(r, "conversions").([]any)
	for _, raw := range conversions {
		c := operationMap(raw)
		if operationString(c["stream_type"]) != "audio" {
			continue
		}
		converted++
		if target == "" {
			target = operationString(c["to_codec"])
		}
	}
	if converted > 0 {
		name := audioTargetNames[target]
		if name == "" {
			name = "a smaller codec"
		}
		noun := "audio track"
		if converted > 1 {
			noun = "audio tracks"
		}
		facts.AudioSummary = fmt.Sprintf("%d %s converted to %s", converted, noun, name)
	}
	return facts
}

// memberFacts keys the facts of each plan or review member by the transcode
// action it came from. A member whose job cannot be read has none and is
// listed without them.
func (s *Server) memberFacts(actionIDs []string) map[string]replacementFacts {
	out := map[string]replacementFacts{}
	st := s.engine.Deps().Store
	if st == nil {
		return out
	}
	for _, id := range actionIDs {
		child, err := st.GetActionInstanceIfExists(id)
		if err != nil || child == nil || child.ActionName != "transcode_media" || child.Status != store.ActionStatusCompleted {
			continue
		}
		out[id] = replacementFactsOf(operationRecord{inst: *child, inputs: decodeOperationJSON(child.InputsJSON), outputs: decodeOperationJSON(child.OutputsJSON), state: decodeOperationJSON(child.StateJSON)})
	}
	return out
}
