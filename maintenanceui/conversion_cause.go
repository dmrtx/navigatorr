package maintenanceui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// conversionCause is why a conversion has no result, as one line with the
// numbers behind it: what was measured, the limit it missed, and each setting
// tried. Stream says which stream made a size miss, when the probe and the
// plan tell.
type conversionCause struct {
	Kind     string          `json:"kind"`
	Summary  string          `json:"summary"`
	Measured *float64        `json:"measured,omitempty"`
	Limit    *float64        `json:"limit,omitempty"`
	Unit     string          `json:"unit,omitempty"`
	Stream   string          `json:"stream,omitempty"`
	Attempts []conversionTry `json:"attempts,omitempty"`
}

// conversionTry is one setting an encode ran and what it measured.
type conversionTry struct {
	Setting   string   `json:"setting"`
	VMAFMean  *float64 `json:"vmaf_mean,omitempty"`
	VMAFWorst *float64 `json:"vmaf_worst,omitempty"`
	Bytes     *int64   `json:"bytes,omitempty"`
	Passed    bool     `json:"passed"`
}

func (c *conversionCause) numbers(measured, limit float64, unit string) *conversionCause {
	c.Measured, c.Limit, c.Unit = &measured, &limit, unit
	return c
}

// causeStatuses are the conversion states a cause explains.
var causeStatuses = map[string]bool{"failed": true, "no_result": true, "replace_failed": true}

// cause is why the conversion of a transcode job has no result, nil when it
// has one or is still running. streams, when known, lets a size miss name the
// stream to blame.
func (f conversionFacts) cause(r operationRecord, streams *action.ConversionStreams) *conversionCause {
	status := f.conversionStatus(r)
	if r.inst.ActionName != "transcode_media" || !causeStatuses[status] {
		return nil
	}
	if status == "replace_failed" {
		rep := f.byID[f.replacements[r.inst.ID]]
		c := failureCause(rep)
		c.Summary = firstLine("Replacement failed: " + c.Summary)
		return c
	}
	return transcodeCause(r, status, streams)
}

// transcodeCause is the cause of a transcode job that failed or ended without
// a new file; status is "failed" or "no_result".
func transcodeCause(r operationRecord, status string, streams *action.ConversionStreams) *conversionCause {
	var c *conversionCause
	if status == "failed" {
		c = failureCause(r)
	} else {
		c = resultCause(r, streams)
	}
	if c != nil && c.Attempts == nil {
		c.Attempts = causeAttempts(r)
	}
	if c != nil {
		c.Summary = firstLine(c.Summary)
	}
	return c
}

// failureCause reads a failed job's stored error.
func failureCause(r operationRecord) *conversionCause {
	text := failureText(r)
	lower := strings.ToLower(text)
	class := failureClass(r, text)
	c := &conversionCause{Kind: "other"}
	switch {
	case savingsFailurePattern.MatchString(text):
		m := savingsFailurePattern.FindStringSubmatch(text)
		predicted, _ := strconv.ParseFloat(m[1], 64)
		minimum, _ := strconv.ParseFloat(m[2], 64)
		c.Kind = "below_min_savings"
		c.Summary = fmt.Sprintf("Predicted saving %.1f%%, below the %.0f%% minimum", predicted, minimum)
		return c.numbers(predicted, minimum, "percent")
	case strings.Contains(text, "transcode candidate rejected by user decision"):
		c.Summary = "The new file was rejected by decision; original kept"
	case action.SourceDamaged(text):
		c.Kind, c.Summary = "file_damaged", "File damaged: ffmpeg found broken data while reading it"
		if strings.Contains(lower, "moov atom not found") {
			c.Summary = "File damaged: its MP4 index (moov atom) is missing"
		}
	case strings.Contains(text, "choosing the tracks to keep"):
		c.Kind, c.Summary = "tracks_mismatch", "The tracks picked for this job are not in the file"
	case class == string(resilience.Cancelled):
		c.Kind, c.Summary = "cancelled", "Cancelled on the worker"
	case class == string(resilience.SourceChanged):
		c.Kind, c.Summary = "file_changed", "The file changed while the job ran"
	case class == string(resilience.SourceUnreachable):
		c.Kind, c.Summary = "file_missing", "The file could not be found or opened at its path"
	case reachFailures[class]:
		c.Kind, c.Summary = "worker_unreachable", fmt.Sprintf("The worker or the NAS could not be reached (%s)", strings.ReplaceAll(class, "_", " "))
	case class == string(resilience.RunnerKilled):
		c.Kind, c.Summary = "encoder_stopped", "The encoder was stopped before it finished (worker restart or out of memory)"
	case class == string(resilience.EncoderTemporarilyUnavailable):
		c.Kind, c.Summary = "encoder_stopped", "The worker's encoder was busy or unavailable"
	case measureFailures[class] || strings.Contains(text, "quality_final_sample_misaligned"):
		c.Kind, c.Summary = "check_failed", "The quality measurement of the new file failed"
	case class == string(resilience.CandidateInvalid) || strings.HasPrefix(class, "validation_"):
		c.Kind, c.Summary = "check_failed", fmt.Sprintf("The new file failed its checks (%s)", strings.ReplaceAll(class, "_", " "))
	case fixFirstFailures[class] != "":
		c.Summary = fixFirstFailures[class]
	default:
		c.Summary = firstLine(text)
		if c.Summary == "" {
			c.Summary = "The job failed without an error message"
		}
	}
	return c
}

