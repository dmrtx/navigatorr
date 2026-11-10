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
	// A preview-image track is never the output's programme video. When the
	// source's programme is real video, an output carrying one has either lost
	// that video (MainVideo falls back to the track when it is all there is) or
	// kept a track the encode leaves out; neither is for accept_loss to publish.
	if len(origVideo) > 0 && !isPreviewImageTrack(origVideo[0]) {
		for _, v := range outRep.Video {
			if !mediainspect.IsAttachedPicture(v) && isPreviewImageTrack(v) {
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("Candidate carries a preview image track (stream %d, %s) where the source's programme video should be (fail closed)", v.Index, v.Codec)}, nil
			}
		}
	}
	if plan.ExpectedBitDepth > 0 && outVideo[0].BitDepth != plan.ExpectedBitDepth {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Video bit depth mismatch: plan requires %d-bit output but candidate is %d-bit (fail closed)", plan.ExpectedBitDepth, outVideo[0].BitDepth)}, nil
	}
	// The tracks the plan keeps are the output's audio and subtitles, exactly:
	// no more and no fewer. That is a hard invariant like the video count, not
	// a discrepancy anyone may accept.
	origAudio, origSubs := getStreamsList(origMap, "audio"), getStreamsList(origMap, "subtitles")
	sourceAudio, sourceSubtitles := origAudio, origSubs
	origAudio, origSubs = keptStreams(origAudio, origSubs, plan.Tracks)
	// ffmpeg marks a track default on its own only when the worker's command
	// set no track's flags; the command set them as the plan says
	// (transcode.WrittenDisposition), which is read the same way here.
	written := dispositionsWritten("audio", sourceAudio, origAudio) || dispositionsWritten("subtitle", sourceSubtitles, origSubs)
	if plan.Tracks != nil && (len(outRep.Audio) != len(origAudio) || len(outRep.Subtitles) != len(origSubs)) {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("Track selection mismatch: the plan keeps %d audio and %d subtitle tracks, the output has %d and %d (fail closed)", len(origAudio), len(origSubs), len(outRep.Audio), len(outRep.Subtitles))}, nil
	}
	if acceptValidationLoss {
		ec.State["validation_decision_applied"] = ec.Decision
		outputs := map[string]any{"decision_applied": ec.Decision, "note": "validation discrepancy accepted by user decision after hard candidate invariants passed"}
		addTrackReport(outputs, ec.State)
		return StepResult{Status: StepCompleted, Outputs: outputs}, nil
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
		wantWidth, wantHeight := origVideo[0].Width, origVideo[0].Height
		// Compact converts to its planned size on purpose, so the output is held
		// to that target and not to the source.
		if c := plan.Compact; c != nil && c.TargetWidth > 0 && c.TargetHeight > 0 {
			wantWidth, wantHeight = c.TargetWidth, c.TargetHeight
		}
		if outVideo[0].Width != wantWidth || outVideo[0].Height != wantHeight {
			return waitDecision(fmt.Sprintf("Video resolution mismatch: expected=%dx%d original=%dx%d output=%dx%d", wantWidth, wantHeight, origVideo[0].Width, origVideo[0].Height, outVideo[0].Width, outVideo[0].Height)), nil
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
	// Each kept track is measured from the candidate's packets, as the
	// worker's pre-publish check measured it, and only once: a converted
	// track's bitrate and every track's length come from the same scan. A
	// scan that fails refuses the candidate. audio_bitrate says "packets" once
	// a converted track's rate was checked; audio_duration says "ok" once the
	// kept tracks' lengths were compared with the source.
	audioBitrate, audioDuration := "not_applicable", "not_applicable"
	var packets map[int]mediainspect.AudioPacketSpan
	var scanErr error
	measured := false
	audioSpan := func(i, streamIndex int, what string) (span mediainspect.AudioPacketSpan, refusal *StepResult, err error) {
		if !measured {
			measured = true
			packets, scanErr = mediainspect.MeasureAudioPackets(ctx, e.deps.Ffprobe, outputPath)
		}
		if scanErr != nil {
			if _, probeFailed := mediainspect.AsProbeError(scanErr); !probeFailed {
				return span, nil, scanErr
			}
			// Fail closed: the worker measured these same bytes with the same
			// parser, so an unreadable scan is a real problem (or a stalled
			// read, which a retry clears), never a pass.
			r := waitDecision(fmt.Sprintf("Audio %s not measured at stream %d: %v", what, i, scanErr))
			return span, &r, nil
		}
		span, ok := packets[streamIndex]
		if !ok {
			r := waitDecision(fmt.Sprintf("Audio stream %d has no measurable packets", i))
			return span, &r, nil
		}
		if span.Timed == 0 {
			// Packets with no timestamps measure neither a rate nor a length.
			r := waitDecision(fmt.Sprintf("Audio %s not measured at stream %d: none of its %d packets has a timestamp", what, i, span.Packets))
			return span, &r, nil
		}
		return span, nil, nil
	}
	// The source's own tracks are read at most once per validation, and only
	// when a candidate track turns out shorter than the container.
	var ownSpans map[int]mediainspect.AudioPacketSpan
	var ownScanErr error
	ownMeasured := false
	sourceSpan := func(streamIndex int) (mediainspect.AudioPacketSpan, error) {
		if !ownMeasured {
			ownMeasured = true
			ownSpans, ownScanErr = e.sourceTrackSpans(ctx, origMap)
		}
		if ownScanErr != nil {
			return mediainspect.AudioPacketSpan{}, ownScanErr
		}
		span, ok := ownSpans[streamIndex]
		if !ok {
			return span, fmt.Errorf("original stream %d has no measurable packets", streamIndex)
		}
		if span.Timed == 0 {
			// Packets with no time say nothing of where the track ends: its
			// length is not measured, which is not a truncation.
			return span, fmt.Errorf("original stream %d has %d packets and none states a time", streamIndex, span.Packets)
		}
		return span, nil
	}
	plannedAudioDispositions := plannedDispositions(origAudio)
	for i, a := range origAudio {
		b := outRep.Audio[i]
		out := transcode.AudioOutputFor(plan.AudioMode, audioTrackOf(a, origDur))
		target := out.Codec
		// Only an AAC conversion can remap the layout; a copied track (PCM in
		// Matroska reports none) is checked by codec and channels.
		checkLayout := target != "copy"
		matches := target == normCodec(b.Codec)
		if target == "copy" {
			target, matches = normCodec(a.Codec), transcode.SameCopiedCodec(a.Codec, b.Codec)
		}
		if !matches {
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
			span, refused, err := audioSpan(i, b.Index, "bitrate")
			if err != nil {
				return StepResult{}, err
			}
			if refused != nil {
				return *refused, nil
			}
			audioBitrate = "packets"
			// The worker's threshold: a short track's padding is not a rate.
			if bps := span.BitsPerSecond(); span.Seconds() > minMeasuredAudioSeconds && bps > float64(transcode.AudioBitrateCeilingBps(out.Kbps)) {
				return waitDecision(fmt.Sprintf("Audio bitrate mismatch at stream %d: planned %d kbps, output is %d kbps", i, out.Kbps, int64(bps)/1000)), nil
			}
		}
		// A kept track must run as long as the source, by the container's own
		// tolerance. The output's container duration is the longest stream's,
		// so a truncated track hides behind the video; its packets cannot. A
		// source with no known duration has nothing to compare, as in the
		// container check above.
		if origDur > 0 {
			span, refused, err := audioSpan(i, b.Index, "duration")
			if err != nil {
				return StepResult{}, err
			}
			if refused != nil {
				return *refused, nil
			}
			if audioDuration != "matches_source_track" {
				audioDuration = "ok"
			}
			if short := origDur - span.Seconds(); short > 3 && short/origDur > 0.02 {
				// Short of the container is only truncation when the source
				// track itself was longer: releases exist whose audio stops
				// minutes before the video, and a faithful copy or re-encode
				// of such a track is as short as it. The source track's own
				// packets decide, read only now that the candidate is short.
				own, ownErr := sourceSpan(a.Index)
				if ownErr != nil {
					// Nothing proves the candidate truncated when the source
					// track could not be measured (a cancelled or stalled read,
					// no packets, no path): it is refused as not measured.
					if ctx.Err() != nil {
						return StepResult{}, ctx.Err()
					}
					return waitDecision(fmt.Sprintf("Audio source track not measured at stream %d (the candidate's is %.1fs short of the container): %v", i, short, ownErr)), nil
				}
				if own.Seconds() <= 0 || span.Seconds() < own.Seconds()-sourceTrackTolerance {
					return waitDecision(fmt.Sprintf("Audio truncated at stream %d: original runs %.1fs, the track's packets cover %.1fs (%.1fs short); the source track itself covers %.1fs", i, origDur, span.Seconds(), short, own.Seconds())), nil
				}
				// The candidate must also cover the same stretch of the timeline:
				// a track that starts minutes late is as long as the source and
				// still wrong. The candidate's container is normalised to zero,
				// so its times are compared as they are with a source track that
				// starts at the origin; a source whose tracks start later in its
				// own timeline (transport streams) has no common origin to
				// compare, and only its length is checked.
				if own.Start <= sourceTrackTolerance && math.Abs(span.Start-own.Start) > sourceTrackTolerance {
					return waitDecision(fmt.Sprintf("Audio out of place at stream %d: the source track runs %.1fs-%.1fs but the candidate's runs %.1fs-%.1fs", i, own.Start, own.End, span.Start, span.End)), nil
				}
				audioDuration = "matches_source_track"
				ec.State["audio_shorter_than_container"] = append(stateStrings(ec.State["audio_shorter_than_container"]), fmt.Sprintf("stream %d: the source track covers %.1fs of the container's %.1fs; the candidate's %.1fs matches it", i, own.Seconds(), origDur, span.Seconds()))
			}
		}
		if !sameLanguage(a.Language, b.Language) {
			return waitDecision(fmt.Sprintf("Audio language mismatch at stream %d: original=%s output=%s", i, a.Language, b.Language)), nil
		}
		// Under a selection the default flag is part of the plan: a kept track
		// the source marked default, or the first kept track when the default
		// was dropped, must be default in the output.
		if plan.Tracks != nil && a.Disposition["default"] > 0 && b.Disposition["default"] == 0 {
			return waitDecision(fmt.Sprintf("Default audio disposition lost at stream %d", i)), nil
		}
		// A copied track is the source's track: its name and flags too. A
		// converted one keeps the flags and carries the name the conversion
		// gives it (transcode.ConvertedAudioTitle), as the worker checked.
		how, want := "Copied", a.Title
		if out.Codec != "copy" {
			how, want = "Converted", transcode.ConvertedAudioTitle(a.Title, a.ChannelLayout, a.Channels)
		}
		if b.Title != want {
			return waitDecision(fmt.Sprintf("%s audio track %d was renamed: the plan calls it %q, the output %q", how, i, want, b.Title)), nil
		}
		if d := transcode.DispositionChange(a.Disposition, b.Disposition, transcode.InferredDefault(i, plannedAudioDispositions, written)); d != "" {
			return waitDecision(fmt.Sprintf("%s audio track %d changed its %s flag", how, i, d)), nil
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
	plannedSubDispositions := plannedDispositions(origSubs)
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
		if act.Operation == "copy" && !transcode.SameCopiedCodec(a.Codec, b.Codec) {
			return waitDecision(fmt.Sprintf("Copied subtitle codec changed at stream %d: original=%s output=%s", i, a.Codec, b.Codec)), nil
		}
		if act.Operation == "transcode" && !transcode.SameConvertedSubtitleCodec(act.Codec, b.Codec) {
			return waitDecision(fmt.Sprintf("Converted subtitle codec mismatch at stream %d: expected=%s output=%s", i, act.Codec, b.Codec)), nil
		}
		if !sameLanguage(a.Language, b.Language) {
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
		// ffmpeg carries a subtitle's name and flags over whether it copies
		// or converts it, and the command names no subtitle: a track keeps
		// both, save ffmpeg's own default (transcode.InferredDefault).
		how := "Copied"
		if act.Operation != "copy" {
			how = "Converted"
		}
		if a.Title != b.Title {
			return waitDecision(fmt.Sprintf("%s subtitle track %d was renamed: the source calls it %q, the output %q", how, i, a.Title, b.Title)), nil
		}
		if d := transcode.DispositionChange(a.Disposition, b.Disposition, transcode.InferredDefault(i, plannedSubDispositions, written)); d != "" {
			return waitDecision(fmt.Sprintf("%s subtitle track %d changed its %s flag", how, i, d)), nil
		}
	}
	if plan.PreserveAttachments {
		if change := transcode.AttachmentChange(attachedFiles(getStreamsList(origMap, "attachments")), attachedFiles(outRep.Attachments)); change != "" {
			return waitDecision(change), nil
		}
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
	addVideoSizes(result, origMap, plan, outVideo[0].Width, outVideo[0].Height)
	bitDepthStatus := "not_requested"
	if plan.ExpectedBitDepth > 0 {
		bitDepthStatus = "ok"
	}
	validation := map[string]any{"duration": "ok", "video_streams": "ok", "resolution": "ok", "bit_depth": bitDepthStatus, "audio_streams": "ok", "audio_bitrate": audioBitrate, "audio_duration": audioDuration, "subtitle_streams": "ok", "attachments": "ok", "chapters": "ok", "original_sha256_pending": "accept_result"}
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

// sameLanguage is transcode.SameLanguage, the worker's rule too.
func sameLanguage(source, output string) bool { return transcode.SameLanguage(source, output) }

// attachedFiles are attachment streams as transcode.AttachmentChange, the
// worker's rule too, compares them. A source record made before attachment
// sizes were kept carries none.
func attachedFiles(streams []mediainspect.DetailedStream) []transcode.AttachedFile {
	var out []transcode.AttachedFile
	for _, s := range streams {
		out = append(out, transcode.AttachedFile{Name: streamTag(s, "filename"), MimeType: streamTag(s, "mimetype"), Size: s.ExtraDataSize})
	}
	return out
}

// streamTag is a stream's tag by name, whatever its case.
func streamTag(s mediainspect.DetailedStream, name string) string {
	for k, v := range s.Tags {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// sourceTrackTolerance is how much shorter than the source track's own packets
// a candidate track may be: AAC priming and frame rounding, as the worker's
// pre-publish check allows.
const sourceTrackTolerance = 0.5

// sourceTrackSpan measures one audio stream of the original from its packets.
// It reads the whole file, so callers ask only when the candidate looks short.
func (e *Engine) sourceTrackSpans(ctx context.Context, origMap map[string]any) (map[int]mediainspect.AudioPacketSpan, error) {
	path := getString(origMap, "path")
	if path == "" {
		return nil, fmt.Errorf("original path unknown")
	}
	return mediainspect.MeasureAudioPackets(ctx, e.deps.Ffprobe, path)
}

func stateStrings(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
