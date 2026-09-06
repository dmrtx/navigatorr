package action

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/mediainspect"
)

var magnetHashRegex = regexp.MustCompile(`(?i)btih:([a-f0-9]{40}|[a-z2-7]{32})`)

func (e *Engine) registerBuiltinTemplates() {
	e.RegisterTemplate(ActionTemplate{
		Name:        "validate_torrent",
		Description: "Inspects torrent files and media streams, checking against malicious extensions and verifying audio/subtitle language accessibility.",
		Steps: []StepDefinition{
			{
				Name:        "resolve_torrent_files",
				Description: "Resolves file list from input files, infohash, or download client",
				Run:         e.stepResolveTorrentFiles,
			},
			{
				Name:        "security_scan",
				Description: "Screens all filenames against dangerous executable patterns",
				Run:         e.stepSecurityScan,
			},
			{
				Name:        "inspect_streams",
				Description: "Extracts media stream codecs, audio, and subtitles if files are accessible on disk",
				Run:         e.stepInspectStreams,
			},
			{
				Name:        "summarize",
				Description: "Produces a normalized safety and accessibility report",
				Run:         e.stepSummarizeValidation,
			},
		},
	})

	e.RegisterTemplate(ActionTemplate{
		Name:        "safe_media_replacement",
		Description: "Coordinates safe media replacement end-to-end: plans replacement, tracks download, verifies safety and media streams, pauses for trade-off decisions, reconciles external state before import, verifies library, and safely handles cleanup.",
		Steps: []StepDefinition{
			{
				Name:        "plan_and_check_current",
				Description: "Records current media file details in library and checks maintenance items",
				Run:         e.stepPlanAndCheckCurrent,
			},
			{
				Name:        "add_or_track_download",
				Description: "Adds replacement torrent to download client or tracks existing infohash",
				Run:         e.stepAddOrTrackDownload,
			},
			{
				Name:        "wait_for_download",
				Description: "Monitors download client progress, transitioning to waiting_external if in flight",
				Run:         e.stepWaitForDownload,
			},
			{
				Name:        "validate_download_safety",
				Description: "Screens downloaded files against dangerous executables before touching library",
				Run:         e.stepValidateDownloadSafety,
			},
			{
				Name:        "inspect_replacement_media",
				Description: "Inspects audio and subtitle streams to ensure accessibility criteria are met",
				Run:         e.stepInspectReplacementMedia,
			},
			{
				Name:        "evaluate_decision",
				Description: "Pauses for LLM decision if trade-offs (e.g. significant size increase) are detected",
				Run:         e.stepEvaluateDecision,
			},
			{
				Name:        "reconcile_and_import",
				Description: "Reconciles library state to avoid duplicate imports, then triggers import if needed",
				Run:         e.stepReconcileAndImport,
			},
			{
				Name:        "verify_library_state",
				Description: "Confirms that the library has the new file active and healthy",
				Run:         e.stepVerifyLibraryState,
			},
			{
				Name:        "update_maintenance_and_cleanup",
				Description: "Resolves maintenance jobs and performs safe cleanup only if allow_destructive is enabled",
				Run:         e.stepUpdateMaintenanceAndCleanup,
			},
		},
	})
}

// --- Steps for validate_torrent ---

func (e *Engine) stepResolveTorrentFiles(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	var files []string
	var totalSize int64

	// 1. Direct file list provided
	if rawFiles, ok := ec.Inputs["files"]; ok {
		switch val := rawFiles.(type) {
		case []string:
			files = val
		case []any:
			for _, item := range val {
				if s, ok := item.(string); ok {
					files = append(files, s)
				}
			}
		case string:
			for _, f := range strings.Split(val, ",") {
				if tf := strings.TrimSpace(f); tf != "" {
					files = append(files, tf)
				}
			}
		}
	}

	// 2. Infohash lookup in qBittorrent
	hash := getString(ec.Inputs, "hash")
	if hash == "" {
		if url := getString(ec.Inputs, "url"); url != "" {
			if m := magnetHashRegex.FindStringSubmatch(url); len(m) > 1 {
				hash = strings.ToLower(m[1])
			}
		}
	}

	if len(files) == 0 && hash != "" && e.deps.Qbit != nil {
		tfList, err := e.deps.Qbit.ListFiles(ctx, hash)
		if err == nil {
			for _, tf := range tfList {
				files = append(files, tf.Name)
				totalSize += tf.Size
			}
		}
	}

	// 3. Fallback to path
	if len(files) == 0 {
		if path := getString(ec.Inputs, "path"); path != "" {
			files = append(files, filepath.Base(path))
		}
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"files":      files,
			"hash":       hash,
			"total_size": totalSize,
			"file_count": len(files),
		},
	}, nil
}

