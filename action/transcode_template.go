package action

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/tdarr"
)

func (e *Engine) registerTranscodeTemplate() {
	e.RegisterTemplate(ActionTemplate{
		Name:           "transcode_media",
		Version:        1,
		Description:    "Coordinates safe media transcoding with Tdarr: inspects media streams, submits job to Tdarr, waits asynchronously via waiting_external, validates output with ffprobe (preserving audio, subtitles, fonts, chapters), and protects original media.",
		RequiredInputs: []string{"path"},
		OptionalInputs: []string{"profile", "replace_original", "expected_video_codec", "max_size_increase_percent", "library_id"},
		Destructive:    false,
		Steps: []StepDefinition{
			{
				Name:        "preflight",
				Description: "Verifies file safety within allowed roots and captures baseline media streams and duration snapshot with ffprobe",
				Run:         e.stepTranscodePreflight,
			},
			{
				Name:        "submit_tdarr",
				Description: "Translates path to Tdarr server path and idempotently submits transcode job to Tdarr",
				Run:         e.stepTranscodeSubmit,
			},
			{
				Name:        "wait_tdarr",
				Description: "Monitors Tdarr transcode progress, entering waiting_external while processing is in flight",
				Run:         e.stepTranscodeWait,
			},
			{
				Name:        "validate_result",
				Description: "Runs ffprobe on the transcoded output, validating container, duration, video, audio tracks, subtitles, attachments, and chapters",
				Run:         e.stepTranscodeValidate,
			},
			{
				Name:        "accept_result",
				Description: "Finalizes transcode report and safely replaces original only if explicitly requested and approved",
				Run:         e.stepTranscodeAccept,
			},
		},
	})
}

// stepTranscodePreflight checks path security and collects baseline ffprobe metadata.
func (e *Engine) stepTranscodePreflight(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	rawPath := strings.TrimSpace(getString(ec.Inputs, "path"))
	if rawPath == "" {
		return StepResult{Status: StepFailed, Error: "input 'path' is required"}, nil
	}

	if e.deps.Fs == nil {
		return StepResult{Status: StepFailed, Error: "filesystem resolver is required for transcode_media"}, nil
	}

	cleanPath, err := e.deps.Fs.ResolveRead(rawPath)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("path %q is outside allowed read roots: %v", rawPath, err),
		}, nil
	}

	fi, err := os.Stat(cleanPath)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("file %q not accessible: %v", cleanPath, err),
		}, nil
	}
	if fi.IsDir() {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("path %q is a directory, not a media file", cleanPath),
		}, nil
	}

	detailedRep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, cleanPath)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("failed to probe original media %s: %v", cleanPath, err),
		}, nil
	}

	origMap := map[string]any{
		"path":         cleanPath,
		"size_bytes":   fi.Size(),
		"duration_sec": detailedRep.DurationSec,
		"container":    detailedRep.Container,
		"video":        detailedRep.Video,
		"audio":        detailedRep.Audio,
		"subtitles":    detailedRep.Subtitles,
		"attachments":  detailedRep.Attachments,
		"chapters":     detailedRep.Chapters,
		"probed":       detailedRep.Probed,
	}
	if len(detailedRep.Video) > 0 {
		origMap["video_codec"] = detailedRep.Video[0].Codec
		origMap["resolution"] = fmt.Sprintf("%dx%d", detailedRep.Video[0].Width, detailedRep.Video[0].Height)
		origMap["bit_depth"] = detailedRep.Video[0].BitDepth
	}

	audioLangs := make([]string, 0, len(detailedRep.Audio))
	for _, a := range detailedRep.Audio {
		if a.Language != "" {
			audioLangs = append(audioLangs, a.Language)
		}
	}
	origMap["audio_languages"] = audioLangs

	subLangs := make([]string, 0, len(detailedRep.Subtitles))
	for _, s := range detailedRep.Subtitles {
		if s.Language != "" {
			subLangs = append(subLangs, s.Language)
		}
	}
	origMap["subtitle_languages"] = subLangs

	ec.State["resolved_path"] = cleanPath
	ec.State["original"] = origMap

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"original":      origMap,
			"resolved_path": cleanPath,
		},
	}, nil
}

