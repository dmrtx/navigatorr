package maintenanceui

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// nextStep is one way to go on with a file whose job ended without a result.
// The server decides the list from what the job measured; the client shows it
// and asks for a step by its ID, never with inputs of its own. Inputs are the
// goal inputs the step starts the file with; "same" and "x265" start from the
// job's own inputs instead, and "tracks" opens the goal form.
type nextStep struct {
	ID     string         `json:"id"`
	Label  string         `json:"label"`
	Detail string         `json:"detail"`
	Inputs map[string]any `json:"inputs,omitempty"`
}

// resultExplanation says what happened to a job and what it showed about the
// file, in measured numbers; empty parts are unknown.
type resultExplanation struct {
	Happened string   `json:"happened,omitempty"`
	Learned  []string `json:"learned,omitempty"`
}

// stepReasons are the endings that offer next steps besides the no-winner
// rounds and the remedies: an Audio only goal that found nothing to convert.
var stepReasons = map[string]bool{"audio_only_nothing_to_convert": true}

// The budget a size goal is offered after it missed its saving: 80% of the
// original is a 20% saving, past the plain goal's 15% minimum.
const offeredBudgetPercent = 80.0

// maxBudgetPercent is the largest size_budget_percent a size goal takes.
const maxBudgetPercent = 85.0

// endedWithoutResult is a finished job that next steps apply to.
func endedWithoutResult(r operationRecord) bool {
	if r.inst.ActionName != "transcode_media" {
		return false
	}
	reason := operationString(operationValue(r, "reason_code"))
	return r.inst.Status == store.ActionStatusCancelled || isNoWinnerRound(r) || remedyMode(r) != "" ||
		(r.inst.Status == store.ActionStatusCompleted && stepReasons[reason])
}

// jobGoal is the goal a job ran under, Audio only told apart from Save space.
func jobGoal(r operationRecord) (mode string, audioOnly bool) {
	mode = firstNonEmpty(operationString(r.inputs["mode"]), operationString(r.state["mode"]))
	audioOnly = mode == "size" && (r.inputs["audio_only"] == true || operationMap(operationMap(r.state["mode_policy"])["size"])["audio_only"] == true)
	return mode, audioOnly
}

