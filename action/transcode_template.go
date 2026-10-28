package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/jakenesler/navigatorr/transcode/resilience"
	"github.com/jakenesler/navigatorr/transcode/selector"
)

func (e *Engine) registerTranscodeTemplate() {
	e.RegisterTemplate(ActionTemplate{
		AutoReconcile:   true,
		ImmutableInputs: true,
		Name:            "transcode_media", Version: 2,
		Validate:       validateTranscodeMedia,
		Description:    "Set mode=size|quality|x265_preserve|compact for immutable quality and final size policies without profile/priority/tuning; quality is the UI default. Every goal encodes the whole file at a short ladder of fixed settings and the worker's final quality check judges each result: the first rung that passes is kept, a rung that fails on quality is followed by one that compresses less, and a rung refused for its size ends the ladder. x265_preserve requires libx265, skips existing HEVC and permits 0% minimum savings; size requires 15%, converts lossless audio (TrueHD, MLP, FLAC, ALAC, PCM, DTS-HD MA) and any lossy track at least 1.25 times its AAC-LC target (stereo 192, mono 128, 5.1 384, 7.1 512 kbps) to AAC-LC at that target with the same layout, Atmos and DTS:X included, lossless or lossy E-AC-3 JOC (their object/height channels are lost, the channel bed kept), while copying the other lossy tracks, and may encode 8-bit sources as 10-bit HEVC on a worker that proves native 10-bit quality; quality and x265_preserve reject growth. Size also takes allow_hardware (default true; false keeps x265 only): with it, a worker that has Apple VideoToolbox encodes the whole file with VideoToolbox first, and a hardware result is reported with its encoder. Size also takes allow_slow_preset (default false): true lets the x265 ladder use the slow preset, whose full encode of a long 1080p source can take hours. Size also takes one optional size budget (a budget also fixes an audio level for the file, see below), size_budget_percent (5-85, a percent of the source file), size_budget_bytes (a whole number of bytes, at least 50000000) or size_budget_source=siblings (the median total bitrate of the other video files in the same season folder, else the series folder, needing at least 3 of the same resolution class, times size_budget_sibling_multiplier 1.0-4.0, default 1.5), exactly one of them. With allow_hardware on (the default) and a worker that has VideoToolbox, a budget encodes the whole file with VideoToolbox at quality 65, then 58, then 50, then at the average bitrate that lands on the budget; the worker refuses any result over the budget before scoring it, and the first one within it is validated on the final windows against a fixed quality floor (VMAF mean 70, p5 60, worst 5 s window 65; banding limits unchanged). A result below that floor is not offered, and the result says to try again with allow_hardware=false. With allow_hardware=false (or a worker without VideoToolbox) the file is encoded once with two-pass x265 at the bitrate that lands on the budget, held to the same floor. A size budget plans its audio as well: the least aggressive of two levels that leaves the video at least about 0.04 bits per pixel per frame (about 2 Mbps for 1080p at 24 fps), else the more compressed one if the budget allows a video at all. A track is converted only when its bitrate is at least 1.25 times the level's target for its layout. Level 1 converts such tracks, lossless included, to AAC-LC at the target (stereo 192, mono 128, 5.1 384, 7.1 512 kbps); level 2 converts such stereo, quad and 5.1 tracks to HE-AAC (stereo 64, quad and 5.1 160 kbps) and every other layout as level 1. Lossless audio that states no bitrate is converted. Any other track and a lossy track whose bitrate is unknown are copied at both levels (goals frozen before the Atmos bed levels also copy E-AC-3 Atmos); track order, languages, titles, layout, subtitles and attachments of the tracks kept never change. A result within the budget is reported as below the quality target unless it meets it; it still needs the usual approval before it replaces the original. compact makes HDR10, HLG, Dolby Vision 7/8 (base layer) and over-1080p SDR sources much smaller: deterministic tone map to SDR BT.709, downscale to max_height (720, 1080 default, 1440 or 2160), HEVC, lossless/DTS/PCM audio to AAC with the tracks of the languages to keep, 30% minimum savings; HDR, Dolby Vision and Atmos are dropped and Dolby Vision profile 5 is rejected. Size and compact keep only the audio and subtitle tracks in the languages to keep: the global setting (default spa, eng, jpn; GET or PUT /api/maintenance/keep-languages) or this job's keep_languages (ISO 639-2 codes, where ger and deu, fre and fra and the other bibliographic and terminology pairs are one language; [] keeps every track). The original-language audio track (disposition original, else the first audio track), any other audio in its language and any untagged or und track are always kept, and when no audio track matches the list every audio track is kept; a subtitle is kept when it is in a language to keep, in the original track's language, or untagged; video, attachments and chapters are never dropped; when the default audio track is dropped the first kept one becomes the default. keep_audio_streams and keep_subtitle_streams (source stream indexes, for one file only) pick tracks by hand, for tracks with a missing or wrong language tag. The list is frozen with the job, so changing it later never affects a job already planned; a job that drops tracks needs a worker that follows the selection, and the result lists the tracks kept and dropped and the bytes saved. Unsupported sources or unavailable quality capabilities preserve the original. Omit mode for a fixed profile. Coordinates safe candidate-only media transcoding using an immutable recipe-resolved plan, bounded transient retries, worker revalidation, post-transcode stream validation, and original SHA-256 verification.",
		RequiredInputs: []string{"path"}, OptionalInputs: []string{"mode", "keep_languages", "keep_audio_streams", "keep_subtitle_streams", "max_height", "allow_hardware", "hardware_margin_percent", "allow_slow_preset", "size_budget_percent", "size_budget_bytes", "size_budget_source", "size_budget_sibling_multiplier", "profile", "profile_config", "preserve_source_bit_depth", "replace_original", "expected_video_codec", "max_size_increase_percent", "media_type", "is_anime", "min_savings_percent", "surface_worker_busy", "parent_action_id", "batch_item_key", "library_context"}, Destructive: false,
		Steps: []StepDefinition{
			{Name: "preflight", Description: "Inspect source, record its size and modification time (hashing it only for a worker that cannot hash while it copies), resolve profile/recipe and per-stream compatibility plan", Run: e.measureCoordinatorPhase("coordinator_preflight", e.stepTranscodePreflight)},
			{Name: "submit_benchmark", Description: "Plan the goal's first encode: a quality ladder rung or the size budget's encode", Run: e.stepPlanEncode},
			{Name: "wait_benchmark", Description: "Unused since the sample search was removed; kept so the stored step positions of earlier actions still line up", Run: stepRetired},
			{Name: "submit_transcode", Description: "Submit the immutable structured plan to the remote worker with bounded transient retries", Run: e.stepTranscodeSubmit},
			{Name: "wait_transcode", Description: "Monitor transcode progress and classify transient transport failures", Run: e.stepTranscodeWait},
			{Name: "validate_result", Description: "Validate duration, codecs, streams, dispositions, attachments, chapters and candidate integrity", Run: e.stepTranscodeValidate},
			{Name: "accept_result", Description: "Finalize candidate report and verify the original is unchanged: size and modification time against the preflight stat, or a full SHA-256 re-read for an action preflighted with a coordinator hash (not applicable when nothing ran against the original)", Run: e.stepTranscodeAccept},
		},
	})
}

