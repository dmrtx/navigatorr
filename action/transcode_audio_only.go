package action

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// audioOnlyEstimate is the cheap pre-screen for a size audio-only plan: the
// source size minus what converting its audio under an audio mode would save,
// from stream bitrates and Matroska statistics. A track whose size is
// unknown saves nothing here, so an unknown never passes the gate.
type audioOnlyEstimate struct {
	CandidateBytes int64
	Compacted      int
	Unknown        int
}

func estimateAudioOnly(rep *mediainspect.DetailedReport, sourceBytes int64, audioMode string) audioOnlyEstimate {
	var e audioOnlyEstimate
	if rep == nil || sourceBytes <= 0 || rep.DurationSec <= 0 {
		return e
	}
	var saved int64
	for _, a := range rep.Audio {
		target, kbps := transcode.AudioTarget(audioMode, audioTrackOf(a, rep.DurationSec))
		if target == "copy" {
			continue
		}
		e.Compacted++
		source := audioTrackBytes(a, rep.DurationSec, sourceBytes)
		if source <= 0 {
			e.Unknown++
			continue
		}
		if compacted := int64(kbps) * 1000 * int64(rep.DurationSec) / 8; source > compacted {
			saved += source - compacted
		}
	}
	e.CandidateBytes = sourceBytes - saved
	return e
}

// audioTrackBytes is an audio track's size read the way the audio policy reads
// its bitrate (transcode.SourceAudioKbps): the stream bitrate (ffprobe's
// bit_rate, or the BPS tag folded into it) first, then the NUMBER_OF_BYTES
// statistic. The decision to convert a track and the estimate of what that
// saves therefore read the same value. A size larger than the whole file is
// unknown.
func audioTrackBytes(a mediainspect.DetailedStream, duration float64, fileBytes int64) int64 {
	if a.BitRate > 0 {
		if n := a.BitRate * int64(duration) / 8; n <= fileBytes {
			return n
		}
		return 0
	}
	return audioStreamBytes(a, duration, fileBytes)
}

// audioStreamBytes is a track's size from its Matroska statistics, else its
// bitrate. The exact NUMBER_OF_BYTES tag wins over language variants, which
// are read in a fixed order; a size larger than the whole file is unknown.
func audioStreamBytes(a mediainspect.DetailedStream, duration float64, fileBytes int64) int64 {
	var exact, variants []string
	for key := range a.Tags {
		if k := strings.ToUpper(key); k == "NUMBER_OF_BYTES" {
			exact = append(exact, key)
		} else if strings.HasPrefix(k, "NUMBER_OF_BYTES-") {
			variants = append(variants, key)
		}
	}
	sort.Strings(exact)
	sort.Strings(variants)
	for _, key := range append(exact, variants...) {
		if n, err := strconv.ParseInt(strings.TrimSpace(a.Tags[key]), 10, 64); err == nil && n > 0 {
			if n > fileBytes {
				return 0
			}
			return n
		}
	}
	if a.BitRate > 0 {
		if n := a.BitRate * int64(duration) / 8; n <= fileBytes {
			return n
		}
	}
	return 0
}

// sizeAudioOnlyFallback is the answer to a ladder that ends without a candidate
// or a budget the audio alone fits: when no video encode meets the quality
// floor and the savings gate, the lossless audio may still be compacted with
// the video stream-copied. It leaves the result to the caller
// (no_suitable_candidate) when the frozen policy or the source has nothing for
// it to do. audioMode is the audio mode the plan converts with; empty takes
// the starting plan's own (a size budget under policy v5 names its level).
func (e *Engine) sizeAudioOnlyFallback(ctx context.Context, ec *ExecutionContext, outputs map[string]any, audioMode string) (StepResult, bool) {
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Mode != "size" || !transcode.IsSizePolicyVersion(snapshot.workerVersion()) || snapshot.Size == nil || !snapshot.Size.AudioOnlyFallback {
		return StepResult{}, false
	}
	base := getPlan(ec.State["plan"])
	if base == nil {
		return StepResult{Status: StepFailed, Error: "the frozen size plan is missing (fail closed)"}, true
	}
	if audioMode == "" {
		audioMode = base.AudioMode
	}
	rep := getSourceReport(ec.State["source_report"])
	original := getInt64(ec.State, "original_size")
	// The estimate is of the output, so it counts the kept tracks only: dropped
	// ones are savings, measured against the whole original below.
	keptRep, keptBytes := keptViewOf(ec.State, rep, original)
	estimate := estimateAudioOnly(keptRep, keptBytes, audioMode)
	if estimate.Compacted == 0 {
		return StepResult{}, false
	}
	outputs["audio_only_estimated_bytes"] = estimate.CandidateBytes
	ec.State["audio_only_estimated_bytes"] = estimate.CandidateBytes
	if !transcode.ModeSizeAllowedVersion(snapshot.Mode, original, estimate.CandidateBytes, snapshot.workerVersion()) {
		if estimate.Unknown > 0 {
			return skipAudioOnly(ec, outputs, "audio_estimate_unavailable")
		}
		return skipAudioOnly(ec, outputs, "audio_compaction_insufficient")
	}
	note := "No video encode met the quality floor and the savings gate; the video is copied unchanged and only lossless audio is compacted."
	if transcode.IsAudioLevelMode(audioMode) {
		note = "No video encode met the quality floor and the savings gate; the video is copied unchanged and only the audio is re-encoded."
	}
	return e.commitAudioOnlyPlan(ctx, ec, base, audioMode, outputs, note)
}