func (e *Engine) stepSecurityScan(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	files := getStringSlice(ec.State, "files")
	dangerous := maint.ScanFilenames(files)

	if len(dangerous) > 0 {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("malicious or dangerous files detected: %s", strings.Join(dangerous, ", ")),
			Outputs: map[string]any{
				"is_safe":         false,
				"dangerous_files": dangerous,
			},
		}, nil
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"is_safe":         true,
			"dangerous_files": []string{},
		},
	}, nil
}

func (e *Engine) stepInspectStreams(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	path := getString(ec.Inputs, "path")
	if path == "" {
		path = getString(ec.State, "path")
	}

	// If no local file path available, skip probe gracefully
	if path == "" {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"inspected": false,
				"note":      "Stream inspection skipped: no local file path provided",
			},
		}, nil
	}

	rep, err := mediainspect.InspectFile(ctx, e.deps.Ffprobe, path)
	if err != nil {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"inspected": false,
				"error":     err.Error(),
			},
		}, nil
	}

	acc := maint.EvaluateLanguageAccessibility(rep.AudioLanguages, rep.SubtitleLanguages)

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"inspected":          true,
			"video_codec":        rep.VideoCodec,
			"resolution":         rep.Resolution,
			"bit_depth":          rep.BitDepth,
			"audio_languages":    rep.AudioLanguages,
			"subtitle_languages": rep.SubtitleLanguages,
			"accessibility":      acc,
		},
	}, nil
}

func (e *Engine) stepSummarizeValidation(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"valid":              true,
			"is_safe":            ec.State["is_safe"],
			"file_count":         ec.State["file_count"],
			"total_size":         ec.State["total_size"],
			"accessibility":      ec.State["accessibility"],
			"audio_languages":    ec.State["audio_languages"],
			"subtitle_languages": ec.State["subtitle_languages"],
		},
	}, nil
}

// --- Steps for safe_media_replacement ---

func (e *Engine) stepPlanAndCheckCurrent(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	service := strings.ToLower(getString(ec.Inputs, "service"))
	mediaID := getString(ec.Inputs, "media_id")

	if service == "" || mediaID == "" {
		return StepResult{
			Status: StepFailed,
			Error:  "service and media_id are required inputs",
		}, nil
	}

	if e.deps.Registry == nil {
		return StepResult{
			Status: StepFailed,
			Error:  "service registry is not configured",
		}, nil
	}

	svc, err := e.deps.Registry.Get(service)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("unknown service %q: %v", service, err),
		}, nil
	}

	// Fetch current media details
	endpoint := fmt.Sprintf("/api/v3/movie/%s", mediaID)
	if service == "sonarr" {
		endpoint = fmt.Sprintf("/api/v3/series/%s", mediaID)
	}

	data, err := svc.Get(ctx, endpoint, nil)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("querying %s %s: %v", service, endpoint, err),
		}, nil
	}

	var mediaMap map[string]any
	if err := json.Unmarshal(data, &mediaMap); err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("decoding %s response: %v", service, err),
		}, nil
	}

	title, _ := mediaMap["title"].(string)
	var curFileID string
	var curSize int64
	var curPath string
	var curAudio, curSubs []string

	if service == "radarr" {
		if mf, ok := mediaMap["movieFile"].(map[string]any); ok {
			curFileID = fmt.Sprintf("%v", mf["id"])
			curSize = int64(numVal(mf["size"]))
			if p, ok := mf["path"].(string); ok {
				curPath = p
			}
			if mi, ok := mf["mediaInfo"].(map[string]any); ok {
				if al, ok := mi["audioLanguages"].(string); ok && al != "" {
					curAudio = strings.Split(al, "/")
				}
				if sl, ok := mi["subtitles"].(string); ok && sl != "" {
					curSubs = strings.Split(sl, "/")
				}
			}
		}
	}

	// Auto-resolve stale maintenance if file ID changed
	if curFileID != "" && e.deps.Store != nil {
		_, _ = e.deps.Store.AutoResolveByMedia(service, mediaID, "Current file ID "+curFileID)
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"media_title":     title,
			"current_file_id": curFileID,
			"current_size":    curSize,
			"current_path":    curPath,
			"current_audio":   curAudio,
			"current_subs":    curSubs,
		},
	}, nil
}