// summaryLimit is the longest line a cause or an explanation says.
const summaryLimit = 140

// firstLine is the first line of a text, at most summaryLimit characters.
func firstLine(text string) string {
	text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > summaryLimit {
		text = string(r[:summaryLimit-1]) + "…"
	}
	return text
}

// resultCause reads a job that completed without a new file: its reason, the
// sizes, and its ladder or budget record.
func resultCause(r operationRecord, streams *action.ConversionStreams) *conversionCause {
	reason := operationString(operationValue(r, "reason_code"))
	original := int64(operationNumber(operationMap(operationValue(r, "original"))["size_bytes"]))
	if original <= 0 {
		original = int64(operationNumber(operationValue(r, "original_size")))
	}
	candidate := causeCandidateBytes(r)
	minimum := operationNumber(operationMap(operationValue(r, "mode_policy"))["min_savings_percent"])
	c := &conversionCause{Kind: "other"}
	switch reason {
	case "insufficient_savings", "size_growth", "size_growth_exceeded":
		estimated := false
		if candidate <= 0 {
			if winner := int64(operationNumber(operationMap(operationMap(operationValue(r, "benchmark_decision"))["winner"])["estimated_bytes"])); winner > 0 {
				candidate, estimated = winner, true
			}
		}
		if original <= 0 || candidate <= 0 {
			c.Kind, c.Summary = "below_min_savings", "The new file would not be smaller enough to keep"
			if minimum > 0 {
				c.Summary = fmt.Sprintf("The new file would not be at least %.0f%% smaller", minimum)
			}
			if reason != "insufficient_savings" {
				c.Kind, c.Summary = "not_smaller", "The new file would not be smaller than the original"
			}
			break
		}
		what := "New file"
		if estimated {
			what = "Estimated new file"
		}
		saved := 100 - percentOf(candidate, original)
		if saved <= 0 || reason != "insufficient_savings" {
			c.Kind = "not_smaller"
			c.Summary = fmt.Sprintf("%s %s vs original %s: not smaller", what, formatSize(candidate), formatSize(original))
			c.numbers(float64(candidate), float64(original), "bytes")
			break
		}
		c.Kind = "below_min_savings"
		c.Summary = fmt.Sprintf("%s %s vs original %s: %.0f%% smaller, below the %.0f%% minimum", what, formatSize(candidate), formatSize(original), saved, minimum)
		c.numbers(saved, minimum, "percent")
	case "size_budget_exceeded":
		target := int64(operationNumber(operationMap(operationValue(r, "size_budget"))["budget_bytes"]))
		c.Kind = "over_size_target"
		switch {
		case candidate > 0 && target > 0 && original > 0:
			c.Summary = fmt.Sprintf("New file %s (%.0f%% of the %s original), over the %s target (%.0f%%)", formatSize(candidate), percentOf(candidate, original), formatSize(original), formatSize(target), percentOf(target, original))
			c.numbers(float64(candidate), float64(target), "bytes")
		case candidate > 0 && target > 0:
			c.Summary = fmt.Sprintf("New file %s, over the %s target", formatSize(candidate), formatSize(target))
			c.numbers(float64(candidate), float64(target), "bytes")
		default:
			c.Summary = "The new file did not fit the target size"
		}
	case "quality_not_met", "no_suitable_candidate", "search_budget_exhausted":
		c = qualityCause(r)
	case "audio_only_nothing_to_convert":
		c.Kind, c.Summary = "nothing_to_do", "Every audio track is already compact and none is removed: nothing to convert"
	case "compact_not_needed":
		c.Kind, c.Summary = "nothing_to_do", "The file is already 1080p SDR or smaller: nothing to shrink"
	case "hdr_requires_compact":
		c.Summary = "HDR or Dolby Vision source: Save space keeps the picture as it is and cannot encode it"
	default:
		if operationValue(r, "skip_transcode") == true {
			c.Kind, c.Summary = "nothing_to_do", "The job skipped encoding"
			if reason != "" {
				c.Summary += " (" + strings.ReplaceAll(reason, "_", " ") + ")"
			}
		} else if candidate > 0 && operationValue(r, "original_intact") != true {
			c.Summary = "The original could not be confirmed intact, so the new file is not offered"
		} else {
			c.Summary = "The job ended without a new file"
		}
	}
	blameStream(c, streams, candidate, original)
	return c
}