func (e *Engine) stepTranscodePreflight(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	// Everything the request decides without reading the source, including
	// profile_config and its x265 settings, is judged before the hash and the
	// probe. Admission ran the same checks; this repeats them for requests
	// stored before they existed.
	req, err := e.validateTranscodeRequest(ec.ActionName, ec.Inputs)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	if err := e.bindModeParent(ec); err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	modeSnapshot, modeErr := freezeModePolicy(ec)
	if modeErr != nil {
		return StepResult{Status: StepFailed, Error: modeErr.Error()}, nil
	}
	rawPath := strings.TrimSpace(getString(ec.Inputs, "path"))
	if rawPath == "" {
		return StepResult{Status: StepFailed, Error: "input 'path' is required"}, nil
	}
	if e.deps.Fs == nil {
		return StepResult{Status: StepFailed, Error: "filesystem resolver is required for transcode_media"}, nil
	}
	cleanPath, err := e.deps.Fs.ResolveRead(rawPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("path %q is outside allowed read roots: %v", rawPath, err)}, nil
	}
	fi, err := os.Stat(cleanPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("file %q not accessible: %v", cleanPath, err)}, nil
	}
	if fi.IsDir() {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("path %q is a directory, not a media file", cleanPath)}, nil
	}
	if !e.workersConfigured() {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, nil
	}

	ec.State["work_progress"] = map[string]any{"phase": "probing_source", "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if err := e.persistExecutionState(ctx, ec); err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}
	probeStarted := e.now()
	rep, err := e.inspectSourceForPreflight(ctx, ec, cleanPath)
	e.recordCoordinatorPhase(ec, "coordinator_probe", probeStarted, e.now())
	// Probing is over whatever its outcome; a later wait (for example for a
	// worker slot) must not be shown as an endless probe.
	delete(ec.State, "work_progress")
	if err != nil {
		res := StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to probe original media %s (fail closed): %v", cleanPath, err)}
		if _, typed := mediainspect.AsProbeError(err); typed {
			// The class comes from the typed probe error, not from reading a
			// message that holds the path and ffprobe's stderr.
			ec.State["failure_classification"] = string(resilience.MediaProbeFailed)
			res.Outputs = map[string]any{"failure_classification": string(resilience.MediaProbeFailed)}
		}
		return res, nil
	}
	if !rep.Probed {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("ffprobe did not produce a trustworthy detailed inspection for %s (fail closed)", cleanPath)}, nil
	}
	// The tracks the goal keeps are worked out before anything that sizes the
	// output, so a size budget counts only the streams that stay.
	var trackSel *transcode.TrackSelection
	var trackReport *TrackSelectionReport
	delete(ec.State, trackSelectionKey)
	delete(ec.State, trackReportKey)
	// A re-run job keeps the previous run's validation outputs; the report a
	// reader sees must come from this run's selection or from none.
	delete(ec.State, "tracks")
	delete(ec.State, "tracks_message")
	if modeSnapshot != nil {
		sel, err := resolveTrackSelection(modeSnapshot, &rep)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("choosing the tracks to keep: %v", err)}, nil
		}
		if sel != nil {
			trackSel, trackReport = sel, trackSelectionReport(modeSnapshot.Tracks, sel, &rep, fi.Size())
			ec.State[trackSelectionKey], ec.State[trackReportKey] = sel, trackReport
		}
	}
	origMap := map[string]any{"path": cleanPath, "size_bytes": fi.Size(), "duration_sec": rep.DurationSec, "container": rep.Container, "video": rep.Video, "audio": rep.Audio, "subtitles": rep.Subtitles, "attachments": rep.Attachments, "chapters": rep.Chapters, "probed": rep.Probed}
	if rep.BitRate > 0 {
		origMap["bit_rate"] = rep.BitRate
	}
	if video := rep.MainVideo(); len(video) > 0 {
		v0 := video[0]
		origMap["video_codec"] = v0.Codec
		origMap["resolution"] = fmt.Sprintf("%dx%d", v0.Width, v0.Height)
		origMap["bit_depth"] = v0.BitDepth
		if v0.Profile != "" {
			origMap["video_profile"] = v0.Profile
		}
		if v0.PixelFormat != "" {
			origMap["pixel_format"] = v0.PixelFormat
		}
		if v0.RFrameRate != "" {
			origMap["r_frame_rate"] = v0.RFrameRate
		}
		if v0.AvgFrameRate != "" {
			origMap["avg_frame_rate"] = v0.AvgFrameRate
		}
		if v0.FrameRate != "" {
			origMap["frame_rate"] = v0.FrameRate
		}
		if v0.FPS > 0 {
			origMap["fps"] = v0.FPS
		}
		if v0.BitRate > 0 {
			origMap["stream_bit_rate"] = v0.BitRate
		}
		if v0.ColorRange != "" {
			origMap["color_range"] = v0.ColorRange
		}
		if v0.ColorSpace != "" {
			origMap["color_space"] = v0.ColorSpace
		}
		if v0.ColorPrimaries != "" {
			origMap["color_primaries"] = v0.ColorPrimaries
		}
		if v0.ColorTransfer != "" {
			origMap["color_transfer"] = v0.ColorTransfer
		}
	}
	if rep.HDR != nil {
		origMap["hdr"] = rep.HDR
	}
	audioLangs := make([]string, 0, len(rep.Audio))
	for _, a := range rep.Audio {
		if a.Language != "" {
			audioLangs = append(audioLangs, a.Language)
		}
	}
	origMap["audio_languages"] = audioLangs
	subLangs := make([]string, 0, len(rep.Subtitles))
	sourceSubs := make([]recipe.SourceSubtitle, 0, len(rep.Subtitles))
	for i, s := range rep.Subtitles {
		if s.Language != "" {
			subLangs = append(subLangs, s.Language)
		}
		sourceSubs = append(sourceSubs, recipe.SourceSubtitle{SourceStreamIndex: s.Index, TypeIndex: i, Codec: s.Codec})
	}
	origMap["subtitle_languages"] = subLangs

	// The source's identity is the baseline accept_result relies on before a
	// candidate may be kept. It is settled only once the source is known to need
	// worker work: probe-only exits below end without it, since a source that is
	// never encoded or promoted needs no integrity baseline.
	//
	// A worker that advertises LearnSourceDigest hashes the original while it
	// copies it, so the preflight records the stat it already has and reads
	// nothing (see transcode_source_identity.go). Any other worker gets the old
	// whole-file hash here, as does an action admitted with one before this flow.
	origSHA := ""
	baselineSet := false
	hashSource := func() *StepResult {
		if baselineSet {
			return nil
		}
		_, _, hasStat := recordedOriginalStat(ec.State)
		legacyAction := getString(ec.State, "original_sha256") != "" && !hasStat
		learns := false
		if !legacyAction {
			var err error
			if learns, err = workerLearnsSourceDigest(ctx, e.actionWorker(ec)); err != nil {
				// Unknown is not absent: wait for the worker rather than
				// hashing the whole file and dropping the recorded stat.
				res := workerCapabilityLookupResult(err, "source digest")
				return &res
			}
		}
		if learns {
			// A digest pinned for this very file version stays; a file that
			// changed since the stat was recorded gets a fresh baseline, as the
			// old hash did on every run.
			if size, mod, ok := recordedOriginalStat(ec.State); ok && size == fi.Size() && mod.Equal(fi.ModTime()) {
				origSHA = getString(ec.State, "original_sha256")
			} else {
				delete(ec.State, "original_sha256")
				delete(ec.Outputs, "original_sha256")
				// A conflict belongs to the baseline it contradicted.
				delete(ec.State, originalDigestConflictKey)
				delete(ec.Outputs, originalDigestConflictKey)
			}
			recordOriginalStat(ec.State, fi)
			if origSHA != "" {
				origMap["sha256"] = origSHA
			}
			baselineSet = true
			return nil
		}
		// Drain: a file that has not begun hashing has not begun its encode
		// either, so it waits here instead of reading the whole source. The
		// next run starts the hash from zero; nothing is half done.
		if held, reason := e.drainHold(); held {
			res := drainBlockedResult("source hash", reason)
			return &res
		}
		sha, res := e.hashPreflightSource(ctx, ec, cleanPath, fi)
		if res != nil {
			return res
		}
		origSHA = sha
		origMap["sha256"] = sha
		delete(ec.State, originalStatKey)
		baselineSet = true
		return nil
	}

	profile := strings.TrimSpace(getString(ec.Inputs, "profile"))
	ephemeralProfile, ephemeralDigest, hasProfileConfig := req.ProfileConfig, req.ProfileDigest, req.HasProfileConfig
	gateRep := coverArtGateReport(&rep, func() bool { return workerSupportsCoverArt(ctx, e.actionWorker(ec)) })
	var compactPlan *transcode.CompactPlan
	if modeSnapshot != nil {
		hfrSupported, err := (&hfrQualitySupport{e: e, worker: e.actionWorker(ec)}).forSource(ctx, gateRep)
		if err != nil {
			return hfrCapabilityLookupResult(err), nil
		}
		reason := modeSourceReason(modeSnapshot.Mode, gateRep, hfrSupported)
		if reason == "" && modeSnapshot.Mode == "compact" {
			compactPlan, reason = compactSourcePlan(ctx, e.deps.Ffprobe, cleanPath, gateRep, modeSnapshot)
		}
		if reason == "" {
			// A budget the other streams use up cannot be met by any encode or
			// search, so it ends before the source is hashed.
			if result, ended := endUnreachableSizeBudget(ec, modeSnapshot, &rep, fi.Size(), nil); ended {
				if result.Status == StepCompleted {
					ec.State["resolved_path"] = cleanPath
					ec.State["original_size"] = fi.Size()
					ec.State["original"] = origMap
				}
				return result, nil
			}
		}
		if reason != "" {
			ec.State["skip_transcode"] = true
			ec.State["reason_code"] = reason
			ec.State["resolved_path"] = cleanPath
			ec.State["original"] = origMap
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"skipped": true, "reason_code": reason, "mode": modeSnapshot.Mode, "policy_digest": modeSnapshot.Digest}}, nil
		}
		ephemeralProfile = modeSnapshot.Profile
		hasProfileConfig = true
		profile = "mode-" + modeSnapshot.Mode
	} else if hasProfileConfig {
		profile = "ephemeral"
	} else {
		if profile == "" && e.deps.Config != nil && strings.TrimSpace(e.deps.Config.Transcode.DefaultProfile) != "" {
			profile = strings.TrimSpace(e.deps.Config.Transcode.DefaultProfile)
		}
		if profile == "" {
			profile = "hevc-vt"
		}
	}

	var autoResult *selector.Result
	if !hasProfileConfig && strings.EqualFold(profile, "auto") {
		mediaType := strings.ToLower(strings.TrimSpace(getString(ec.Inputs, "media_type")))
		isAnime := getBool(ec.Inputs, "is_anime")
		if !isAnime && mediaType == "anime" {
			isAnime = true
		}

		minSavings := 0.0
		if e.deps.Config != nil {
			minSavings = e.deps.Config.Transcode.MinSavingsPercent
		}
		if rawSavings := getFloat(ec.Inputs, "min_savings_percent"); rawSavings > 0 {
			minSavings = rawSavings
		}

		selInput := selector.Input{
			Report:            *gateRep,
			MediaType:         mediaType,
			IsAnime:           isAnime,
			MinSavingsPercent: minSavings,
		}
		res := selector.Select(selInput)
		autoResult = &res

		ec.State["auto_decision"] = res.Decision
		ec.State["auto_reasons"] = res.Reasons
		ec.State["auto_profile"] = res.Profile
		ec.State["expected_savings_percent"] = res.ExpectedSavingsPercent

		if res.Decision == selector.DecisionSkip || res.Decision == selector.DecisionReview {
			ec.State["skip_transcode"] = true
			ec.State["resolved_path"] = cleanPath
			ec.State["original_size"] = fi.Size()
			ec.State["original"] = origMap
			ec.State["profile"] = "auto"
			out := map[string]any{
				"original":                 origMap,
				"resolved_path":            cleanPath,
				"profile":                  "auto",
				"auto_decision":            res.Decision,
				"auto_reasons":             res.Reasons,
				"expected_savings_percent": res.ExpectedSavingsPercent,
				"skipped":                  true,
				"message":                  fmt.Sprintf("Auto profile decided to %s (reasons: %s)", res.Decision, strings.Join(res.Reasons, ", ")),
			}
			return StepResult{Status: StepCompleted, Outputs: out}, nil
		}

		profile = res.Profile
	}

	if e.deps.Config == nil {
		return StepResult{Status: StepFailed, Error: "navigatorr config is required to resolve transcode recipes"}, nil
	}
	var plan *transcode.Plan
	var recipeProfile recipe.Profile
	if hasProfileConfig {
		plan, recipeProfile, ephemeralDigest, err = e.deps.Config.Transcode.ResolveEphemeralPlanForSource(ephemeralProfile, sourceSubs)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("resolving ephemeral profile_config: %v", err)}, nil
		}
	} else {
		plan, recipeProfile, err = e.deps.Config.Transcode.ResolvePlanAndProfileForSource(profile, sourceSubs)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("resolving transcode profile %q: %v", profile, err)}, nil
		}
	}
	preserveBitDepth := req.PreserveBitDepth
	if video := rep.MainVideo(); len(video) > 0 {
		if want := req.PinnedBitDepth; want > 0 && (video[0].BitDepth == 8 || video[0].BitDepth == 10) && want != video[0].BitDepth {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("profile_config requests %d-bit output for a %d-bit source; set preserve_source_bit_depth=false to use the requested conversion", want, video[0].BitDepth)}, nil
		}
		if err := preserveSourceBitDepth(plan, video[0].BitDepth, preserveBitDepth); err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("resolving transcode profile %q: %v", profile, err)}, nil
		}
	}
	if modeSnapshot != nil {
		if video := rep.MainVideo(); modeSnapshot.Size != nil && modeSnapshot.Size.EightBitMain10 && len(video) > 0 && video[0].BitDepth == 8 {
			caps, err := e.actionWorker(ec).Capabilities(ctx)
			if err != nil {
				return workerCapabilityLookupResult(err, "size Main10"), nil
			}
			if sizeMain10Supported(caps) {
				if err := setMain10(plan); err != nil {
					return StepResult{Status: StepFailed, Error: err.Error()}, nil
				}
				ec.State["size_main10"] = true
			}
		}
		if modeSnapshot.Mode == "compact" {
			c := compactPlan
			if c == nil {
				return StepResult{Status: StepFailed, Error: "compact plan missing after source classification (fail closed)"}, nil
			}
			plan.Compact = c.Clone()
			// HDR and DV sources become SDR; the encode keeps 10-bit output
			// (the tone-mapped picture is 10-bit), SDR keeps the source depth.
			if c.ToneMap != nil && plan.ExpectedBitDepth != 10 {
				return StepResult{Status: StepFailed, Error: "compact HDR conversion requires 10-bit output (fail closed)"}, nil
			}
		}
		if err := applyModePlan(plan, modeSnapshot); err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, nil
		}
		if err := applyTrackSelection(plan, trackSel); err != nil {
			return StepResult{Status: StepFailed, Error: err.Error()}, nil
		}
	}
	ext, err := transcode.ContainerExtension(plan.Container)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}

	// A fixed profile never measures samples. One that still asks for the
	// removed sample search belongs to an action admitted before it went.
	if recipeProfile.Optimization != nil && recipeProfile.Optimization.Enabled {
		if modeSnapshot == nil {
			return StepResult{Status: StepFailed, Error: removedSearchMessage}, nil
		}
		// The goal's quality limits and sample windows are the ones its final
		// validation holds every rung to (see transcode_final_validation.go).
		ec.State["optimization_policy"] = recipeProfile.Optimization
	}

	// Every path from here submits worker work, so it needs the baseline.
	if res := hashSource(); res != nil {
		return *res, nil
	}
	ec.State["resolved_path"] = cleanPath
	if origSHA != "" {
		ec.State["original_sha256"] = origSHA
	}
	ec.State["original_size"] = fi.Size()
	ec.State["original"] = origMap
	ec.State["source_report"] = rep
	e.recordSizeBudgetSiblings(ctx, ec, modeSnapshot, cleanPath, &rep, fi.Size())
	ec.State["profile"] = profile
	ec.State["plan"] = plan
	ec.State["plan_source"] = "profile_base"
	ec.State["candidate_extension"] = ext
	ec.State["recipe_version"] = plan.RecipeVersion
	ec.State["recipe_digest"] = plan.RecipeDigest
	ec.State["plan_digest"] = plan.PlanDigest
	ec.State["preserve_source_bit_depth"] = preserveBitDepth
	ec.State["applied_fallbacks"] = plan.AppliedFallbacks
	if hasProfileConfig {
		ec.State["ephemeral_profile"] = recipeProfile
		ec.State["ephemeral_recipe_digest"] = ephemeralDigest
	}
	ec.State["fallback_count"] = len(plan.AppliedFallbacks)
	ec.State["attempt"] = 1
	ec.State["retry_count"] = 0
	outputs := map[string]any{
		"original":                  origMap,
		"resolved_path":             cleanPath,
		"profile":                   profile,
		"plan":                      plan,
		"plan_source":               "profile_base",
		"recipe_version":            plan.RecipeVersion,
		"recipe_digest":             plan.RecipeDigest,
		"plan_digest":               plan.PlanDigest,
		"preserve_source_bit_depth": preserveBitDepth,
		"applied_fallbacks":         plan.AppliedFallbacks,
	}
	if origSHA != "" {
		outputs["original_sha256"] = origSHA
	}
	if hasProfileConfig {
		outputs["ephemeral_profile"] = recipeProfile
		outputs["ephemeral_recipe_digest"] = ephemeralDigest
	}
	if trackReport != nil {
		outputs[trackSelectionKey], outputs[trackReportKey] = trackSel, trackReport
	}
	if autoResult != nil {
		outputs["auto_decision"] = autoResult.Decision
		outputs["auto_reasons"] = autoResult.Reasons
		outputs["auto_profile"] = autoResult.Profile
		outputs["expected_savings_percent"] = autoResult.ExpectedSavingsPercent
	}
	return StepResult{Status: StepCompleted, Outputs: outputs}, nil
}