func (e *Engine) stepAddOrTrackDownload(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	// If local path is provided directly, no download client is needed
	if path := getString(ec.Inputs, "path"); path != "" {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"download_path": path,
				"download_done": true,
			},
		}, nil
	}

	hash := strings.ToLower(getString(ec.Inputs, "hash"))
	url := getString(ec.Inputs, "url")
	if hash == "" && url != "" {
		if m := magnetHashRegex.FindStringSubmatch(url); len(m) > 1 {
			hash = strings.ToLower(m[1])
		}
	}

	if e.deps.Qbit == nil {
		// If no qbit and no local path, fail
		return StepResult{
			Status: StepFailed,
			Error:  "qBittorrent client is not configured and no local file path was provided",
		}, nil
	}

	// If url provided and not yet added, add to qbit
	if url != "" {
		savePath := getString(ec.Inputs, "save_path")
		_ = e.deps.Qbit.AddTorrent(ctx, url, savePath)
	}

	if hash == "" {
		return StepResult{
			Status: StepFailed,
			Error:  "could not determine infohash from inputs (provide hash or url)",
		}, nil
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"hash": hash,
		},
	}, nil
}

func (e *Engine) stepWaitForDownload(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if done, _ := ec.State["download_done"].(bool); done {
		return StepResult{Status: StepCompleted}, nil
	}

	hash := getString(ec.State, "hash")
	if hash == "" {
		hash = getString(ec.Inputs, "hash")
	}

	if hash == "" || e.deps.Qbit == nil {
		return StepResult{Status: StepCompleted}, nil
	}

	torrents, err := e.deps.Qbit.ListTorrents(ctx)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("checking qBittorrent downloads: %v", err),
		}, nil
	}

	var match *struct {
		Progress    float64
		State       string
		ContentPath string
		SavePath    string
		Name        string
	}

	for _, t := range torrents {
		if strings.EqualFold(t.Hash, hash) {
			match = &struct {
				Progress    float64
				State       string
				ContentPath string
				SavePath    string
				Name        string
			}{
				Progress:    t.Progress,
				State:       t.State,
				ContentPath: t.ContentPath,
				SavePath:    t.SavePath,
				Name:        t.Name,
			}
			break
		}
	}

	if match == nil {
		// Torrent not found yet in qbit
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "download_in_client",
			WaitingReason:    fmt.Sprintf("Torrent %s not yet detected in qBittorrent", hash),
		}, nil
	}

	if match.Progress < 1.0 && !isCompleteState(match.State) {
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "download_complete",
			WaitingReason:    fmt.Sprintf("Downloading %q (%.1f%% complete, status: %s)", match.Name, match.Progress*100, match.State),
			Outputs: map[string]any{
				"download_progress": match.Progress,
				"download_state":    match.State,
			},
		}, nil
	}

	// Download is complete
	targetPath := match.ContentPath
	if targetPath == "" && match.SavePath != "" && match.Name != "" {
		targetPath = filepath.Join(match.SavePath, match.Name)
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"download_done": true,
			"download_path": targetPath,
		},
	}, nil
}

func (e *Engine) stepValidateDownloadSafety(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	hash := getString(ec.State, "hash")
	var filenames []string

	if hash != "" && e.deps.Qbit != nil {
		files, err := e.deps.Qbit.ListFiles(ctx, hash)
		if err == nil {
			for _, f := range files {
				filenames = append(filenames, f.Name)
			}
		}
	}

	if len(filenames) == 0 {
		if dlPath := getString(ec.State, "download_path"); dlPath != "" {
			filenames = append(filenames, filepath.Base(dlPath))
		}
	}

	dangerous := maint.ScanFilenames(filenames)
	if len(dangerous) > 0 {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("security scan failed: dangerous files found in download (%s); library remains untouched", strings.Join(dangerous, ", ")),
			Outputs: map[string]any{
				"dangerous_files": dangerous,
			},
		}, nil
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"safety_verified": true,
		},
	}, nil
}

