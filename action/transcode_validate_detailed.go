package action

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// This file holds the full structural/media validation used only where a full
// inspection is legitimately required (candidate promotion). It is
// intentionally separated from action/transcode_validate.go so the normal
// transcode pipeline's post-publish step never performs heavy NAS inspection:
// the worker validates the local candidate BEFORE publish, and the coordinator
// only checks cheap independent identity facts afterwards.

// validateCandidateDetailed performs the historical full candidate validation
// (ffprobe + detailed stream/policy checks) against an already-published
// candidate. It returns the same StepResult contract (completed / failed /
// waiting-decision) as the original validator so promotion can reuse it.
func (e *Engine) validateCandidateDetailed(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	acceptValidationLoss := false
	if ec.Decision != "" {
		if strings.EqualFold(ec.Decision, "reject") || strings.EqualFold(ec.Decision, "cancel") {
			return rejectedCandidateResult(), nil
		}
		acceptValidationLoss = strings.EqualFold(ec.Decision, "accept_loss") || strings.EqualFold(ec.Decision, "approve")
	}
	outputPath := getString(ec.State, "candidate_path")
	if outputPath == "" {
		outputPath = getString(ec.State, "output_path")
	}
	if outputPath == "" {
		return StepResult{Status: StepFailed, Error: "candidate path is missing (fail closed)"}, nil
	}
	fi, err := os.Stat(outputPath)
	if err != nil || fi.Size() == 0 {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("transcoded candidate file %q not accessible or has 0 bytes: %v", outputPath, err)}, nil
	}
	outRep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, outputPath)
	if err != nil || !outRep.Probed {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("ffprobe failed to inspect candidate file %q: %v", outputPath, err)}, nil
	}
	origMap, _ := ec.State["original"].(map[string]any)
	plan := getPlan(ec.State["plan"])
	if plan == nil {
		return StepResult{Status: StepFailed, Error: "resolved plan missing during validation"}, nil
	}
	waitDecision := func(reason string) StepResult {
		return StepResult{Status: StepWaitingDecision, WaitingReason: reason, WaitingOptions: []WaitingOption{{Decision: "reject", Description: "Reject candidate and keep original"}, {Decision: "accept_loss", Description: "Accept candidate despite validation discrepancy"}}}
	}
	outVideo := outRep.MainVideo()
	if len(outVideo) == 0 {
		return StepResult{Status: StepFailed, Error: "transcoded candidate contains no video streams"}, nil
	}
	// Programme video count is a hard invariant: an encoder that turned cover
	// art into a second video track must not be publishable via accept_loss.
	origAllVideo := getStreamsList(origMap, "video")
	origVideo := mediainspect.MainVideoStreams(origAllVideo)
	if len(origVideo) > 0 && len(outVideo) != len(origVideo) {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Video stream count mismatch: original=%d output=%d programme video streams (fail closed)", len(origVideo), len(outVideo))}, nil
	}
	if plan.ExpectedBitDepth > 0 && outVideo[0].BitDepth != plan.ExpectedBitDepth {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Video bit depth mismatch: plan requires %d-bit output but candidate is %d-bit (fail closed)", plan.ExpectedBitDepth, outVideo[0].BitDepth)}, nil
	}
	// The tracks the plan keeps are the output's audio and subtitles, exactly:
	// no more and no fewer. That is a hard invariant like the video count, not
	// a discrepancy anyone may accept.
	origAudio, origSubs := getStreamsList(origMap, "audio"), getStreamsList(origMap, "subtitles")
	sourceSubtitles := origSubs
	origAudio, origSubs = keptStreams(origAudio, origSubs, plan.Tracks)
	if plan.Tracks != nil && (len(outRep.Audio) != len(origAudio) || len(outRep.Subtitles) != len(origSubs)) {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Track selection mismatch: the plan keeps %d audio and %d subtitle tracks, the output has %d and %d (fail closed)", len(origAudio), len(origSubs), len(outRep.Audio), len(outRep.Subtitles))}, nil
	}
	if acceptValidationLoss {
		ec.State["validation_decision_applied"] = ec.Decision
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"decision_applied": ec.Decision, "note": "validation discrepancy accepted by user decision after hard candidate invariants passed"}}, nil
	}
	origDur := getFloat(origMap, "duration_sec")
	if origDur > 0 && outRep.DurationSec > 0 {
		d := math.Abs(outRep.DurationSec - origDur)
		if d > 3 && (d/origDur) > 0.02 {
			return waitDecision(fmt.Sprintf("Duration discrepancy: original was %.1fs, output is %.1fs (difference: %.1fs)", origDur, outRep.DurationSec, d)), nil
		}
	}
	// Explicit expectations may use the same encoder names as recipe.video.codec.
	// Normalize them just like the plan: ffprobe reports hevc, not libx265.
	expected := expectedVideoCodec(getString(ec.Inputs, "expected_video_codec"))
	if expected == "" {
		expected = expectedVideoCodec(plan.VideoCodec)
		// A copied video stream keeps the source codec.
		if transcode.IsSizeAudioOnlyPlan(plan) {
			expected = normCodec(getString(origMap, "video_codec"))
		}
	}
	if expected != "" && !strings.Contains(strings.ToLower(outVideo[0].Codec), expected) {
		return waitDecision(fmt.Sprintf("Video codec mismatch: expected %s, transcoded output is %s", expected, outVideo[0].Codec)), nil
	}
	if len(origVideo) > 0 && origVideo[0].Width > 0 && origVideo[0].Height > 0 {
		if outVideo[0].Width != origVideo[0].Width || outVideo[0].Height != origVideo[0].Height {
			return waitDecision(fmt.Sprintf("Video resolution mismatch: original=%dx%d output=%dx%d", origVideo[0].Width, origVideo[0].Height, outVideo[0].Width, outVideo[0].Height)), nil
		}
	}
	if origCovers, outCovers := len(mediainspect.CoverArtStreams(origAllVideo)), len(outRep.CoverArt()); outCovers != origCovers {
		return waitDecision(fmt.Sprintf("Cover art count mismatch: original=%d output=%d", origCovers, outCovers)), nil
	}
	if len(outRep.Audio) != len(origAudio) {
		missing := make([]string, 0)
		for _, a := range origAudio {
			found := false
			for _, b := range outRep.Audio {
				if a.Language != "" && normCodec(a.Language) == normCodec(b.Language) {
					found = true
					break
				}
			}
			if !found {
				label := a.Language
				if label == "" {
					label = a.Codec
				}
				missing = append(missing, label)
			}
		}
		return waitDecision(fmt.Sprintf("Audio stream lost: original=%d output=%d missing=%s", len(origAudio), len(outRep.Audio), strings.Join(missing, ","))), nil
	}
	// A converted track's bitrate is measured from the candidate's packets,
	// as the worker's pre-publish check measured it, and only once. A scan
	// that fails refuses the candidate; audio_bitrate says "packets" once one
	// succeeded.
	audioBitrate := "not_applicable"
	var packets map[int]mediainspect.AudioPacketSpan
	measured := false
	for i, a := range origAudio {
		b := outRep.Audio[i]
		out := transcode.AudioOutputFor(plan.AudioMode, audioTrackOf(a, origDur))
		target := out.Codec
		// Only an AAC conversion can remap the layout; a copied track (PCM in
		// Matroska reports none) is checked by codec and channels.
		checkLayout := target != "copy"
		if target == "copy" {
			target = normCodec(a.Codec)
		}
		if target != normCodec(b.Codec) {
			return waitDecision(fmt.Sprintf("Audio codec mismatch at stream %d: original=%s output=%s", i, a.Codec, b.Codec)), nil
		}
		if transcode.IsAudioLevelMode(plan.AudioMode) && checkLayout {
			// A converted track must be the profile of its level at about its
			// planned bitrate. The encoder is variable bitrate, so only gross
			// overshoot is refused, as the worker's packet check does.
			wantProfile := "LC"
			if out.HEAAC {
				wantProfile = "HE-AAC"
			}
			if !strings.EqualFold(b.Profile, wantProfile) {
				return waitDecision(fmt.Sprintf("Audio profile mismatch at stream %d: expected %s, output is %q", i, wantProfile, b.Profile)), nil
			}
			// ffprobe's per-stream bit_rate is no evidence here: Matroska
			// stores none for AAC, so ffprobe's figure comes from the
			// statistics tags (BPS, NUMBER_OF_BYTES), which may be the source
			// track's, copied over with the metadata. The packets are what
			// was encoded.
			if !measured {
				measured = true
				var err error
				packets, err = mediainspect.MeasureAudioPackets(ctx, e.deps.Ffprobe, outputPath)
				if _, probeFailed := mediainspect.AsProbeError(err); err != nil && !probeFailed {
					return StepResult{}, err
				}
				if err != nil {
					// Fail closed: the worker measured these same bytes with
					// the same parser, so an unreadable scan is a real problem
					// (or a stalled read, which a retry clears), never a pass.
					return waitDecision(fmt.Sprintf("Audio bitrate not measured at stream %d: %v", i, err)), nil
				}
				audioBitrate = "packets"
			}
			span, ok := packets[b.Index]
			if !ok {
				return waitDecision(fmt.Sprintf("Audio stream %d has no measurable packets", i)), nil
			}
			// The worker's threshold: a short track's padding is not a rate.
			if bps := span.BitsPerSecond(); span.Seconds() > minMeasuredAudioSeconds && bps > float64(transcode.AudioBitrateCeilingBps(out.Kbps)) {
				return waitDecision(fmt.Sprintf("Audio bitrate mismatch at stream %d: planned %d kbps, output is %d kbps", i, out.Kbps, int64(bps)/1000)), nil
			}
		}
		if a.Language != "" && normCodec(a.Language) != normCodec(b.Language) {
			return waitDecision(fmt.Sprintf("Audio language mismatch at stream %d: original=%s output=%s", i, a.Language, b.Language)), nil
		}
		// Under a selection the default flag is part of the plan: a kept track
		// the source marked default, or the first kept track when the default
		// was dropped, must be default in the output.
		if plan.Tracks != nil && a.Disposition["default"] > 0 && b.Disposition["default"] == 0 {
			return waitDecision(fmt.Sprintf("Default audio disposition lost at stream %d", i)), nil
		}
		if a.Channels > 0 && b.Channels != a.Channels {
			return waitDecision(fmt.Sprintf("Audio channel mismatch at stream %d: original=%d output=%d", i, a.Channels, b.Channels)), nil
		}
		if checkLayout && !transcode.SameAudioLayout(a.ChannelLayout, b.ChannelLayout) {
			return waitDecision(fmt.Sprintf("Audio layout mismatch at stream %d: original=%s output=%s", i, a.ChannelLayout, b.ChannelLayout)), nil
		}
	}
	if len(outRep.Subtitles) != len(origSubs) {
		return waitDecision(fmt.Sprintf("Subtitle stream lost: original=%d output=%d", len(origSubs), len(outRep.Subtitles))), nil
	}
	actions := map[int]transcode.SubtitleAction{}
	for _, a := range plan.SubtitleActions {
		actions[a.TypeIndex] = a
	}
	// An action belongs to the subtitle's position among all of the source's
	// subtitles, which is not its position in the output once some are dropped.
	position := map[int]int{}
	for i, s := range sourceSubtitles {
		position[s.Index] = i
	}
	for i, a := range origSubs {
		b := outRep.Subtitles[i]
		at := i
		if plan.Tracks != nil {
			at = position[a.Index]
		}
		act, ok := actions[at]
		if !ok {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("plan missing subtitle action %d during validation", i)}, nil
		}
		if act.Operation == "copy" && normCodec(a.Codec) != normCodec(b.Codec) {
			return waitDecision(fmt.Sprintf("Copied subtitle codec changed at stream %d: original=%s output=%s", i, a.Codec, b.Codec)), nil
		}
		if act.Operation == "transcode" && normCodec(b.Codec) != normCodec(act.Codec) {
			return waitDecision(fmt.Sprintf("Converted subtitle codec mismatch at stream %d: expected=%s output=%s", i, act.Codec, b.Codec)), nil
		}
		if a.Language != "" && normCodec(a.Language) != normCodec(b.Language) {
			return waitDecision(fmt.Sprintf("Subtitle language mismatch at stream %d", i)), nil
		}
		for _, d := range []string{"forced", "default"} {
			if a.Disposition[d] > 0 && b.Disposition[d] == 0 {
				label := "Default"
				if d == "forced" {
					label = "Forced"
				}
				return waitDecision(fmt.Sprintf("%s subtitle disposition lost at stream %d", label, i)), nil
			}
		}
	}
	origAttachments := getStreamsList(origMap, "attachments")
	if plan.PreserveAttachments && len(outRep.Attachments) != len(origAttachments) {
		return waitDecision(fmt.Sprintf("Attachment count mismatch: original=%d output=%d", len(origAttachments), len(outRep.Attachments))), nil
	}
	origChapters := getInt(origMap, "chapters")
	if plan.PreserveChapters && outRep.Chapters != origChapters {
		return waitDecision(fmt.Sprintf("Chapter count mismatch: original=%d output=%d", origChapters, outRep.Chapters)), nil
	}
	origSize := getInt64(origMap, "size_bytes")
	saved := origSize - fi.Size()
	pct := float64(0)
	if origSize > 0 {
		pct = float64(saved) / float64(origSize) * 100
	}
	_, maxInc := e.effectiveSizeGuardrails(ec)
	if origSize > 0 && fi.Size() > origSize {
		increasePct := float64(fi.Size()-origSize) / float64(origSize) * 100
		if increasePct > maxInc {
			return waitDecision(fmt.Sprintf("Candidate file size (%d bytes) exceeds original (%d bytes) by %.1f%%, which is greater than max_size_increase_percent (%.1f%%)", fi.Size(), origSize, increasePct, maxInc)), nil
		}
	}
	result := map[string]any{
		"candidate_path": outputPath, "output_path": outputPath, "size_bytes": fi.Size(), "duration_sec": outRep.DurationSec,
		"video_codec": outVideo[0].Codec, "resolution": fmt.Sprintf("%dx%d", outVideo[0].Width, outVideo[0].Height), "bit_depth": outVideo[0].BitDepth,
		"profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest, "plan_digest": plan.PlanDigest,
		"video_profile": plan.VideoProfile, "pixel_format": plan.PixelFormat, "prioritize_speed": plan.PrioritizeSpeed, "spatial_aq": plan.SpatialAQ, "realtime": plan.Realtime,
		"average_bitrate_kbps": plan.AverageBitrateKbps, "max_bitrate_kbps": plan.MaxBitrateKbps, "constant_bitrate": plan.ConstantBitrate,
		"qmin": plan.QMin, "qmax": plan.QMax, "gop_size": plan.GOPSize, "b_frames": plan.BFrames, "closed_gop": plan.ClosedGOP,
		"power_efficient": plan.PowerEfficient, "max_ref_frames": plan.MaxRefFrames, "expected_bit_depth": plan.ExpectedBitDepth,
		"attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count"), "applied_fallbacks": plan.AppliedFallbacks,
	}
	if c := ec.State["conversions"]; c != nil {
		result["conversions"] = c
	}
	addTrackReport(result, ec.State)
	bitDepthStatus := "not_requested"
	if plan.ExpectedBitDepth > 0 {
		bitDepthStatus = "ok"
	}
	validation := map[string]any{"duration": "ok", "video_streams": "ok", "resolution": "ok", "bit_depth": bitDepthStatus, "audio_streams": "ok", "audio_bitrate": audioBitrate, "subtitle_streams": "ok", "attachments": "ok", "chapters": "ok", "original_sha256_pending": "accept_result"}
	ec.State["validation"] = validation
	ec.State["result"] = result
	ec.State["size_saved_bytes"] = saved
	ec.State["size_saved_percent"] = pct
	outputs := map[string]any{"result": result, "validation": validation, "candidate_path": outputPath, "output_path": outputPath, "size_saved_bytes": saved, "size_saved_percent": pct, "profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest}
	addTrackReport(outputs, ec.State)
	return StepResult{Status: StepCompleted, Outputs: outputs}, nil
}

// minMeasuredAudioSeconds is the shortest measured track whose bitrate is
// checked: the worker packet check's threshold.
const minMeasuredAudioSeconds = 10

func expectedVideoCodec(codec string) string {
	switch normCodec(codec) {
	case "hevc_videotoolbox", "libx265":
		return "hevc"
	case "h264_videotoolbox":
		return "h264"
	default:
		return normCodec(codec)
	}
}

func normCodec(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
