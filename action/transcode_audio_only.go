package action

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// audioOnlyEstimate is the cheap pre-screen for a size v3 audio-only plan:
// the source size minus what compacting its lossless tracks would save,
// from stream bitrates and Matroska statistics. A track whose size is
// unknown saves nothing here, so an unknown never passes the gate.
type audioOnlyEstimate struct {
	CandidateBytes int64
	Compacted      int
	Unknown        int
}

func estimateAudioOnly(rep *mediainspect.DetailedReport, sourceBytes int64) audioOnlyEstimate {
	var e audioOnlyEstimate
	if rep == nil || sourceBytes <= 0 || rep.DurationSec <= 0 {
		return e
	}
	var saved int64
	for _, a := range rep.Audio {
		target, kbps := transcode.AudioTarget(transcode.AudioModeCompactLossless, transcode.DetailedAudioTrack(a.Codec, a.Profile, a.Title, a.Tags, a.Channels, a.ChannelLayout))
		if target == "copy" {
			continue
		}
		e.Compacted++
		source := audioStreamBytes(a, rep.DurationSec, sourceBytes)
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
// it to do.
func (e *Engine) sizeAudioOnlyFallback(ctx context.Context, ec *ExecutionContext, outputs map[string]any) (StepResult, bool) {
	if ec.ActionName != "transcode_media" || getString(ec.Inputs, "mode") != "size" {
		return StepResult{}, false
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Mode != "size" || snapshot.workerVersion() != transcode.ModePolicyV3 || snapshot.Size == nil || !snapshot.Size.AudioOnlyFallback {
		return StepResult{}, false
	}
	rep := getSourceReport(ec.State["source_report"])
	original := getInt64(ec.State, "original_size")
	estimate := estimateAudioOnly(rep, original)
	if estimate.Compacted == 0 {
		return StepResult{}, false
	}
	skip := func(reason string) (StepResult, bool) {
		ec.State["skip_transcode"] = true
		ec.State["reason_code"] = reason
		outputs["skipped"] = true
		outputs["reason_code"] = reason
		return StepResult{Status: StepCompleted, Outputs: outputs}, true
	}
	outputs["audio_only_estimated_bytes"] = estimate.CandidateBytes
	ec.State["audio_only_estimated_bytes"] = estimate.CandidateBytes
	if !transcode.ModeSizeAllowedVersion(snapshot.Mode, original, estimate.CandidateBytes, snapshot.workerVersion()) {
		if estimate.Unknown > 0 {
			return skip("audio_estimate_unavailable")
		}
		return skip("audio_compaction_insufficient")
	}
	video := rep.MainVideo()
	if len(video) != 1 {
		return skip("unsupported_source")
	}
	if !e.workersConfigured() {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, true
	}
	caps, err := e.actionWorker(ec).Capabilities(ctx)
	if err != nil {
		return workerCapabilityLookupResult(err, "size audio-only"), true
	}
	if !caps.SizeAudioOnly {
		return skip("audio_only_worker_unsupported")
	}
	plan, err := transcode.NewSizeAudioOnlyPlan(getPlan(ec.State["plan"]), video[0].BitDepth)
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
	outputs["plan_note"] = "No video encode met the quality floor and the savings gate; the video is copied unchanged and only lossless audio is compacted."
	return StepResult{Status: StepCompleted, Outputs: outputs}, true
}