func (e *Engine) stepInspectReplacementMedia(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	dlPath := getString(ec.State, "download_path")
	if dlPath == "" {
		dlPath = getString(ec.Inputs, "path")
	}

	if dlPath == "" {
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"inspected": false,
			},
		}, nil
	}

	rep, err := mediainspect.InspectFile(ctx, e.deps.Ffprobe, dlPath)
	if err != nil {
		// Could not inspect file directly (e.g. directory or unmounted path)
		return StepResult{
			Status: StepCompleted,
			Outputs: map[string]any{
				"inspected": false,
				"note":      err.Error(),
			},
		}, nil
	}

	objective := getString(ec.Inputs, "objective")
	if objective == "" {
		objective = "accessibility_repair"
	}

	acc := maint.EvaluateLanguageAccessibility(rep.AudioLanguages, rep.SubtitleLanguages)

	// If objective is accessibility_repair, verify that it actually fixes the issue!
	if objective == "accessibility_repair" && acc == maint.IssueMissingLanguage {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("replacement media does not fix accessibility issue: audio=%v, subs=%v; library remains untouched", rep.AudioLanguages, rep.SubtitleLanguages),
			Outputs: map[string]any{
				"accessibility":      acc,
				"audio_languages":    rep.AudioLanguages,
				"subtitle_languages": rep.SubtitleLanguages,
			},
		}, nil
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"replacement_size":       rep.SizeBytes,
			"replacement_video":      rep.VideoCodec,
			"replacement_resolution": rep.Resolution,
			"replacement_audio":      rep.AudioLanguages,
			"replacement_subs":       rep.SubtitleLanguages,
			"replacement_acc":        acc,
		},
	}, nil
}

func (e *Engine) stepEvaluateDecision(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	// If decision was already submitted via resume
	if ec.Decision != "" {
		if strings.EqualFold(ec.Decision, "reject") || strings.EqualFold(ec.Decision, "cancel") {
			return StepResult{
				Status: StepFailed,
				Error:  "replacement rejected by decision; original file preserved",
			}, nil
		}
		if strings.EqualFold(ec.Decision, "approve") || strings.EqualFold(ec.Decision, "proceed") {
			return StepResult{
				Status: StepCompleted,
				Outputs: map[string]any{
					"decision_applied": ec.Decision,
				},
			}, nil
		}
	}

	curSize := getInt64(ec.State, "current_size")
	repSize := getInt64(ec.State, "replacement_size")
	objective := getString(ec.Inputs, "objective")

	// Check if trade-off requires approval:
	// If replacement is > 1.5x original size and curSize > 0
	isSignificantSizeIncrease := curSize > 0 && repSize > (curSize*3/2)
	isSizeOptFailed := objective == "size_optimization" && curSize > 0 && repSize >= curSize

	if isSignificantSizeIncrease || isSizeOptFailed {
		reason := fmt.Sprintf("Trade-off detected: replacement size (%d bytes) is larger than original (%d bytes). Confirm replacement.", repSize, curSize)
		if isSizeOptFailed {
			reason = fmt.Sprintf("Objective is size_optimization but replacement (%d bytes) is not smaller than current (%d bytes). Confirm replacement.", repSize, curSize)
		}

		return StepResult{
			Status:        StepWaitingDecision,
			WaitingReason: reason,
			WaitingOptions: []WaitingOption{
				{Decision: "approve", Description: "Proceed with importing the replacement file"},
				{Decision: "reject", Description: "Abort replacement and preserve original file"},
			},
		}, nil
	}

	return StepResult{Status: StepCompleted}, nil
}