// causeCandidateBytes is the new file's size, else the last setting tried's.
func causeCandidateBytes(r operationRecord) int64 {
	if b := int64(operationNumber(operationMap(operationValue(r, "result"))["size_bytes"])); b > 0 {
		return b
	}
	if b := int64(operationNumber(operationValue(r, "candidate_size_bytes"))); b > 0 {
		return b
	}
	if ladder := jobLadder(r); ladder != nil && len(ladder.Tried) > 0 {
		return ladder.Tried[len(ladder.Tried)-1].CandidateBytes
	}
	return 0
}

// blameStream names the stream behind a size miss, from the plan's estimate
// of what each stream of the new file weighs.
func blameStream(c *conversionCause, streams *action.ConversionStreams, candidate, original int64) {
	if streams == nil || candidate <= 0 {
		return
	}
	switch c.Kind {
	case "not_smaller", "below_min_savings", "over_size_target":
	default:
		return
	}
	audio, video := streams.Totals.AudioAfter, streams.Totals.VideoAfter
	if audio <= 0 || video <= 0 {
		return
	}
	var target int64
	if c.Kind == "over_size_target" && c.Limit != nil {
		target = int64(*c.Limit)
	}
	if audio > video {
		tail := ""
		switch c.Kind {
		case "over_size_target":
			tail = fmt.Sprintf("; the target was %s", formatSize(target))
			if video+streams.Totals.SubtitlesAfter <= target {
				tail = fmt.Sprintf("; the video alone (%s) met the %s target", formatSize(video), formatSize(target))
			}
		case "not_smaller":
			tail = fmt.Sprintf("; the original is %s", formatSize(original))
		case "below_min_savings":
			if c.Measured != nil && c.Limit != nil {
				tail = fmt.Sprintf("; %.0f%% smaller, below the %.0f%% minimum", *c.Measured, *c.Limit)
			}
		}
		c.Kind, c.Stream = "audio_too_big", "audio"
		c.Summary = fmt.Sprintf("Audio is %s of the %s result (%.0f%%)%s", formatSize(audio), formatSize(candidate), percentOf(audio, candidate), tail)
		return
	}
	c.Stream = "video"
	if c.Kind == "over_size_target" && target > 0 && video > target {
		c.Kind = "video_too_big"
		c.Summary = fmt.Sprintf("Video alone is %s, over the %s target (new file %s)", formatSize(video), formatSize(target), formatSize(candidate))
		return
	}
	c.Summary += fmt.Sprintf("; video is %s (%.0f%%) of it", formatSize(video), percentOf(video, candidate))
}