// jobSourceReport is the probe the job planned from, nil when it has none.
func jobSourceReport(r operationRecord) *mediainspect.DetailedReport {
	raw, ok := r.state["source_report"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var rep mediainspect.DetailedReport
	if json.Unmarshal(data, &rep) != nil || len(rep.Video) == 0 {
		return nil
	}
	return &rep
}

// sourceSplit is what the source's video and audio weigh, and what the
// size goal's audio level would make of the audio, from the probe alone.
type sourceSplit struct {
	total, video, audio, audioUnderSize int64
	audioTracks, subtitles              int
	tall, hdr                           bool
}

func splitOf(rep *mediainspect.DetailedReport) (sourceSplit, bool) {
	if rep == nil {
		return sourceSplit{}, false
	}
	p := action.PreviewStreams(rep)
	s := sourceSplit{total: p.SourceBytes, video: p.VideoBytes, audioTracks: len(rep.Audio), subtitles: len(rep.Subtitles), hdr: mediainspect.IsHDRorDolbyVisionReport(rep)}
	for _, v := range rep.MainVideo() {
		s.tall = s.tall || v.Height > 1080
	}
	for _, a := range p.Audio {
		s.audio += a.Bytes
		s.audioUnderSize += a.Outputs["size"].Bytes
	}
	// An unknown audio size leaves the video unknown too: no split.
	return s, s.total > 0 && s.video > 0 && s.audio > 0
}

func percentOf(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

// explainResult says what happened and what was learned. It reads only what
// the job stored: its reason, the original and candidate sizes, its ladder or
// budget record, and the probe of its source.
func explainResult(r operationRecord) resultExplanation {
	var e resultExplanation
	mode, audioOnly := jobGoal(r)
	reason := operationString(operationValue(r, "reason_code"))
	original := int64(operationNumber(operationValue(r, "original_size")))
	candidate := int64(operationNumber(operationValue(r, "candidate_size_bytes")))
	minimum := operationNumber(operationMap(r.state["mode_policy"])["min_savings_percent"])
	switch reason {
	case "insufficient_savings", "size_growth", "size_growth_exceeded":
		need := fmt.Sprintf("at least %.0f%% smaller", minimum)
		if minimum <= 0 {
			need = "smaller"
		}
		if original > 0 && candidate > 0 {
			saved := 100 - percentOf(candidate, original)
			if saved > 0 {
				e.Happened = fmt.Sprintf("The new file came out %.0f%% smaller (%s → %s); this goal keeps it only if it is %s.", saved, formatSize(original), formatSize(candidate), need)
			} else {
				e.Happened = fmt.Sprintf("The new file came out larger than the original (%s → %s); this goal keeps it only if it is %s.", formatSize(original), formatSize(candidate), need)
			}
		} else {
			e.Happened = fmt.Sprintf("The estimate showed this goal could not make the file %s.", need)
		}
	case "size_budget_exceeded":
		target := operationNumber(operationMap(operationValue(r, "size_budget"))["budget_bytes"])
		if original > 0 && candidate > 0 && target > 0 {
			e.Happened = fmt.Sprintf("The new file came out at %.0f%% of the original; the target was %.0f%%.", percentOf(candidate, original), percentOf(int64(target), original))
		} else {
			e.Happened = "The new file did not fit the target size."
		}
	case "quality_not_met", "no_suitable_candidate", "search_budget_exhausted":
		tried, _ := operationMap(operationValue(r, "quality_ladder"))["tried"].([]any)
		if len(tried) > 1 {
			e.Happened = fmt.Sprintf("Each of the %d settings tried missed this goal's quality limits.", len(tried))
		} else {
			e.Happened = "The new file missed this goal's quality limits."
		}
	case "hdr_requires_compact":
		e.Happened = "Save space keeps the picture as it is and cannot encode HDR or Dolby Vision."
	case "compact_not_needed":
		e.Happened = "The file is already 1080p SDR or smaller, so Shrink to 1080p SDR has nothing to shrink."
	case "audio_only_nothing_to_convert":
		e.Happened = "Every audio track is already compact and no track is removed, so Audio only has nothing to make smaller."
	}
	if r.inst.Status == store.ActionStatusCancelled {
		e.Happened = "The job was cancelled before it finished."
	}
	if s, ok := splitOf(jobSourceReport(r)); ok {
		e.Learned = append(e.Learned, fmt.Sprintf("Video is %s (%.0f%% of the file); audio is %s (%.0f%%) in %d %s.", formatSize(s.video), percentOf(s.video, s.total), formatSize(s.audio), percentOf(s.audio, s.total), s.audioTracks, plural(s.audioTracks, "track", "tracks")))
		if s.audio > s.video {
			e.Learned = append(e.Learned, "The audio weighs more than the video, so shrinking the video alone cannot save much.")
		}
		if saved := s.audio - s.audioUnderSize; !audioOnly && mode != "compact" && saved > 0 {
			e.Learned = append(e.Learned, fmt.Sprintf("Converting the audio alone would save about %s (%.0f%% of the file).", formatSize(saved), percentOf(saved, s.total)))
		}
	}
	return e
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func formatSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (b+1023)/1024)
	}
}