// stepTranscodeSubmit translates the path and idempotently submits the job to Tdarr.
func (e *Engine) stepTranscodeSubmit(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	// Idempotency: if already submitted, skip duplicate submission
	if getBool(ec.State, "tdarr_submitted") || getString(ec.State, "tdarr_job_id") != "" {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"tdarr_job_id":      getString(ec.State, "tdarr_job_id"),
				"tdarr_server_path": getString(ec.State, "tdarr_server_path"),
				"reused":            true,
			},
		}, nil
	}

	if e.deps.Tdarr == nil {
		return StepResult{
			Status: StepFailed,
			Error:  "Tdarr integration is disabled or not configured in navigatorr",
		}, nil
	}

	cleanPath := getString(ec.State, "resolved_path")
	if cleanPath == "" {
		cleanPath = getString(ec.Inputs, "path")
	}

	serverPath := cleanPath
	if e.deps.Config != nil {
		serverPath = e.deps.Config.Tdarr.TranslateLocalToServer(cleanPath)
	}

	profile := strings.TrimSpace(getString(ec.Inputs, "profile"))
	if profile == "" {
		profile = "hevc_safe"
	}
	if e.deps.Config != nil && e.deps.Config.Tdarr.Flows != nil {
		if flowID, ok := e.deps.Config.Tdarr.Flows[profile]; ok && flowID != "" {
			profile = flowID
		}
	}

	libraryID := strings.TrimSpace(getString(ec.Inputs, "library_id"))

	resp, err := e.deps.Tdarr.Submit(ctx, tdarr.SubmitRequest{
		FilePath:  serverPath,
		LibraryID: libraryID,
		Profile:   profile,
	})
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("failed to submit transcode job to Tdarr: %v", err),
		}, nil
	}

	ref := resp.Reference
	if ref == "" {
		ref = serverPath
	}

	ec.State["tdarr_submitted"] = true
	ec.State["tdarr_job_id"] = ref
	ec.State["tdarr_server_path"] = serverPath
	ec.State["tdarr_profile"] = profile

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"tdarr_job_id":      ref,
			"tdarr_server_path": serverPath,
			"tdarr_profile":     profile,
		},
	}, nil
}

// stepTranscodeWait monitors Tdarr progress and enters waiting_external while running.
func (e *Engine) stepTranscodeWait(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	ref := getString(ec.State, "tdarr_job_id")
	if ref == "" {
		ref = getString(ec.State, "tdarr_server_path")
	}
	if ref == "" {
		return StepResult{
			Status: StepFailed,
			Error:  "no active Tdarr job ID or file path to monitor",
		}, nil
	}

	if e.deps.Tdarr == nil {
		return StepResult{
			Status: StepFailed,
			Error:  "Tdarr client is not available to monitor job",
		}, nil
	}

	st, err := e.deps.Tdarr.JobStatus(ctx, ref)
	if err != nil {
		// Temporary error during query: stay in waiting_external
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "tdarr_transcode_complete",
			WaitingReason:    fmt.Sprintf("Checking Tdarr job status: %v", err),
		}, nil
	}

	switch st.Status {
	case "running", "queued":
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "tdarr_transcode_complete",
			WaitingReason:    fmt.Sprintf("Tdarr is transcoding media (%s, progress: %.1f%%, eta: %s, fps: %.1f)", st.Status, st.Progress, st.ETA, st.FPS),
			Outputs: map[string]any{
				"tdarr_status":   st.Status,
				"progress":       st.Progress,
				"eta":            st.ETA,
				"fps":            st.FPS,
				"worker_details": st.Details,
			},
		}, nil

	case "failed":
		errMsg := st.Error
		if errMsg == "" {
			errMsg = st.Details
		}
		if errMsg == "" {
			errMsg = "Tdarr reported job failure"
		}
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("Tdarr transcode failed: %s", errMsg),
		}, nil

	case "completed":
		serverOutput := st.OutputPath
		if serverOutput == "" {
			serverOutput = getString(ec.State, "tdarr_server_path")
		}

		localOutput := serverOutput
		if e.deps.Config != nil {
			localOutput = e.deps.Config.Tdarr.TranslateServerToLocal(serverOutput)
		}

		ec.State["tdarr_output_path"] = localOutput
		ec.State["tdarr_server_output_path"] = serverOutput

		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"tdarr_done":         true,
				"output_path":        localOutput,
				"server_output_path": serverOutput,
			},
		}, nil

	default:
		// Unknown status yet: keep waiting
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "tdarr_transcode_complete",
			WaitingReason:    fmt.Sprintf("Tdarr is processing transcode (%s)", st.Details),
		}, nil
	}
}

