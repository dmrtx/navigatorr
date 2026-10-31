package action

import (
	"context"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// Goal verdict statuses. "unknown" means a gate needs evidence this preview
// could not gather (worker capabilities, decoded frames); the job decides then.
const (
	GoalOK      = "ok"
	GoalSkip    = "skip"
	GoalUnknown = "unknown"
)

// SourceGoalModes lists the automatic goals in display order.
var SourceGoalModes = []string{"size", "compact", "quality", "x265_preserve"}

// AudioOnlyGoal is the key of the audio-only size goal (mode=size with
// audio_only) among a source's goal verdicts.
const AudioOnlyGoal = "audio_only"

// GoalVerdict is whether one goal would take a source or end it as a skip.
type GoalVerdict struct {
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code,omitempty"`
}

// SourceGoals is the per-goal preview of a single source and the goal worth
// offering first.
type SourceGoals struct {
	Goals       map[string]GoalVerdict `json:"goals"`
	Recommended string                 `json:"recommended,omitempty"`
	// Dynamic is the source's dynamic range classified with the same evidence
	// the compact gate uses; nil when that evidence could not be read.
	Dynamic *mediainspect.DynamicRange `json:"-"`
}

// SourceClassifier classifies a source's dynamic range, with decoded-frame
// evidence when it is HDR (mediainspect.ClassifySource).
type SourceClassifier func(context.Context, *mediainspect.DetailedReport) (mediainspect.DynamicRange, error)

// SourceGoals runs the same source gates a transcode_media job applies in
// preflight (modeSourceReason, then the compact plan for the compact goal)
// without queueing anything, so a preview and the job cannot disagree. A gate
// that needs evidence the caller could not collect yields "unknown", never a
// guess. classify may be nil to use ffprobe on path with the engine's ffprobe.
func (e *Engine) SourceGoals(ctx context.Context, path string, rep *mediainspect.DetailedReport, classify SourceClassifier) SourceGoals {
	out := SourceGoals{Goals: make(map[string]GoalVerdict, len(SourceGoalModes)+1)}
	if rep == nil {
		return out
	}
	if classify == nil {
		classify = func(ctx context.Context, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
			return mediainspect.ClassifySource(ctx, e.deps.Ffprobe, path, rep)
		}
	}
	// Decoded frames are read at most once, for the compact gate and the label.
	var classified *mediainspect.DynamicRange
	var classifyErr error
	inner := classify
	classify = func(ctx context.Context, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
		if classified == nil && classifyErr == nil {
			d, err := inner(ctx, rep)
			if err != nil {
				classifyErr = err
			} else {
				classified = &d
			}
		}
		if classifyErr != nil {
			return mediainspect.DynamicRange{}, classifyErr
		}
		return *classified, nil
	}
	gateRep := coverArtGateReport(rep, e.cachedCoverArtSupport(ctx))
	hfrSupported, hfrErr := (&hfrQualitySupport{e: e}).forSource(ctx, gateRep)
	for _, mode := range SourceGoalModes {
		reason := modeSourceReason(mode, gateRep, hfrSupported)
		verdict := GoalVerdict{Status: GoalOK}
		switch {
		case reason == "hfr_quality_unavailable" && hfrErr != nil:
			verdict = GoalVerdict{Status: GoalUnknown, ReasonCode: "worker_capabilities_unavailable"}
		case reason == "" && mode == "compact":
			policy, err := modePolicyFor("compact", false, 0)
			if err != nil {
				verdict = GoalVerdict{Status: GoalUnknown, ReasonCode: "compact_policy_unavailable"}
				break
			}
			_, reason = compactSourcePlanWith(ctx, gateRep, &policy, classify)
			switch reason {
			case "":
			case "compact_frame_evidence_unavailable", "compact_policy_unavailable":
				verdict = GoalVerdict{Status: GoalUnknown, ReasonCode: reason}
			default:
				verdict = GoalVerdict{Status: GoalSkip, ReasonCode: reason}
			}
		case reason != "":
			verdict = GoalVerdict{Status: GoalSkip, ReasonCode: reason}
		}
		out.Goals[mode] = verdict
	}
	out.Goals[AudioOnlyGoal] = GoalVerdict{Status: GoalOK}
	if reason := audioOnlySourceReason(gateRep); reason != "" {
		out.Goals[AudioOnlyGoal] = GoalVerdict{Status: GoalSkip, ReasonCode: reason}
	}
	if d, err := classify(ctx, gateRep); err == nil {
		out.Dynamic = &d
	}
	out.Recommended = recommendedGoal(gateRep, out.Goals)
	return out
}

// recommendedGoal prefers compact for HDR and Dolby Vision when compact accepts
// them, otherwise the size goal: an SDR 4K source keeps its resolution under
// Save space, which also compresses the audio more. A goal that would skip is
// never recommended.
func recommendedGoal(rep *mediainspect.DetailedReport, goals map[string]GoalVerdict) string {
	video := rep.MainVideo()
	if len(video) == 1 && goals["compact"].Status == GoalOK && isSourceHDRorDV(rep) {
		return "compact"
	}
	if goals["size"].Status != GoalSkip {
		return "size"
	}
	// Save space would skip an HDR source. When the compact check could not read
	// its frames in time, compact is still the only goal that converts it; the
	// job's own preflight re-checks it.
	if len(video) == 1 && goals["compact"].Status == GoalUnknown && isSourceHDRorDV(rep) {
		return "compact"
	}
	return ""
}
