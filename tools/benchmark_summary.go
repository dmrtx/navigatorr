package tools

import (
	"encoding/json"
	"fmt"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// benchmarkSummary is the encode summary of a goal under the recipe, under the
// action_status key "benchmark" that earlier versions gave their sample test.
type benchmarkSummary struct {
	Outcome  string `json:"outcome"`
	Message  string `json:"message"`
	NextStep string `json:"next_step"`
	// UserNextStep says the same for a person in the UI; NextStep keeps the
	// tool-level guidance for agents.
	UserNextStep  string           `json:"user_next_step,omitempty"`
	ProgressScope string           `json:"progress_scope"`
	Audio         []benchmarkAudio `json:"planned_audio,omitempty"`
	AudioNote     string           `json:"audio_note,omitempty"`
	PlanSource    string           `json:"plan_source,omitempty"`
	PlanNote      string           `json:"plan_note,omitempty"`
	// SizeBudget is the operation's size budget: what it planned, the
	// rate-quality points and, once measured, how the result compares with the
	// quality target.
	SizeBudget *action.SizeBudgetRecord `json:"size_budget,omitempty"`
}

type benchmarkAudio struct {
	Stream      int    `json:"stream"`
	Language    string `json:"language,omitempty"`
	Channels    int    `json:"channels,omitempty"`
	SourceCodec string `json:"source_codec"`
	Operation   string `json:"operation"`
	OutputCodec string `json:"output_codec"`
	TargetKbps  int    `json:"target_kbps,omitempty"`
}

// Read a known projection directly: the generic operational map drops values
// above 1 KiB, including the very evidence needed to explain most rejections.
func benchmarkField(res *action.ActionResult, key string, dst any) bool {
	v, ok := res.Outputs[key]
	if !ok {
		v, ok = res.State[key]
	}
	if !ok || v == nil {
		return false
	}
	b, err := json.Marshal(v)
	return err == nil && json.Unmarshal(b, dst) == nil
}

func summaryPlanSource(res *action.ActionResult) string {
	var source string
	benchmarkField(res, "plan_source", &source)
	return source
}

// compactBenchmark summarizes the encode of a goal. Actions that ran the
// removed sample search have no summary: their state is shown as stored.
func compactBenchmark(res *action.ActionResult) *benchmarkSummary {
	if res.ActionName != "transcode_media" || summaryInput(res, "mode") == "" {
		return nil
	}
	ladderStep, ladder := action.SizeBudgetLadderStep(res.State)
	recipeStep, recipe := action.RecipeStep(res.State)
	planSource := summaryPlanSource(res)
	s := &benchmarkSummary{ProgressScope: "Worker progress is for the full encode, not a sample test.", Outcome: "preparing", Message: "Preparing the goal's first encode; no quality decision yet.", NextStep: "Monitor this action with action_status. A progress percentage does not mean quality passed.", UserNextStep: "Nothing to do yet. The first encode is being prepared."}
	switch {
	case res.Status == action.StatusCancelled:
		s.Outcome, s.Message, s.NextStep = "cancelled", "Cancelled.", "Keep the original; inspect action details before starting other work."
		s.UserNextStep = "Nothing else to do: the original is kept."
	case res.Status == action.StatusWaitingDecision:
		// A paused workflow is not encoding anything: it waits only for the user.
		var reason string
		benchmarkField(res, "reason_code", &reason)
		s.Outcome, s.Message = "attention", res.WaitingReason
		if s.Message == "" {
			s.Message = "Waiting for your decision; no quality result was recorded."
		}
		s.NextStep = "Review the waiting reason and choose one of the displayed decisions."
		s.UserNextStep = "Choose one of the options shown. Nothing runs until you decide."
		if reason == "quality_policy_unavailable" {
			s.Outcome, s.Message = reason, "No encode was started: the worker cannot run the quality checks this goal requires. The original is preserved."
			s.NextStep = "Cancel to keep the original. To process this file, make the missing quality check available on the worker (the waiting reason names it) and start the goal again; quality requirements are never relaxed automatically."
			s.UserNextStep = "Keep the original. The worker cannot run a quality check this goal needs; once it can, start the goal again."
		}
	case ladder:
		// The VideoToolbox ladder encodes the whole file, and the worker's
		// progress is the full encode of the current step.
		var budget action.SizeBudgetRecord
		benchmarkField(res, "size_budget", &budget)
		s.Outcome, s.Message = "size_budget_ladder", budget.Message
		s.ProgressScope = fmt.Sprintf("Worker progress is for the full encode with %s.", ladderStep)
		s.NextStep = "Monitor this action. Each step encodes the whole file, a step over the budget moves to the next, and the first one within the budget is checked against the quality floor. The original is never replaced without the usual approval."
		s.UserNextStep = fmt.Sprintf("Nothing to do yet. The whole file is being encoded with %s; the first result within your size budget is checked against the quality floor before you approve any replacement.", ladderStep)
		if res.Status == action.StatusFailed {
			s.Outcome, s.Message = "execution_error", "The size budget encode could not finish."
			s.NextStep = "Inspect the action error and fix the execution problem before retrying."
			s.UserNextStep = "The encode could not finish. Check the error in the job details."
		}
	case recipe:
		// The worker's progress is the full encode of the current step.
		s.Outcome, s.Message = "recipe", "Encoding the whole file with "+recipeStep+"."
		if ladder := action.QualityLadderOf(res.State); ladder != nil && ladder.Message != "" {
			s.Message = ladder.Message
		} else {
			var budget action.SizeBudgetRecord
			if benchmarkField(res, "size_budget", &budget) && budget.Message != "" {
				s.Message = budget.Message
			}
		}
		s.ProgressScope = fmt.Sprintf("Worker progress is for the full encode with %s.", recipeStep)
		s.NextStep = "Monitor this action. The whole file is encoded with the goal's fixed settings and checked against its quality limits on the whole file; a quality miss encodes it again one step better. The original is never replaced without the usual approval."
		s.UserNextStep = fmt.Sprintf("Nothing to do yet. The whole file is being encoded with %s and then checked against this goal's quality limits before you approve any replacement.", recipeStep)
		if res.Status == action.StatusFailed {
			s.Outcome, s.Message = "execution_error", "The encode could not finish."
			s.NextStep = "Inspect the action error and fix the execution problem before retrying."
			s.UserNextStep = "The encode could not finish. Check the error in the job details."
		}
	case planSource == "size_budget":
		var budget action.SizeBudgetRecord
		benchmarkField(res, "size_budget", &budget)
		s.Outcome, s.Message = "size_budget", budget.Message
		s.NextStep = "Monitor this action. The result is reported as below the quality target unless it measures within the goal's limits, and it is held to a fixed quality floor; the original is never replaced without the usual approval."
		s.UserNextStep = "Nothing to do yet. The file is encoded once at your size budget; if it comes out below the quality target you will be told before you approve any replacement."
	case planSource == "audio_only":
		s.Outcome, s.Message = "audio_only", "The video is copied unchanged and only the lossless audio is compacted to AAC."
		var audioOnly struct {
			AudioMode string `json:"audio_mode"`
		}
		if benchmarkField(res, "plan", &audioOnly) && transcode.IsAudioLevelMode(audioOnly.AudioMode) {
			s.Message = "The video is copied unchanged and only the audio is re-encoded to AAC."
		}
		s.NextStep = "Monitor this action. The candidate must prove its video bit-identical to the source and still meet the same minimum savings; the original is never changed before that."
		s.UserNextStep = "Nothing to do yet. The new file keeps the video exactly as it is and only makes the audio smaller; the original is not changed before that is checked."
	case res.Status == action.StatusFailed:
		s.Outcome, s.Message, s.NextStep = "execution_error", "The encode could not finish reliably.", "Inspect the action error and fix the execution problem before retrying; changing compression settings is not an established remedy."
		s.UserNextStep = "The encode could not finish. Check the error in the job details and fix it before retrying."
	}
	if explanation := action.ExplainTranscode(res); explanation != nil && (explanation.Outcome == "no_suitable_candidate" || explanation.Outcome == "size_rejected") {
		s.Outcome = explanation.Outcome
		s.Message = explanation.Message
		s.NextStep = "Keep the original, or explicitly choose another processing goal. Technical profiles are optional; quality requirements are never relaxed automatically."
		s.UserNextStep = "Keep the original, or choose another goal. Quality requirements are never lowered automatically."
	}
	benchmarkField(res, "plan_note", &s.PlanNote)
	if s.PlanNote != "" {
		s.PlanSource = planSource
	}
	if planSource == "size_budget" {
		var budget action.SizeBudgetRecord
		if benchmarkField(res, "size_budget", &budget) && budget.BudgetBytes > 0 {
			s.SizeBudget = &budget
		}
	}
	var plan transcode.Plan
	var source struct {
		Audio       []mediainspect.DetailedStream `json:"audio"`
		DurationSec float64                       `json:"duration_sec"`
	}
	if benchmarkField(res, "plan", &plan) && benchmarkField(res, "original", &source) {
		for _, a := range source.Audio {
			if len(s.Audio) >= 8 {
				break
			}
			target, kbps := transcode.AudioTarget(plan.AudioMode, transcode.DetailedAudioTrackAt(a.Codec, a.Profile, a.Title, a.Tags, a.Channels, a.ChannelLayout, a.BitRate, source.DurationSec))
			entry := benchmarkAudio{Stream: a.Index, Language: a.Language, Channels: a.Channels, SourceCodec: a.Codec, Operation: "copy", OutputCodec: a.Codec}
			if target != "copy" {
				entry.Operation, entry.OutputCodec, entry.TargetKbps = "convert_lossy", target, kbps
			}
			s.Audio = append(s.Audio, entry)
		}
		s.AudioNote = "Planned audio only (first 8 tracks). Compact mode copies already compact codecs such as EAC3."
	}
	return s
}