// jobLadder is the job's quality ladder record, nil without one.
func jobLadder(r operationRecord) *action.QualityLadderRecord {
	return action.QualityLadderOf(map[string]any{"quality_ladder": operationValue(r, "quality_ladder")})
}

// ladderSetting names one rung: "CRF 22 · medium", "Quality 60".
func ladderSetting(ladder *action.QualityLadderRecord, q int) string {
	name := "CRF"
	if ladder.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
		name = "Quality"
	}
	setting := fmt.Sprintf("%s %d", name, q)
	if ladder.Preset != "" {
		setting += " · " + ladder.Preset
	}
	return setting
}

// qualityCause says which quality limit the last setting tried missed, with
// its score and the limit.
func qualityCause(r operationRecord) *conversionCause {
	c := &conversionCause{Kind: "quality_below_target"}
	var final *action.FinalQualitySummary
	at := ""
	if ladder := jobLadder(r); ladder != nil && len(ladder.Tried) > 0 {
		last := ladder.Tried[len(ladder.Tried)-1]
		final = last.Final
		at = " at " + ladderSetting(ladder, last.Quality)
		if n := len(ladder.Tried); n > 1 {
			at += fmt.Sprintf(", the last of %d settings tried", n)
		}
	} else if budget := operationMap(operationValue(r, "size_budget")); operationMap(budget["final"]) != nil {
		var f action.FinalQualitySummary
		if reencodeJSON(budget["final"], &f) {
			final = &f
			if final.Limits == nil {
				target, floor := operationNumber(budget["quality_target"]), operationNumber(budget["quality_floor"])
				final.Limits = &action.QualityLimits{}
				if target > 0 {
					final.Limits.VMAFMean = &target
				}
				if floor > 0 {
					final.Limits.VMAFWorstWindow = &floor
				}
			}
			if kbps := operationNumber(budget["video_bitrate_kbps"]); kbps > 0 {
				at = fmt.Sprintf(" at %.0f kbps", kbps)
			}
		}
	}
	if final == nil {
		// A job from before the ladder: its full-file scores, without limits.
		evidence := operationMap(operationValue(r, "quality_evidence"))
		vmaf := operationMap(evidence["vmaf"])
		f := action.FinalQualitySummary{}
		if v, ok := vmaf["mean"].(float64); ok {
			f.Measured.VMAFMean = &v
		}
		if v, ok := vmaf["worst_window_mean"].(float64); ok {
			f.Measured.VMAFWorstWindow = &v
		}
		codes, _ := evidence["reason_codes"].([]any)
		for _, code := range codes {
			f.FailedChecks = append(f.FailedChecks, operationString(code))
		}
		final = &f
	}
	m, l := final.Measured, final.Limits
	if l == nil {
		l = &action.QualityLimits{}
	}
	failed := func(codes ...string) bool {
		for _, code := range codes {
			if slices.Contains(final.FailedChecks, code) {
				return true
			}
		}
		return false
	}
	meanMissed := failed("below_minimum_quality", "sample_below_minimum_quality") || m.VMAFMean != nil && l.VMAFMean != nil && *m.VMAFMean < *l.VMAFMean
	worstMissed := failed("quality_vmaf_worst_window_below_minimum") || m.VMAFWorstWindow != nil && l.VMAFWorstWindow != nil && *m.VMAFWorstWindow < *l.VMAFWorstWindow
	switch {
	case worstMissed && !meanMissed && m.VMAFWorstWindow != nil:
		c.Kind = "worst_window_below_floor"
		window := "Worst window"
		if l.WorstWindowSeconds > 0 {
			window = fmt.Sprintf("Worst %.0f-second window", l.WorstWindowSeconds)
		}
		if l.VMAFWorstWindow != nil {
			c.Summary = fmt.Sprintf("%s %.1f VMAF%s, below the %.1f floor", window, *m.VMAFWorstWindow, at, *l.VMAFWorstWindow)
			c.numbers(*m.VMAFWorstWindow, *l.VMAFWorstWindow, "vmaf")
		} else {
			c.Summary = fmt.Sprintf("%s %.1f VMAF%s, below this goal's floor", window, *m.VMAFWorstWindow, at)
			c.Measured, c.Unit = m.VMAFWorstWindow, "vmaf"
		}
	case m.VMAFMean != nil && (meanMissed || !failed("quality_vmaf_p5_below_minimum")):
		if l.VMAFMean != nil {
			c.Summary = fmt.Sprintf("Quality %.1f VMAF%s, below the %.1f target", *m.VMAFMean, at, *l.VMAFMean)
			if !meanMissed && worstMissed && m.VMAFWorstWindow != nil && l.VMAFWorstWindow != nil {
				c.Summary = fmt.Sprintf("Quality %.1f VMAF%s; worst window %.1f, below the %.1f floor", *m.VMAFMean, at, *m.VMAFWorstWindow, *l.VMAFWorstWindow)
			}
			c.numbers(*m.VMAFMean, *l.VMAFMean, "vmaf")
		} else {
			c.Summary = fmt.Sprintf("Quality %.1f VMAF%s, below this goal's minimum", *m.VMAFMean, at)
			c.Measured, c.Unit = m.VMAFMean, "vmaf"
		}
	case m.VMAFP5 != nil && l.VMAFP5 != nil:
		c.Summary = fmt.Sprintf("VMAF p5 %.1f%s, below the %.1f minimum", *m.VMAFP5, at, *l.VMAFP5)
		c.numbers(*m.VMAFP5, *l.VMAFP5, "vmaf")
	case len(final.Failures) > 0:
		c.Summary = strings.Join(final.Failures, "; ") + at
	default:
		c.Summary = "The new file missed this goal's quality limits" + at + "; no scores were reported"
	}
	return c
}