// nextStepsFor lists what the file can do next, the recommended step first.
// Steps follow what the job measured: the media engine missing quality offers
// x265, heavy audio offers Audio only, a missed saving offers a smaller one
// (when the encode saved at least that) and a target size,
// HDR or a picture taller than 1080p offers Shrink to 1080p SDR. Running the
// same settings again comes last, and only where something may change it.
func nextStepsFor(r operationRecord) []nextStep {
	if !endedWithoutResult(r) {
		return nil
	}
	mode, audioOnly := jobGoal(r)
	reason := operationString(operationValue(r, "reason_code"))
	split, known := splitOf(jobSourceReport(r))
	audioSaving := split.audio - split.audioUnderSize
	var steps []nextStep
	add := func(s nextStep) {
		for _, have := range steps {
			if have.ID == s.ID {
				return
			}
		}
		steps = append(steps, s)
	}
	audioOnlyStep := func() {
		if audioOnly || !known || audioSaving <= 0 || percentOf(audioSaving, split.total) < 5 {
			return
		}
		add(nextStep{ID: "audio_only", Label: "Audio only", Detail: fmt.Sprintf("Keep the video as it is and convert only the audio: about %s smaller (%.0f%%).", formatSize(audioSaving), percentOf(audioSaving, split.total)), Inputs: map[string]any{"mode": "size", "audio_only": true}})
	}
	compactStep := func() {
		if mode == "compact" || !known || !(split.tall || split.hdr) {
			return
		}
		add(nextStep{ID: "compact", Label: "Shrink to 1080p SDR", Detail: "Convert the picture to 1080p SDR, which saves far more than keeping it as it is.", Inputs: map[string]any{"mode": "compact"}})
	}
	tracksStep := func() {
		if known && (split.audioTracks > 1 || split.subtitles > 0) {
			add(nextStep{ID: "tracks", Label: "Choose tracks", Detail: "Pick the audio and subtitle tracks to keep, then choose a goal."})
		}
	}
	if r.inst.Status == store.ActionStatusCancelled {
		add(nextStep{ID: "same", Label: "Run again", Detail: "Start the same goal again from the beginning."})
		return steps
	}
	if remedy := remedyMode(r); remedy != "" {
		if remedy == "compact" {
			add(nextStep{ID: "compact", Label: "Convert to SDR 1080p", Detail: "Shrink to 1080p SDR converts HDR and Dolby Vision to SDR.", Inputs: map[string]any{"mode": "compact"}})
		} else {
			add(nextStep{ID: "size", Label: "Make it smaller", Detail: "Save space re-encodes the video at its current resolution.", Inputs: map[string]any{"mode": "size"}})
		}
		// Audio only keeps HDR and Dolby Vision as they are.
		audioOnlyStep()
		tracksStep()
		return steps
	}
	if audioOnly {
		add(nextStep{ID: "size", Label: "Save space", Detail: "Encode the video too, which is where most of the size usually is.", Inputs: map[string]any{"mode": "size"}})
		compactStep()
		tracksStep()
		if reason != "audio_only_nothing_to_convert" {
			add(nextStep{ID: "same", Label: "Run again unchanged", Detail: "Run Audio only again with the same settings."})
		}
		return steps
	}
	hardware, _, frozen := action.FrozenHardware(r.state)
	x265 := offersWithoutHardware(r) || (mode == "size" && frozen && hardware)
	switch reason {
	case "quality_not_met", "no_suitable_candidate", "search_budget_exhausted":
		if x265 {
			add(nextStep{ID: "x265", Label: "Try with x265", Detail: "x265 is slower than the media engine but keeps more quality at the same size."})
		}
		compactStep()
		audioOnlyStep()
	case "size_budget_exceeded":
		original := float64(operationNumber(operationValue(r, "original_size")))
		candidate := float64(operationNumber(operationValue(r, "candidate_size_bytes")))
		// The next 5% step above what this encode reached, while a budget can
		// still name it (at most 85% of the source).
		if percent := math.Ceil(candidate*100/original/5)*5 + 5; original > 0 && candidate > 0 && percent <= maxBudgetPercent {
			add(nextStep{ID: "budget", Label: fmt.Sprintf("Aim for %.0f%% of the original", percent), Detail: fmt.Sprintf("A target just above the %.0f%% this encode reached.", candidate*100/original), Inputs: map[string]any{"mode": "size", "size_budget_percent": percent}})
		}
		if x265 {
			add(nextStep{ID: "x265", Label: "Try with x265", Detail: "x265 is slower than the media engine but fits more quality into the target."})
		}
		audioOnlyStep()
		compactStep()
	default:
		// The saving was too small: heavy audio first, then a target size.
		if known && split.audio*100 >= split.total*30 {
			audioOnlyStep()
		}
		frozenSize := operationMap(operationMap(r.state["mode_policy"])["size"])
		// A size goal that saved a little, but less than its minimum, can
		// keep that smaller saving.
		original := operationNumber(operationValue(r, "original_size"))
		candidate := operationNumber(operationValue(r, "candidate_size_bytes"))
		minimum := operationNumber(operationMap(r.state["mode_policy"])["min_savings_percent"])
		if saved := 100 - candidate*100/original; reason == "insufficient_savings" && mode == "size" && frozenSize["size_budget"] == nil && frozenSize["smaller_saving"] != true &&
			original > 0 && candidate > 0 && saved >= transcode.SizeSmallerSavingPercent && saved < minimum {
			add(nextStep{ID: "smaller", Label: fmt.Sprintf("Accept a %d%% saving", transcode.SizeSmallerSavingPercent), Detail: fmt.Sprintf("This encode saved %.0f%%. Run Save space again and keep any file at least %d%% smaller.", saved, transcode.SizeSmallerSavingPercent), Inputs: map[string]any{"mode": "size", "accept_smaller_saving": true}})
		}
		if mode == "size" && frozenSize["size_budget"] == nil {
			add(nextStep{ID: "budget", Label: fmt.Sprintf("Aim for %.0f%% of the original", offeredBudgetPercent), Detail: "Encode to a target size: it lowers the quality floor to reach the saving.", Inputs: map[string]any{"mode": "size", "size_budget_percent": offeredBudgetPercent}})
		}
		compactStep()
		audioOnlyStep()
		if x265 {
			add(nextStep{ID: "x265", Label: "Try with x265", Detail: "x265 is slower but usually smaller than the media engine at the same quality."})
		}
	}
	tracksStep()
	add(nextStep{ID: "same", Label: "Run again unchanged", Detail: "The same goal and settings; useful only after a worker or policy change."})
	return steps
}

// stepByID finds a step the server offers for this job.
func stepByID(r operationRecord, id string) (nextStep, bool) {
	for _, s := range nextStepsFor(r) {
		if s.ID == id {
			return s, true
		}
	}
	return nextStep{}, false
}