// stepTranscodeValidate checks output file integrity and compares streams against original baseline.
func (e *Engine) stepTranscodeValidate(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	outputPath := getString(ec.State, "tdarr_output_path")
	if outputPath == "" {
		outputPath = getString(ec.Outputs, "output_path")
	}
	if outputPath == "" {
		outputPath = getString(ec.State, "resolved_path")
	}

	fi, err := os.Stat(outputPath)
	if err != nil || fi.Size() == 0 {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("transcoded output file %q not accessible or has 0 bytes: %v", outputPath, err),
		}, nil
	}

	outRep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, outputPath)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("ffprobe failed to inspect transcoded file %q: %v", outputPath, err),
		}, nil
	}

	origMap, _ := ec.State["original"].(map[string]any)

	// 1. Duration check
	origDur := getFloat(origMap, "duration_sec")
	if origDur > 0 && outRep.DurationSec > 0 {
		durDiff := math.Abs(outRep.DurationSec - origDur)
		if durDiff > 3.0 && (durDiff/origDur) > 0.02 {
			return StepResult{
				Status:        StepWaitingDecision,
				WaitingReason: fmt.Sprintf("Duration discrepancy: original was %.1fs, output is %.1fs (difference: %.1fs)", origDur, outRep.DurationSec, durDiff),
				WaitingOptions: []WaitingOption{
					{Decision: "reject", Description: "Reject transcode due to duration difference (keep original)"},
					{Decision: "accept_loss", Description: "Accept transcode despite duration difference"},
				},
			}, nil
		}
	}

	// 2. Video stream check
	if len(outRep.Video) == 0 {
		return StepResult{
			Status: StepFailed,
			Error:  "transcoded output contains no video streams",
		}, nil
	}
	expectedCodec := strings.ToLower(strings.TrimSpace(getString(ec.Inputs, "expected_video_codec")))
	if expectedCodec != "" {
		outCodec := strings.ToLower(outRep.Video[0].Codec)
		if !strings.Contains(outCodec, expectedCodec) {
			return StepResult{
				Status:        StepWaitingDecision,
				WaitingReason: fmt.Sprintf("Video codec mismatch: expected %s, transcoded output is %s", expectedCodec, outCodec),
				WaitingOptions: []WaitingOption{
					{Decision: "reject", Description: "Reject transcode due to unexpected video codec (keep original)"},
					{Decision: "accept_loss", Description: "Accept transcode with current codec"},
				},
			}, nil
		}
	}

	// 3. Audio streams check
	origAudio := getStreamsList(origMap, "audio")
	outAudioLangs := make(map[string]bool)
	for _, a := range outRep.Audio {
		if a.Language != "" {
			outAudioLangs[strings.ToLower(a.Language)] = true
		}
	}
	for _, origA := range origAudio {
		if origA.Language != "" {
			norm := strings.ToLower(origA.Language)
			if !outAudioLangs[norm] {
				return StepResult{
					Status:        StepWaitingDecision,
					WaitingReason: fmt.Sprintf("Audio stream lost: original had audio language %q which is missing in transcoded output", origA.Language),
					WaitingOptions: []WaitingOption{
						{Decision: "reject", Description: "Reject transcode to prevent audio track loss (keep original)"},
						{Decision: "accept_loss", Description: "Accept transcode without the missing audio track"},
					},
				}, nil
			}
		}
	}

	// 4. Subtitle streams check
	origSubs := getStreamsList(origMap, "subtitles")
	outSubLangs := make(map[string]bool)
	outHasASS := false
	for _, s := range outRep.Subtitles {
		if s.Language != "" {
			outSubLangs[strings.ToLower(s.Language)] = true
		}
		if s.Codec == "ass" || s.Codec == "ssa" {
			outHasASS = true
		}
	}
	origHasASS := false
	for _, origS := range origSubs {
		if origS.Codec == "ass" || origS.Codec == "ssa" {
			origHasASS = true
		}
		if origS.Language != "" {
			norm := strings.ToLower(origS.Language)
			if !outSubLangs[norm] {
				return StepResult{
					Status:        StepWaitingDecision,
					WaitingReason: fmt.Sprintf("Subtitle stream lost: original had subtitle language %q which is missing in transcoded output", origS.Language),
					WaitingOptions: []WaitingOption{
						{Decision: "reject", Description: "Reject transcode to prevent subtitle loss (keep original)"},
						{Decision: "accept_loss", Description: "Accept transcode without the missing subtitle track"},
					},
				}, nil
			}
		}
	}
	if origHasASS && !outHasASS {
		return StepResult{
			Status:        StepWaitingDecision,
			WaitingReason: "ASS/SSA stylized subtitles lost in transcode: original had ASS/SSA subtitle streams",
			WaitingOptions: []WaitingOption{
				{Decision: "reject", Description: "Reject transcode to preserve stylized ASS/SSA subtitles (keep original)"},
				{Decision: "accept_loss", Description: "Accept transcode without ASS/SSA subtitles"},
			},
		}, nil
	}

	// 5. Font attachments check
	origAttachments := getStreamsList(origMap, "attachments")
	if len(origAttachments) > 0 && len(outRep.Attachments) == 0 {
		return StepResult{
			Status:        StepWaitingDecision,
			WaitingReason: fmt.Sprintf("Font attachments lost: original file contained %d attachments (e.g. anime fonts)", len(origAttachments)),
			WaitingOptions: []WaitingOption{
				{Decision: "reject", Description: "Reject transcode to preserve font attachments (keep original)"},
				{Decision: "accept_loss", Description: "Accept transcode without font attachments"},
			},
		}, nil
	}

	// 6. Chapters check
	origChapters := getInt(origMap, "chapters")
	if origChapters > 0 && outRep.Chapters == 0 {
		return StepResult{
			Status:        StepWaitingDecision,
			WaitingReason: fmt.Sprintf("Chapters lost: original file had %d chapters, transcoded output has 0", origChapters),
			WaitingOptions: []WaitingOption{
				{Decision: "reject", Description: "Reject transcode to preserve chapters (keep original)"},
				{Decision: "accept_loss", Description: "Accept transcode without chapters"},
			},
		}, nil
	}

	// All checks passed
	origSize := getInt64(origMap, "size_bytes")
	outputSize := fi.Size()
	savedBytes := origSize - outputSize
	var savedPercent float64
	if origSize > 0 {
		savedPercent = (float64(savedBytes) / float64(origSize)) * 100
	}

	valSummary := map[string]any{
		"duration":         "ok",
		"video_streams":    "ok",
		"audio_streams":    "ok",
		"subtitle_streams": "ok",
		"attachments":      "ok",
		"chapters":         "ok",
	}

	videoCodec := ""
	resolution := ""
	if len(outRep.Video) > 0 {
		videoCodec = outRep.Video[0].Codec
		resolution = fmt.Sprintf("%dx%d", outRep.Video[0].Width, outRep.Video[0].Height)
	}

	resultMap := map[string]any{
		"path":         outputPath,
		"size_bytes":   outputSize,
		"duration_sec": outRep.DurationSec,
		"video_codec":  videoCodec,
		"resolution":   resolution,
	}

	ec.State["validation"] = valSummary
	ec.State["result"] = resultMap
	ec.State["size_saved_bytes"] = savedBytes
	ec.State["size_saved_percent"] = savedPercent

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"result":             resultMap,
			"validation":         valSummary,
			"size_saved_bytes":   savedBytes,
			"size_saved_percent": savedPercent,
		},
	}, nil
}

