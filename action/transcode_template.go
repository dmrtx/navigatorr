package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/jakenesler/navigatorr/transcode/selector"
)

func (e *Engine) registerTranscodeTemplate() {
	e.RegisterTemplate(ActionTemplate{
		Name: "transcode_media", Version: 2,
		Description:    "Coordinates safe candidate-only media transcoding using an immutable recipe-resolved plan, bounded transient retries, worker revalidation, post-transcode stream validation, and original SHA-256 verification.",
		RequiredInputs: []string{"path"}, OptionalInputs: []string{"profile", "replace_original", "expected_video_codec", "max_size_increase_percent", "media_type", "is_anime", "min_savings_percent", "surface_worker_busy"}, Destructive: false,
		Steps: []StepDefinition{
			{Name: "preflight", Description: "Inspect source, hash original, resolve profile/recipe and per-stream compatibility plan", Run: e.stepTranscodePreflight},
			{Name: "submit_transcode", Description: "Submit the immutable structured plan to the remote worker with bounded transient retries", Run: e.stepTranscodeSubmit},
			{Name: "wait_transcode", Description: "Monitor transcode progress and classify transient transport failures", Run: e.stepTranscodeWait},
			{Name: "validate_result", Description: "Validate duration, codecs, streams, dispositions, attachments, chapters and candidate integrity", Run: e.stepTranscodeValidate},
			{Name: "accept_result", Description: "Finalize candidate report and verify original SHA-256 is unchanged", Run: e.stepTranscodeAccept},
		},
	})
}

func (e *Engine) stepTranscodePreflight(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getBool(ec.Inputs, "replace_original") {
		return StepResult{Status: StepFailed, Error: "destructive replacement (replace_original: true) is not supported; transcoding is candidate-only and never modifies the original"}, nil
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
	if e.deps.Transcode == nil {
		return StepResult{Status: StepFailed, Error: "transcode executor is disabled or not configured in navigatorr"}, nil
	}

	f, err := os.Open(cleanPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to open original file %s: %v", cleanPath, err)}, nil
	}
	h := sha256.New()
	_, hashErr := io.Copy(h, f)
	_ = f.Close()
	if hashErr != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to compute hash of original file %s: %v", cleanPath, hashErr)}, nil
	}
	origSHA := hex.EncodeToString(h.Sum(nil))
	rep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, cleanPath)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to probe original media %s: %v", cleanPath, err)}, nil
	}
	if !rep.Probed {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("ffprobe did not produce a trustworthy detailed inspection for %s (fail closed)", cleanPath)}, nil
	}
	origMap := map[string]any{"path": cleanPath, "size_bytes": fi.Size(), "sha256": origSHA, "duration_sec": rep.DurationSec, "container": rep.Container, "video": rep.Video, "audio": rep.Audio, "subtitles": rep.Subtitles, "attachments": rep.Attachments, "chapters": rep.Chapters, "probed": rep.Probed}
	if len(rep.Video) > 0 {
		origMap["video_codec"] = rep.Video[0].Codec
		origMap["resolution"] = fmt.Sprintf("%dx%d", rep.Video[0].Width, rep.Video[0].Height)
		origMap["bit_depth"] = rep.Video[0].BitDepth
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

	profile := strings.TrimSpace(getString(ec.Inputs, "profile"))
	if profile == "" && e.deps.Config != nil && strings.TrimSpace(e.deps.Config.Transcode.DefaultProfile) != "" {
		profile = strings.TrimSpace(e.deps.Config.Transcode.DefaultProfile)
	}
	if profile == "" {
		profile = "hevc-vt"
	}

	if getBool(ec.Inputs, "surface_worker_busy") {
		ec.State["surface_worker_busy"] = true
	}

	var autoResult *selector.Result
	if strings.EqualFold(profile, "auto") {
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
			Report:            rep,
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
			ec.State["original_sha256"] = origSHA
			ec.State["original_size"] = fi.Size()
			ec.State["original"] = origMap
			ec.State["profile"] = "auto"
			out := map[string]any{
				"original":                 origMap,
				"original_sha256":          origSHA,
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
	plan, err := e.deps.Config.Transcode.ResolvePlanForSource(profile, sourceSubs)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("resolving transcode profile %q: %v", profile, err)}, nil
	}
	ext, err := transcode.ContainerExtension(plan.Container)
	if err != nil {
		return StepResult{Status: StepFailed, Error: err.Error()}, nil
	}

	ec.State["resolved_path"] = cleanPath
	ec.State["original_sha256"] = origSHA
	ec.State["original_size"] = fi.Size()
	ec.State["original"] = origMap
	ec.State["profile"] = profile
	ec.State["plan"] = plan
	ec.State["candidate_extension"] = ext
	ec.State["recipe_version"] = plan.RecipeVersion
	ec.State["recipe_digest"] = plan.RecipeDigest
	ec.State["plan_digest"] = plan.PlanDigest
	ec.State["applied_fallbacks"] = plan.AppliedFallbacks
	ec.State["fallback_count"] = len(plan.AppliedFallbacks)
	ec.State["attempt"] = 1
	ec.State["retry_count"] = 0
	outputs := map[string]any{
		"original":          origMap,
		"original_sha256":   origSHA,
		"resolved_path":     cleanPath,
		"profile":           profile,
		"plan":              plan,
		"recipe_version":    plan.RecipeVersion,
		"recipe_digest":     plan.RecipeDigest,
		"plan_digest":       plan.PlanDigest,
		"applied_fallbacks": plan.AppliedFallbacks,
	}
	if autoResult != nil {
		outputs["auto_decision"] = autoResult.Decision
		outputs["auto_reasons"] = autoResult.Reasons
		outputs["auto_profile"] = autoResult.Profile
		outputs["expected_savings_percent"] = autoResult.ExpectedSavingsPercent
	}
	return StepResult{Status: StepCompleted, Outputs: outputs}, nil
}
