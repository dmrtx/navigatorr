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

func (e *Engine) stepTranscodeValidate(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getBool(ec.State, "skip_transcode") {
		return StepResult{
			Status:  StepSkipped,
			Outputs: map[string]any{"skipped": true},
		}, nil
	}
	if ec.Decision != "" {
		if strings.EqualFold(ec.Decision, "reject") || strings.EqualFold(ec.Decision, "cancel") {
			return StepResult{Status: StepFailed, Error: "transcode candidate rejected by user decision; original file remains untouched"}, nil
		}
		if strings.EqualFold(ec.Decision, "accept_loss") || strings.EqualFold(ec.Decision, "approve") {
			ec.State["validation_decision_applied"] = ec.Decision
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"decision_applied": ec.Decision, "note": "validation discrepancy accepted by user decision"}}, nil
		}
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
	origDur := getFloat(origMap, "duration_sec")
	if origDur > 0 && outRep.DurationSec > 0 {
		d := math.Abs(outRep.DurationSec - origDur)
		if d > 3 && (d/origDur) > 0.02 {
			return waitDecision(fmt.Sprintf("Duration discrepancy: original was %.1fs, output is %.1fs (difference: %.1fs)", origDur, outRep.DurationSec, d)), nil
		}
	}
	if len(outRep.Video) == 0 {
		return StepResult{Status: StepFailed, Error: "transcoded candidate contains no video streams"}, nil
	}
	expected := strings.ToLower(strings.TrimSpace(getString(ec.Inputs, "expected_video_codec")))
	if expected == "" {
		expected = expectedVideoCodec(plan.VideoCodec)
	}
	if expected != "" && !strings.Contains(strings.ToLower(outRep.Video[0].Codec), expected) {
		return waitDecision(fmt.Sprintf("Video codec mismatch: expected %s, transcoded output is %s", expected, outRep.Video[0].Codec)), nil
	}
	origAudio := getStreamsList(origMap, "audio")
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
	for i, a := range origAudio {
		b := outRep.Audio[i]
		if plan.AudioMode == "copy" && normCodec(a.Codec) != normCodec(b.Codec) {
			return waitDecision(fmt.Sprintf("Audio codec mismatch at stream %d: original=%s output=%s", i, a.Codec, b.Codec)), nil
		}
		if a.Language != "" && normCodec(a.Language) != normCodec(b.Language) {
			return waitDecision(fmt.Sprintf("Audio language mismatch at stream %d: original=%s output=%s", i, a.Language, b.Language)), nil
		}
		if a.Channels > 0 && b.Channels != a.Channels {
			return waitDecision(fmt.Sprintf("Audio channel mismatch at stream %d: original=%d output=%d", i, a.Channels, b.Channels)), nil
		}
	}
	origSubs := getStreamsList(origMap, "subtitles")
	if len(outRep.Subtitles) != len(origSubs) {
		return waitDecision(fmt.Sprintf("Subtitle stream lost: original=%d output=%d", len(origSubs), len(outRep.Subtitles))), nil
	}
	actions := map[int]transcode.SubtitleAction{}
	for _, a := range plan.SubtitleActions {
		actions[a.TypeIndex] = a
	}
	for i, a := range origSubs {
		b := outRep.Subtitles[i]
		act, ok := actions[i]
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
	result := map[string]any{"candidate_path": outputPath, "output_path": outputPath, "size_bytes": fi.Size(), "duration_sec": outRep.DurationSec, "video_codec": outRep.Video[0].Codec, "resolution": fmt.Sprintf("%dx%d", outRep.Video[0].Width, outRep.Video[0].Height), "profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest, "plan_digest": plan.PlanDigest, "attempt": getInt(ec.State, "attempt"), "retry_count": getInt(ec.State, "retry_count"), "fallback_count": getInt(ec.State, "fallback_count"), "applied_fallbacks": plan.AppliedFallbacks}
	if c := ec.State["conversions"]; c != nil {
		result["conversions"] = c
	}
	validation := map[string]any{"duration": "ok", "video_streams": "ok", "audio_streams": "ok", "subtitle_streams": "ok", "attachments": "ok", "chapters": "ok", "original_sha256_pending": "accept_result"}
	ec.State["validation"] = validation
	ec.State["result"] = result
	ec.State["size_saved_bytes"] = saved
	ec.State["size_saved_percent"] = pct
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"result": result, "validation": validation, "candidate_path": outputPath, "output_path": outputPath, "size_saved_bytes": saved, "size_saved_percent": pct, "profile": getString(ec.State, "profile"), "recipe_version": plan.RecipeVersion, "recipe_digest": plan.RecipeDigest}}, nil
}

func expectedVideoCodec(codec string) string {
	switch normCodec(codec) {
	case "hevc_videotoolbox":
		return "hevc"
	default:
		return normCodec(codec)
	}
}

func normCodec(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