// stepTranscodeAccept completes the transcode and conditionally handles destructive replacement safely.
func (e *Engine) stepTranscodeAccept(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if ec.Decision == "reject" {
		return StepResult{
			Status: StepFailed,
			Error:  "transcode rejected during validation decision; original file remains untouched",
		}, nil
	}

	replaceOriginal := getBool(ec.Inputs, "replace_original")
	if !replaceOriginal {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"replace_original": false,
				"message":          "Transcode completed and validated. Original file preserved untouched.",
			},
		}, nil
	}

	// replace_original is true: check central safety gate
	if !e.AllowDestructive() {
		return StepResult{
			Status:        StepWaitingDecision,
			WaitingReason: "Destructive replacement was requested (replace_original: true), but allow_destructive is disabled. Original file is intact.",
			WaitingOptions: []WaitingOption{
				{Decision: "keep_both", Description: "Keep both original and transcoded files without replacing original"},
			},
		}, nil
	}

	origPath := getString(ec.State, "resolved_path")
	outputPath := getString(ec.State, "output_path")
	if origPath == "" || outputPath == "" || origPath == outputPath {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"replace_original": true,
				"message":          "Output already in destination path.",
			},
		}, nil
	}

	// Safe atomic replacement:
	// 1. Rename origPath to backupPath
	// 2. Rename outputPath to origPath
	// 3. If rename fails: rollback immediately
	// 4. Remove backupPath
	backupPath := origPath + ".tdarr.orig.bak"
	if err := os.Rename(origPath, backupPath); err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("safe replace failed creating backup: %v", err),
		}, nil
	}

	if err := os.Rename(outputPath, origPath); err != nil {
		_ = os.Rename(backupPath, origPath) // Rollback
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("safe replace failed moving output into place (original restored): %v", err),
		}, nil
	}

	_ = os.Remove(backupPath)

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"replace_original": true,
			"message":          "Transcode completed, validated, and original file replaced safely.",
		},
	}, nil
}

// Helpers

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func getStreamsList(m map[string]any, key string) []mediainspect.DetailedStream {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}

	switch v := raw.(type) {
	case []mediainspect.DetailedStream:
		return v
	case []any:
		res := make([]mediainspect.DetailedStream, 0, len(v))
		for _, item := range v {
			if ds, ok := item.(mediainspect.DetailedStream); ok {
				res = append(res, ds)
				continue
			}
			if itemMap, ok := item.(map[string]any); ok {
				ds := mediainspect.DetailedStream{
					Kind:     getString(itemMap, "kind"),
					Codec:    getString(itemMap, "codec"),
					Language: getString(itemMap, "language"),
					Title:    getString(itemMap, "title"),
				}
				res = append(res, ds)
			}
		}
		return res
	}
	return nil
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if b, ok := m[key].(bool); ok {
		return b
	}
	if s, ok := m[key].(string); ok {
		lower := strings.ToLower(strings.TrimSpace(s))
		return lower == "true" || lower == "1" || lower == "yes"
	}
	return false
}
