package action

import (
	"context"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// compactVideoSourceKey marks a size goal that copied the video because the
// source's was already compact (see compactVideoSource).
const compactVideoSourceKey = "compact_video_source"

// reasonVideoAlreadyCompact ends a size goal whose video is already compact
// and whose audio alone would not save enough (or has nothing to convert).
const reasonVideoAlreadyCompact = "video_already_compact"

// maxPlausibleFPS bounds the frame rate compactVideoSource trusts.
const maxPlausibleFPS = 120

// compactVideoSource reports a source whose programme video is already HEVC or
// AV1 at or below sizeBudgetGoodBitsPerPixel. A size goal's x265 ladder starts
// at a CRF that rarely makes such a stream smaller, and on a film each rung is
// hours of encoding, so the goal copies the video and converts the audio only.
// A size or frame rate that is unknown or implausible (a container's timebase
// read as the frame rate) never counts as compact.
func compactVideoSource(rep *mediainspect.DetailedReport) bool {
	if rep == nil || rep.DurationSec <= 0 {
		return false
	}
	main := rep.MainVideo()
	if len(main) != 1 {
		return false
	}
	v := main[0]
	switch strings.ToLower(v.Codec) {
	case "hevc", "av1":
	default:
		return false
	}
	if v.FPS <= 0 || v.FPS > maxPlausibleFPS {
		return false
	}
	pixels := float64(v.Width) * float64(v.Height) * v.FPS
	bytes := PreviewStreams(rep).VideoBytes
	if pixels <= 0 || bytes <= 0 {
		return false
	}
	return float64(bytes)*8/rep.DurationSec/pixels <= sizeBudgetGoodBitsPerPixel
}

// startCompactSourceAudioOnly plans a size goal (without a budget) on a source
// whose video is already compact: the video is copied and only the audio is
// converted, in place of the quality ladder. When the audio alone would not
// save enough, the job ends at once without an encode.
func (e *Engine) startCompactSourceAudioOnly(ctx context.Context, ec *ExecutionContext) (StepResult, bool) {
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" || QualityLadderOf(ec.State) != nil {
		return StepResult{}, false
	}
	if getBool(ec.State, compactVideoSourceKey) && (getBool(ec.State, "size_audio_only") || getBool(ec.State, "skip_transcode")) {
		return StepResult{Status: StepCompleted}, true // planned already; this step ran again
	}
	if getBool(ec.State, "size_audio_only") {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Size == nil || !snapshot.Size.AudioOnlyFallback || snapshot.Recipe == nil || snapshot.Recipe.Kind != RecipeQualityLadder {
		return StepResult{}, false
	}
	if !compactVideoSource(getSourceReport(ec.State["source_report"])) {
		return StepResult{}, false
	}
	ec.State[compactVideoSourceKey] = true
	outputs := map[string]any{compactVideoSourceKey: true}
	res, handled := e.sizeAudioOnlyFallback(ctx, ec, outputs, "")
	if !handled {
		return skipAudioOnly(ec, outputs, reasonVideoAlreadyCompact)
	}
	if res.Status != StepCompleted {
		return res, true
	}
	if res.Outputs == nil {
		res.Outputs = outputs
	}
	switch getString(ec.State, "reason_code") {
	case "audio_compaction_insufficient":
		ec.State["reason_code"], res.Outputs["reason_code"] = reasonVideoAlreadyCompact, reasonVideoAlreadyCompact
	}
	if getString(ec.State, "plan_source") == "audio_only" {
		res.Outputs["plan_note"] = "The video is already compact, so re-encoding it would not make it smaller; it is copied unchanged and only the audio is re-encoded."
	}
	return res, true
}