// skipAudioOnly ends an audio-only plan with a reason and no encode.
func skipAudioOnly(ec *ExecutionContext, outputs map[string]any, reason string) (StepResult, bool) {
	ec.State["skip_transcode"] = true
	ec.State["reason_code"] = reason
	outputs["skipped"] = true
	outputs["reason_code"] = reason
	return StepResult{Status: StepCompleted, Outputs: outputs}, true
}

// commitAudioOnlyPlan makes the audio-only plan derived from base, at
// audioMode, the action's encode, once the source has one programme video and
// the worker can prove a copied stream.
func (e *Engine) commitAudioOnlyPlan(ctx context.Context, ec *ExecutionContext, base *transcode.Plan, audioMode string, outputs map[string]any, note string) (StepResult, bool) {
	video := getSourceReport(ec.State["source_report"]).MainVideo()
	if len(video) != 1 {
		return skipAudioOnly(ec, outputs, "unsupported_source")
	}
	if !e.workersConfigured() {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, true
	}
	caps, err := e.actionWorker(ec).Capabilities(ctx)
	if err != nil {
		return workerCapabilityLookupResult(err, "size audio-only"), true
	}
	if err := workerRunsPolicyVersion(caps, base.ModePolicyVersion); err != nil {
		return StepResult{Status: StepFailed, Error: err.Error() + " before encoding"}, true
	}
	if !caps.SizeAudioOnly {
		return skipAudioOnly(ec, outputs, "audio_only_worker_unsupported")
	}
	base, err = planWithAudioMode(base, audioMode)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building audio-only plan: %v", err)}, true
	}
	plan, err := transcode.NewSizeAudioOnlyPlan(base, video[0].BitDepth)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("building audio-only plan: %v", err)}, true
	}
	ec.State["plan"] = plan
	ec.State["plan_source"] = "audio_only"
	ec.State["plan_digest"] = plan.PlanDigest
	ec.State["size_audio_only"] = true
	delete(ec.State, "size_main10")
	outputs["plan"] = plan
	outputs["plan_source"] = "audio_only"
	outputs["plan_digest"] = plan.PlanDigest
	outputs["plan_note"] = note
	return StepResult{Status: StepCompleted, Outputs: outputs}, true
}

// errAudioOnlyGoalWorker is the failure of an audio-only goal on a worker that
// does not run policy v7.
const errAudioOnlyGoalWorker = "worker does not run the audio-only goal; upgrade the worker"

// errCompactAudioLevelsWorker and errSmallerSavingWorker are the failures of a
// policy v8 or v9 plan on a worker that does not run it.
const (
	errCompactAudioLevelsWorker = "worker does not run Shrink at the size goal's audio levels; upgrade the worker"
	errSmallerSavingWorker      = "worker does not run a size goal that accepts a smaller saving; upgrade the worker"
)

// workerRunsPolicyVersion reports, as an error, a plan version newer workers
// add without a fingerprint change, which this worker does not run. Older
// workers refuse those versions at their first gate, so the coordinator checks
// before it submits one.
func workerRunsPolicyVersion(caps transcode.WorkerCapabilities, version string) error {
	switch {
	case version == transcode.ModePolicyV7 && !caps.SizeAudioOnlyGoal:
		return errors.New(errAudioOnlyGoalWorker)
	case version == transcode.ModePolicyV8 && !caps.CompactAudioLevels:
		return errors.New(errCompactAudioLevelsWorker)
	case version == transcode.ModePolicyV9 && !caps.SizeSmallerSaving:
		return errors.New(errSmallerSavingWorker)
	}
	return nil
}

// startAudioOnlyGoal plans the size goal "audio only" (RecipeAudioOnly): the
// video is copied unchanged and the audio converted at the goal's level, with
// the tracks the goal keeps. It needs no saving beyond a smaller file, which
// the final gate measures, so only a source where nothing would convert and
// nothing is removed is skipped here. It is not handled for any other goal.
func (e *Engine) startAudioOnlyGoal(ctx context.Context, ec *ExecutionContext) (StepResult, bool) {
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Recipe == nil || snapshot.Recipe.Kind != RecipeAudioOnly {
		return StepResult{}, false
	}
	base := getPlan(ec.State["plan"])
	if base == nil || base.ModePolicyVersion != transcode.ModePolicyV7 {
		return StepResult{Status: StepFailed, Error: "the audio-only goal's frozen plan is missing (fail closed)"}, true
	}
	outputs := map[string]any{}
	original := getInt64(ec.State, "original_size")
	rep := getSourceReport(ec.State["source_report"])
	keptRep, keptBytes := keptViewOf(ec.State, rep, original)
	estimate := estimateAudioOnly(keptRep, keptBytes, base.AudioMode)
	outputs["audio_only_estimated_bytes"] = estimate.CandidateBytes
	ec.State["audio_only_estimated_bytes"] = estimate.CandidateBytes
	// A selection that keeps every track (hand-picked boxes left as they were)
	// removes nothing either.
	removes := rep != nil && keptRep != nil && len(keptRep.Audio)+len(keptRep.Subtitles) < len(rep.Audio)+len(rep.Subtitles)
	if estimate.Compacted == 0 && !removes {
		return skipAudioOnly(ec, outputs, "audio_only_nothing_to_convert")
	}
	return e.commitAudioOnlyPlan(ctx, ec, base, base.AudioMode, outputs, "This goal copies the video unchanged and only converts the audio (and removes the tracks it does not keep); the file is kept if it comes out smaller.")
}