// causeAttempts is each setting the job encoded and what it measured: the
// rungs of its quality ladder, or its size budget's one encode.
func causeAttempts(r operationRecord) []conversionTry {
	if ladder := jobLadder(r); ladder != nil {
		var out []conversionTry
		for _, t := range ladder.Tried {
			try := conversionTry{Setting: ladderSetting(ladder, t.Quality)}
			if t.CandidateBytes > 0 {
				b := t.CandidateBytes
				try.Bytes = &b
			}
			if t.Final != nil {
				try.VMAFMean, try.VMAFWorst = t.Final.Measured.VMAFMean, t.Final.Measured.VMAFWorstWindow
			}
			out = append(out, try)
		}
		if ladder.Final != nil && ladder.Final.Verdict == "pass" {
			rung := min(max(ladder.Rung, 0), len(ladder.Values)-1)
			try := conversionTry{Setting: ladderSetting(ladder, ladder.Values[rung]), VMAFMean: ladder.Final.Measured.VMAFMean, VMAFWorst: ladder.Final.Measured.VMAFWorstWindow, Passed: true}
			if b := causeCandidateBytes(r); b > 0 && r.inst.Status == store.ActionStatusCompleted && operationValue(r, "skip_transcode") != true {
				try.Bytes = &b
			}
			out = append(out, try)
		}
		return out
	}
	budget := operationMap(operationValue(r, "size_budget"))
	if kbps := operationNumber(budget["video_bitrate_kbps"]); kbps > 0 && (budget["final"] != nil || operationNumber(budget["candidate_bytes"]) > 0) {
		try := conversionTry{Setting: fmt.Sprintf("%.0f kbps", kbps)}
		if b := int64(operationNumber(budget["candidate_bytes"])); b > 0 {
			try.Bytes = &b
		}
		final := operationMap(budget["final"])
		measured := operationMap(final["measured"])
		if v, ok := measured["vmaf_mean"].(float64); ok {
			try.VMAFMean = &v
		}
		if v, ok := measured["vmaf_worst_window"].(float64); ok {
			try.VMAFWorst = &v
		}
		try.Passed = operationString(final["verdict"]) == "pass"
		return []conversionTry{try}
	}
	return nil
}

func reencodeJSON(from, to any) bool {
	b, err := json.Marshal(from)
	return err == nil && json.Unmarshal(b, to) == nil
}