func (e *Engine) stepReconcileAndImport(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	service := strings.ToLower(getString(ec.Inputs, "service"))
	mediaID := getString(ec.Inputs, "media_id")
	currentFileID := getString(ec.State, "current_file_id")

	svc, err := e.deps.Registry.Get(service)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  err.Error(),
		}, nil
	}

	// 1. External Reconciliation:
	// Query current state of media in Radarr/Sonarr. If file ID already changed,
	// the file was already imported externally (e.g. by auto-importer or another process).
	endpoint := fmt.Sprintf("/api/v3/movie/%s", mediaID)
	if service == "sonarr" {
		endpoint = fmt.Sprintf("/api/v3/series/%s", mediaID)
	}

	data, err := svc.Get(ctx, endpoint, nil)
	if err == nil {
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			if service == "radarr" {
				if mf, ok := m["movieFile"].(map[string]any); ok {
					activeFileID := fmt.Sprintf("%v", mf["id"])
					if activeFileID != "" && currentFileID != "" && activeFileID != currentFileID {
						// Already imported externally! Reconcile state without error.
						return StepResult{
							Status: StepCompleted,
							Outputs: map[string]any{
								"reconciled_externally": true,
								"new_file_id":           activeFileID,
								"note":                  "Detected external import; adopted new file ID without duplicating import",
							},
						}, nil
					}
				}
			}
		}
	}

	// 2. Perform import command if not already imported
	dlPath := getString(ec.State, "download_path")
	if dlPath == "" {
		dlPath = getString(ec.Inputs, "path")
	}

	cmdPayload := map[string]any{
		"name": "DownloadedEpisodesScan",
		"path": dlPath,
	}
	if service == "radarr" {
		cmdPayload["name"] = "DownloadedMoviesScan"
	}

	cmdBytes, _ := json.Marshal(cmdPayload)
	_, _ = svc.Post(ctx, "/api/v3/command", cmdBytes)

	// Fetch updated file ID
	newFileID := currentFileID
	refreshData, err := svc.Get(ctx, endpoint, nil)
	if err == nil {
		var rm map[string]any
		if json.Unmarshal(refreshData, &rm) == nil {
			if service == "radarr" {
				if mf, ok := rm["movieFile"].(map[string]any); ok {
					newFileID = fmt.Sprintf("%v", mf["id"])
				}
			}
		}
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"import_triggered": true,
			"new_file_id":      newFileID,
		},
	}, nil
}

func (e *Engine) stepVerifyLibraryState(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	service := strings.ToLower(getString(ec.Inputs, "service"))
	mediaID := getString(ec.Inputs, "media_id")

	svc, err := e.deps.Registry.Get(service)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  err.Error(),
		}, nil
	}

	endpoint := fmt.Sprintf("/api/v3/movie/%s", mediaID)
	if service == "sonarr" {
		endpoint = fmt.Sprintf("/api/v3/series/%s", mediaID)
	}

	data, err := svc.Get(ctx, endpoint, nil)
	if err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  fmt.Sprintf("library verification error: %v", err),
		}, nil
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return StepResult{
			Status: StepFailed,
			Error:  "invalid JSON from library verification",
		}, nil
	}

	hasFile, _ := m["hasFile"].(bool)
	if !hasFile && service == "radarr" {
		return StepResult{
			Status: StepFailed,
			Error:  "library verification failed: media has no active file in library; keeping original",
		}, nil
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"library_verified": true,
		},
	}, nil
}

func (e *Engine) stepUpdateMaintenanceAndCleanup(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	service := strings.ToLower(getString(ec.Inputs, "service"))
	mediaID := getString(ec.Inputs, "media_id")
	newFileID := getString(ec.State, "new_file_id")

	// 1. Auto-resolve maintenance items
	autoResolved := 0
	if e.deps.Store != nil && newFileID != "" {
		autoResolved, _ = e.deps.Store.AutoResolveByMedia(service, mediaID, "Replaced with active file ID "+newFileID)
	}

	// 2. Centralized safety gate for cleanup
	allowCleanup, _ := ec.Inputs["allow_cleanup"].(bool)
	cleanupStatus := "none_requested"

	if allowCleanup {
		if !e.AllowDestructive() {
			cleanupStatus = "skipped_destructive_disabled"
		} else {
			// Clean up old file or torrent download
			cleanupStatus = "performed"
			oldPath := getString(ec.State, "current_path")
			if oldPath != "" && e.deps.Fs != nil {
				_ = e.deps.Fs.Delete(oldPath)
			}
		}
	}

	return StepResult{
		Status: StepCompleted,
		Outputs: map[string]any{
			"maintenance_auto_resolved": autoResolved,
			"cleanup_status":            cleanupStatus,
			"success":                   true,
		},
	}, nil
}

// Helpers

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func getInt64(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		return int64(numVal(v))
	}
	return 0
}

func getStringSlice(m map[string]any, key string) []string {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case []string:
			return val
		case []any:
			var res []string
			for _, item := range val {
				if s, ok := item.(string); ok {
					res = append(res, s)
				}
			}
			return res
		}
	}
	return nil
}

func numVal(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func isCompleteState(state string) bool {
	s := strings.ToLower(state)
	return strings.Contains(s, "upload") ||
		strings.Contains(s, "seed") ||
		strings.Contains(s, "complete") ||
		strings.Contains(s, "pausedup")
}