// hashPreflightSource reads the whole source once for the integrity baseline
// accept_result later re-verifies. The probe ran before this read, so the file
// is also stat'd afterwards: a source replaced in between would otherwise get
// a hash that matches the replacement, not the media the plan was built from.
func (e *Engine) hashPreflightSource(ctx context.Context, ec *ExecutionContext, cleanPath string, fi os.FileInfo) (string, *StepResult) {
	fail := func(format string, args ...any) (string, *StepResult) {
		return "", &StepResult{Status: StepFailed, Error: fmt.Sprintf(format, args...)}
	}
	f, err := os.Open(cleanPath)
	if err != nil {
		return fail("failed to open original file %s: %v", cleanPath, err)
	}
	h := sha256.New()
	var lastObservation time.Time
	observe := promotionHashProgress(func(_ string, read, total int64) error {
		if read != 0 && read != total && time.Since(lastObservation) < time.Second {
			return nil
		}
		lastObservation = time.Now()
		ec.State["work_progress"] = map[string]any{"phase": "hashing_source", "bytes_read": read, "total_bytes": total, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
		return e.persistExecutionState(ctx, ec)
	})
	// Show a queued hash as waiting, not as a hash stuck at zero bytes, and
	// time only the hash itself.
	ec.State["work_progress"] = map[string]any{"phase": "waiting_for_hash_slot", "total_bytes": fi.Size(), "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if err := e.persistExecutionState(ctx, ec); err != nil {
		_ = f.Close()
		return fail("%v", err)
	}
	release, held, err := e.acquireHashSlotUnlessDraining(ctx)
	if err != nil {
		_ = f.Close()
		return fail("failed to compute hash of original file %s: %v", cleanPath, err)
	}
	if held != nil {
		_ = f.Close()
		delete(ec.State, "work_progress")
		return "", held
	}
	defer release()
	if err := observe(cleanPath, 0, fi.Size()); err != nil {
		_ = f.Close()
		return fail("%v", err)
	}
	hashStarted := e.now()
	_, hashErr := io.Copy(h, &promotionContextReader{ctx: ctx, r: f, path: cleanPath, total: fi.Size(), progress: observe})
	release()
	_ = f.Close()
	e.recordCoordinatorPhase(ec, "coordinator_source_hash", hashStarted, e.now())
	delete(ec.State, "work_progress")
	if hashErr != nil {
		return fail("failed to compute hash of original file %s: %v", cleanPath, hashErr)
	}
	after, err := os.Stat(cleanPath)
	if err != nil {
		return fail("original file %s is no longer accessible after hashing: %v", cleanPath, err)
	}
	if after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime()) {
		ec.State["failure_classification"] = string(resilience.SourceChanged)
		return fail("original file %s changed while it was being inspected (fail closed)", cleanPath)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func resolvePreserveSourceBitDepth(inputs map[string]any) (bool, error) {
	raw, present := inputs["preserve_source_bit_depth"]
	if !present {
		return true, nil
	}
	preserve, ok := raw.(bool)
	if !ok {
		return false, inputErr("preserve_source_bit_depth", "input 'preserve_source_bit_depth' must be a boolean")
	}
	return preserve, nil
}

func preserveSourceBitDepth(plan *transcode.Plan, sourceBitDepth int, preserve bool) error {
	if plan == nil {
		return fmt.Errorf("resolved plan is missing")
	}
	if !preserve {
		return nil
	}
	if sourceBitDepth != 8 && sourceBitDepth != 10 {
		return nil
	}
	if plan.ExpectedBitDepth == sourceBitDepth {
		return nil
	}
	if sourceBitDepth == 10 {
		plan.VideoProfile = "main10"
		plan.PixelFormat = "p010le"
		plan.ExpectedBitDepth = 10
	} else {
		plan.VideoProfile = "main"
		plan.PixelFormat = "yuv420p"
		plan.ExpectedBitDepth = 8
	}
	plan.PlanDigest = ""
	digest, err := transcode.DigestPlan(plan)
	if err != nil {
		return err
	}
	plan.PlanDigest = digest
	return nil
}

func decodeEphemeralProfileInput(inputs map[string]any) (recipe.Profile, string, bool, error) {
	raw, present := inputs["profile_config"]
	if !present || raw == nil {
		return recipe.Profile{}, "", false, nil
	}
	if strings.TrimSpace(getString(inputs, "profile")) != "" {
		return recipe.Profile{}, "", false, inputErr("profile_config", "profile and profile_config are mutually exclusive; profile_config is a complete ephemeral profile")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return recipe.Profile{}, "", false, inputErr("profile_config", "encoding profile_config: %v", err)
	}
	profile, digest, err := recipe.DecodeProfileStrict("ephemeral", b)
	if err != nil {
		return recipe.Profile{}, "", false, inputErr("profile_config", "invalid profile_config: %v", err)
	}
	return profile, digest, true, nil
}

func (e *Engine) validateTranscodeInputs(ec *ExecutionContext) error {
	tmpl, ok := e.GetTemplate(ec.ActionName)
	if !ok {
		return fmt.Errorf("unknown action template %q", ec.ActionName)
	}
	return unsupportedInput(tmpl, ec.Inputs)
}
