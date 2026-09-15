package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode/resilience"
	"github.com/jakenesler/navigatorr/transcode/selector"
)

func (e *Engine) registerTranscodeBatchTemplate() {
	e.RegisterTemplate(ActionTemplate{
		Name:    "transcode_batch",
		Version: 1,
		Description: "Coordinates persistent batch transcoding for media libraries (Sonarr), resolving episodes, " +
			"applying deterministic auto-profile selection, respecting concurrency limits, and tracking per-item status in SQLite.",
		RequiredInputs: []string{"service", "series_id"},
		OptionalInputs: []string{
			"season",
			"profile",
			"replace_original",
			"dry_run",
			"media_type",
			"is_anime",
			"min_savings_percent",
			"paused",
			"max_size_increase_percent",
			"max_output_items",
			"max_items",
		},
		Destructive: false,
		Steps: []StepDefinition{
			{
				Name:        "resolve_and_inspect",
				Description: "Resolve series and episode files from Sonarr, deduplicate, inspect media streams, and select transcode profiles",
				Run:         e.stepTranscodeBatchResolve,
			},
			{
				Name:        "schedule_batch",
				Description: "Schedule and execute transcode jobs respecting max_parallel_jobs, handle worker_busy, and track per-item status",
				Run:         e.stepTranscodeBatchSchedule,
			},
		},
	})
}

func (e *Engine) stepTranscodeBatchResolve(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if getBool(ec.Inputs, "replace_original") {
		return StepResult{
			Status: StepFailed,
			Error:  "destructive replacement (replace_original: true) is not supported; transcoding is candidate-only and never modifies the original",
		}, nil
	}

	service := strings.ToLower(strings.TrimSpace(getString(ec.Inputs, "service")))
	if service == "" {
		return StepResult{Status: StepFailed, Error: "input 'service' is required"}, nil
	}
	if service != "sonarr" {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("service %q is not supported; only 'sonarr' is supported for transcode_batch", service)}, nil
	}

	var seriesID string
	if sVal, ok := ec.Inputs["series_id"]; ok && sVal != nil {
		seriesID = strings.TrimSpace(fmt.Sprintf("%v", sVal))
	}
	if seriesID == "" || seriesID == "<nil>" {
		return StepResult{Status: StepFailed, Error: "input 'series_id' is required"}, nil
	}

	var targetSeason *int
	if sVal, ok := ec.Inputs["season"]; ok && sVal != nil {
		sStr := strings.TrimSpace(fmt.Sprintf("%v", sVal))
		if sStr != "" && sStr != "<nil>" {
			sInt, err := strconv.Atoi(sStr)
			if err != nil {
				return StepResult{Status: StepFailed, Error: fmt.Sprintf("invalid season %q: %v", sStr, err)}, nil
			}
			targetSeason = &sInt
		}
	}

	if e.deps.Registry == nil {
		return StepResult{Status: StepFailed, Error: "arr service registry is required for transcode_batch"}, nil
	}
	svc, err := e.deps.Registry.Get(service)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("arr service %q not configured: %v", service, err)}, nil
	}
	if e.deps.Fs == nil {
		return StepResult{Status: StepFailed, Error: "filesystem resolver is required for transcode_batch"}, nil
	}
	if e.deps.Store == nil {
		return StepResult{Status: StepFailed, Error: "store is required for transcode_batch"}, nil
	}

	dryRun := getBool(ec.Inputs, "dry_run")

	// If items already exist in the database for this batch (e.g. on resume / restart), load them
	existingItems, err := e.deps.Store.ListTranscodeBatchItems(ec.InstanceID)
	if err == nil && len(existingItems) > 0 {
		seriesTitle := getString(ec.State, "series_title")
		isAnime := getBool(ec.State, "is_anime")
		return StepResult{
			Status:  StepCompleted,
			Outputs: buildBatchOutputs(ec.InstanceID, existingItems, seriesTitle, isAnime, dryRun, getMaxOutputItems(ec.Inputs)),
		}, nil
	}

	// 1. Fetch Series info
	seriesData, err := svc.Get(ctx, "/series/"+seriesID, nil)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to query Sonarr series %s: %v", seriesID, err)}, nil
	}
	var seriesObj map[string]any
	if err := json.Unmarshal(seriesData, &seriesObj); err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to parse Sonarr series %s response: %v", seriesID, err)}, nil
	}
	seriesTitle, _ := seriesObj["title"].(string)
	seriesType, _ := seriesObj["seriesType"].(string)
	isAnime := strings.EqualFold(seriesType, "anime")
	if !isAnime {
		if genres, ok := seriesObj["genres"].([]any); ok {
			for _, g := range genres {
				if strings.EqualFold(fmt.Sprintf("%v", g), "anime") {
					isAnime = true
					break
				}
			}
		}
	}
	if getBool(ec.Inputs, "is_anime") || strings.EqualFold(getString(ec.Inputs, "media_type"), "anime") {
		isAnime = true
	}

	ec.State["series_title"] = seriesTitle
	ec.State["series_type"] = seriesType
	ec.State["is_anime"] = isAnime
	ec.State["service"] = service
	ec.State["series_id"] = seriesID

	// 2. Fetch Episodes
	epData, err := svc.Get(ctx, "/episode", map[string]string{"seriesId": seriesID})
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to query Sonarr episodes for series %s: %v", seriesID, err)}, nil
	}
	var epList []map[string]any
	if err := json.Unmarshal(epData, &epList); err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to parse Sonarr episodes for series %s: %v", seriesID, err)}, nil
	}

	// 3. Fetch Episode Files
	epFileData, err := svc.Get(ctx, "/episodefile", map[string]string{"seriesId": seriesID})
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to query Sonarr episode files for series %s: %v", seriesID, err)}, nil
	}
	var epFileList []map[string]any
	if err := json.Unmarshal(epFileData, &epFileList); err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to parse Sonarr episode files for series %s: %v", seriesID, err)}, nil
	}

	fileMap := make(map[int64]map[string]any)
	for _, ef := range epFileList {
		id := int64(numVal(ef["id"]))
		if id > 0 {
			fileMap[id] = ef
		}
	}

	type episodeMeta struct {
		seasonNumber  int
		episodeNumber int
		title         string
	}
	episodesByFile := make(map[int64][]episodeMeta)
	var fileIDs []int64
	seenFile := make(map[int64]bool)

	for _, ep := range epList {
		hasFile, _ := ep["hasFile"].(bool)
		if !hasFile {
			continue
		}
		fID := int64(numVal(ep["episodeFileId"]))
		if fID <= 0 {
			continue
		}
		seasonNum := int(numVal(ep["seasonNumber"]))
		if targetSeason != nil && seasonNum != *targetSeason {
			continue
		}
		if !seenFile[fID] {
			seenFile[fID] = true
			fileIDs = append(fileIDs, fID)
		}
		epNum := int(numVal(ep["episodeNumber"]))
		epTitle, _ := ep["title"].(string)
		episodesByFile[fID] = append(episodesByFile[fID], episodeMeta{
			seasonNumber:  seasonNum,
			episodeNumber: epNum,
			title:         epTitle,
		})
	}
	sort.Slice(fileIDs, func(i, j int) bool {
		return fileIDs[i] < fileIDs[j]
	})

	requestedProfile := strings.TrimSpace(getString(ec.Inputs, "profile"))
	if requestedProfile == "" {
		requestedProfile = "auto"
	}
	mediaType := "tv"
	if isAnime {
		mediaType = "anime"
	}
	if mt := getString(ec.Inputs, "media_type"); mt != "" {
		mediaType = mt
	}
	minSavings := 0.0
	if e.deps.Config != nil {
		minSavings = e.deps.Config.Transcode.MinSavingsPercent
	}
	if s := getFloat(ec.Inputs, "min_savings_percent"); s > 0 {
		minSavings = s
	}

	var batchItems []store.TranscodeBatchItem
	for _, fID := range fileIDs {
		itemKey := fmt.Sprintf("epfile-%d", fID)
		existing, _ := e.deps.Store.GetTranscodeBatchItem(ec.InstanceID, itemKey)
		if existing != nil {
			batchItems = append(batchItems, *existing)
			continue
		}

		ef, ok := fileMap[fID]
		if !ok {
			continue
		}
		rawPath, _ := ef["path"].(string)
		if rawPath == "" {
			continue
		}
		cleanPath, err := e.deps.Fs.ResolveRead(rawPath)
		if err != nil {
			item := store.TranscodeBatchItem{
				BatchID:      ec.InstanceID,
				ItemKey:      itemKey,
				FilePath:     rawPath,
				DisplayLabel: filepath.Base(rawPath),
				Decision:     "review",
				Status:       "review",
				Reasons:      []string{fmt.Sprintf("path outside allowed read roots: %v", err)},
			}
			_ = e.deps.Store.CreateTranscodeBatchItem(item)
			batchItems = append(batchItems, item)
			continue
		}

		episodes := episodesByFile[fID]
		sort.Slice(episodes, func(i, j int) bool {
			if episodes[i].seasonNumber != episodes[j].seasonNumber {
				return episodes[i].seasonNumber < episodes[j].seasonNumber
			}
			return episodes[i].episodeNumber < episodes[j].episodeNumber
		})

		episodeInfo := ""
		displayLabel := filepath.Base(cleanPath)
		if len(episodes) > 0 {
			if len(episodes) == 1 {
				episodeInfo = fmt.Sprintf("S%02dE%02d", episodes[0].seasonNumber, episodes[0].episodeNumber)
			} else if episodes[0].seasonNumber == episodes[len(episodes)-1].seasonNumber {
				episodeInfo = fmt.Sprintf("S%02dE%02d-E%02d", episodes[0].seasonNumber, episodes[0].episodeNumber, episodes[len(episodes)-1].episodeNumber)
			} else {
				episodeInfo = fmt.Sprintf("S%02dE%02d-S%02dE%02d", episodes[0].seasonNumber, episodes[0].episodeNumber, episodes[len(episodes)-1].seasonNumber, episodes[len(episodes)-1].episodeNumber)
			}
			if seriesTitle != "" {
				displayLabel = fmt.Sprintf("%s - %s", seriesTitle, episodeInfo)
			}
		}

		if _, err := os.Stat(cleanPath); err != nil {
			item := store.TranscodeBatchItem{
				BatchID:      ec.InstanceID,
				ItemKey:      itemKey,
				FilePath:     cleanPath,
				DisplayLabel: displayLabel,
				EpisodeInfo:  episodeInfo,
				Decision:     "review",
				Status:       "review",
				Reasons:      []string{fmt.Sprintf("media file inaccessible: %v", err)},
			}
			_ = e.deps.Store.CreateTranscodeBatchItem(item)
			batchItems = append(batchItems, item)
			continue
		}

		rep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, cleanPath)
		if err != nil || !rep.Probed {
			reason := "ffprobe did not produce a trustworthy inspection"
			if err != nil {
				reason = fmt.Sprintf("mediainspect failed: %v", err)
			}
			item := store.TranscodeBatchItem{
				BatchID:      ec.InstanceID,
				ItemKey:      itemKey,
				FilePath:     cleanPath,
				DisplayLabel: displayLabel,
				EpisodeInfo:  episodeInfo,
				Decision:     "review",
				Status:       "review",
				Reasons:      []string{reason},
			}
			_ = e.deps.Store.CreateTranscodeBatchItem(item)
			batchItems = append(batchItems, item)
			continue
		}

		var itemDecision, itemProfile, itemStatus string
		var itemReasons []string

		if strings.EqualFold(requestedProfile, "auto") {
			selInput := selector.Input{
				Report:            rep,
				MediaType:         mediaType,
				IsAnime:           isAnime,
				MinSavingsPercent: minSavings,
			}
			res := selector.Select(selInput)
			itemDecision = string(res.Decision)
			itemProfile = res.Profile
			itemReasons = res.Reasons
			switch res.Decision {
			case selector.DecisionSkip:
				itemStatus = "skip"
			case selector.DecisionReview:
				itemStatus = "review"
			case selector.DecisionTranscode:
				itemStatus = "queued"
			default:
				itemStatus = "queued"
			}
		} else {
			itemDecision = "transcode"
			itemProfile = requestedProfile
			itemReasons = []string{"explicit profile requested"}
			itemStatus = "queued"
		}

		item := store.TranscodeBatchItem{
			BatchID:      ec.InstanceID,
			ItemKey:      itemKey,
			FilePath:     cleanPath,
			DisplayLabel: displayLabel,
			EpisodeInfo:  episodeInfo,
			Decision:     itemDecision,
			Profile:      itemProfile,
			Reasons:      itemReasons,
			Status:       itemStatus,
			Attempts:     0,
		}
		_ = e.deps.Store.CreateTranscodeBatchItem(item)
		batchItems = append(batchItems, item)
	}

	outputs := buildBatchOutputs(ec.InstanceID, batchItems, seriesTitle, isAnime, dryRun, getMaxOutputItems(ec.Inputs))
	return StepResult{
		Status:  StepCompleted,
		Outputs: outputs,
	}, nil
}

func (e *Engine) stepTranscodeBatchSchedule(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	dryRun := getBool(ec.Inputs, "dry_run")
	seriesTitle := getString(ec.State, "series_title")
	isAnime := getBool(ec.State, "is_anime")

	items, err := e.deps.Store.ListTranscodeBatchItems(ec.InstanceID)
	if err != nil {
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to list batch items: %v", err)}, nil
	}

	// In dry run, resolution and auto-selection are complete; no jobs are scheduled.
	if dryRun {
		return StepResult{
			Status:  StepCompleted,
			Outputs: buildBatchOutputs(ec.InstanceID, items, seriesTitle, isAnime, dryRun, getMaxOutputItems(ec.Inputs)),
		}, nil
	}

	if strings.EqualFold(ec.Decision, "resume") {
		ec.Decision = ""
		delete(ec.Inputs, "paused")
		ec.Inputs["paused"] = false
		ec.State["paused"] = false
	}

	// Handle pause / cancellation semantics
	if strings.EqualFold(ec.Decision, "pause") || getBool(ec.Inputs, "paused") {
		ec.Decision = ""
		return StepResult{
			Status:        StepWaitingDecision,
			WaitingReason: "Transcode batch paused by request",
			WaitingOptions: []WaitingOption{
				{Decision: "resume", Description: "Resume transcode batch"},
				{Decision: "cancel", Description: "Cancel remaining queued and waiting items (active remote jobs are not stopped)"},
			},
			Outputs: buildBatchOutputs(ec.InstanceID, items, seriesTitle, isAnime, dryRun, getMaxOutputItems(ec.Inputs)),
		}, nil
	}

	if strings.EqualFold(ec.Decision, "cancel") {
		ec.Decision = ""
		for i := range items {
			if items[i].Status == "queued" || items[i].Status == "waiting_for_slot" {
				items[i].Status = "failed"
				items[i].Error = "cancelled by user decision"
				_ = e.deps.Store.UpdateTranscodeBatchItem(items[i])
			}
		}
		items, _ = e.deps.Store.ListTranscodeBatchItems(ec.InstanceID)
		outputs := buildBatchOutputs(ec.InstanceID, items, seriesTitle, isAnime, dryRun, getMaxOutputItems(ec.Inputs))
		outputs["note"] = "Queued and waiting items were cancelled. Active remote transcode jobs are not stopped and remain running on workers."

		hasRunning := false
		for _, it := range items {
			if it.Status == "running" {
				hasRunning = true
				break
			}
		}
		if hasRunning {
			return StepResult{
				Status:           StepWaitingExternal,
				WaitingCondition: "transcode_running",
				WaitingReason:    "Remaining queued items cancelled; waiting for active remote transcode job(s) to finish (active jobs not stopped)",
				Outputs:          outputs,
			}, nil
		}
		return StepResult{
			Status:  StepCompleted,
			Outputs: outputs,
		}, nil
	}

	// Forward user decision to child action if an item is in waiting_decision
	if ec.Decision != "" {
		decisionToForward := ec.Decision
		ec.Decision = ""
		for i := range items {
			if items[i].Status == "waiting_decision" && items[i].ChildActionID != "" {
				resumedRes, resumedErr := e.Resume(ctx, items[i].ChildActionID, decisionToForward, map[string]any{"surface_worker_busy": true})
				if resumedErr == nil && resumedRes != nil {
					switch resumedRes.Status {
					case StatusCompleted:
						items[i].Status = "completed"
						cand := getString(resumedRes.Outputs, "candidate_path")
						if cand == "" {
							cand = getString(resumedRes.Outputs, "output_path")
						}
						items[i].CandidatePath = cand
						items[i].Error = ""
						if items[i].Attempts == 0 {
							items[i].Attempts = 1
						}
					case StatusFailed:
						items[i].Status = "failed"
						items[i].Error = resumedRes.Error
						if items[i].Attempts == 0 {
							items[i].Attempts = 1
						}
					case StatusWaitingDecision:
						items[i].Status = "waiting_decision"
					case StatusWaitingExternal:
						if resumedRes.WaitingCondition == "worker_busy" {
							items[i].Status = "waiting_for_slot"
						} else {
							items[i].Status = "running"
						}
					default:
						items[i].Status = "running"
					}
					_ = e.deps.Store.UpdateTranscodeBatchItem(items[i])
				}
				break
			}
		}
	}

	// Recover existing child actions across all statuses to ensure no duplicate submits
	for i := range items {
		it := &items[i]
		childIdempotencyKey := fmt.Sprintf("batch-%s-%s", ec.InstanceID, it.ItemKey)

		if it.ChildActionID == "" {
			if existingChild, err := e.deps.Store.FindActionByIdempotencyKey("transcode_media", childIdempotencyKey); err == nil && existingChild != nil {
				it.ChildActionID = existingChild.ID
			}
		}

		if it.ChildActionID != "" {
			existingChild, err := e.deps.Store.GetActionInstance(it.ChildActionID)
			if err == nil && existingChild != nil {
				if it.JobID == "" {
					var state map[string]any
					_ = json.Unmarshal([]byte(existingChild.StateJSON), &state)
					if j := getString(state, "job_id"); j != "" {
						it.JobID = j
					}
				}

				switch existingChild.Status {
				case StatusCompleted:
					if it.Status != "completed" {
						it.Status = "completed"
						var out map[string]any
						_ = json.Unmarshal([]byte(existingChild.OutputsJSON), &out)
						cand := getString(out, "candidate_path")
						if cand == "" {
							cand = getString(out, "output_path")
						}
						it.CandidatePath = cand
						it.Error = ""
						if it.Attempts == 0 {
							it.Attempts = 1
						}
						_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
					}
				case StatusFailed:
					var out map[string]any
					_ = json.Unmarshal([]byte(existingChild.OutputsJSON), &out)
					isBusy := getString(out, "failure_classification") == string(resilience.WorkerBusy) ||
						strings.Contains(strings.ToLower(existingChild.ErrorJSON), "worker busy")
					if isBusy {
						if it.Status != "waiting_for_slot" {
							it.Status = "waiting_for_slot"
							_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
						}
					} else if it.Status != "failed" {
						it.Status = "failed"
						it.Error = existingChild.ErrorJSON
						if it.Attempts == 0 {
							it.Attempts = 1
						}
						_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
					}
				case StatusWaitingDecision:
					if it.Status != "waiting_decision" {
						it.Status = "waiting_decision"
						_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
					}
				case StatusWaitingExternal:
					if existingChild.WaitingCondition == "worker_busy" {
						if it.Status != "waiting_for_slot" {
							it.Status = "waiting_for_slot"
							_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
						}
					} else {
						if it.Status != "running" {
							it.Status = "running"
							_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
						}
					}
				case StatusRunning, StatusPending:
					if it.Status != "running" {
						it.Status = "running"
						_ = e.deps.Store.UpdateTranscodeBatchItem(*it)
					}
				}
			}
		}
	}

	maxParallel := 1
	if e.deps.Config != nil && e.deps.Config.Transcode.MaxParallelJobs > 0 {
		maxParallel = e.deps.Config.Transcode.MaxParallelJobs
	}

	// PR3 note: the worker now owns an authoritative durable queue
	// (transcode submits persist as queued instead of returning "worker busy"),
	// so this loop's maxParallel gating is LOCAL action fan-out throttling
	// only (bounding concurrent child-action execution engine-side) and must
	// not be confused with remote-capacity admission. No remote-capacity probe
	// is performed here; "worker busy" handling below is retained solely for
	// backward compatibility (legacy workers) and benchmarks, which keep
	// busy-409 submission semantics under the same global ceiling. Phase-4
	// retry semantics are untouched.

	// First, check/advance any items that are already running
	for i := range items {
		it := &items[i]
		if it.Status == "running" {
			_, err := e.processBatchItem(ctx, it, ec, seriesTitle, isAnime)
			if err != nil && ctx.Err() != nil {
				return StepResult{Status: StepFailed, Error: ctx.Err().Error()}, nil
			}
		}
	}

	var workerBusyEncountered bool

	// Loop to dispatch items while slots are available and pending items exist
	for {
		if ctx.Err() != nil {
			return StepResult{Status: StepFailed, Error: ctx.Err().Error()}, nil
		}
		if workerBusyEncountered {
			break
		}

		items, err = e.deps.Store.ListTranscodeBatchItems(ec.InstanceID)
		if err != nil {
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("failed to list batch items: %v", err)}, nil
		}

		activeCount := 0
		hasWaitingDecision := false
		for _, it := range items {
			if it.Status == "running" {
				activeCount++
			} else if it.Status == "waiting_decision" {
				hasWaitingDecision = true
			}
		}

		if hasWaitingDecision {
			break
		}

		availableSlots := maxParallel - activeCount
		if availableSlots <= 0 {
			break
		}

		// Find next batch of items (both queued and waiting_for_slot are candidates)
		var pendingIndices []int
		for i := range items {
			if items[i].Status == "queued" || items[i].Status == "waiting_for_slot" {
				pendingIndices = append(pendingIndices, i)
			}
		}

		if len(pendingIndices) == 0 {
			break
		}

		toSchedule := pendingIndices
		if len(toSchedule) > availableSlots {
			toSchedule = toSchedule[:availableSlots]
		}

		if len(toSchedule) == 1 {
			it := &items[toSchedule[0]]
			busy, err := e.processBatchItem(ctx, it, ec, seriesTitle, isAnime)
			if err != nil && ctx.Err() != nil {
				return StepResult{Status: StepFailed, Error: ctx.Err().Error()}, nil
			}
			if busy {
				workerBusyEncountered = true
				break
			}
		} else {
			sem := make(chan struct{}, availableSlots)
			var wg sync.WaitGroup
			var busyMu sync.Mutex

			cancelled := false
			for _, idx := range toSchedule {
				it := &items[idx]
				busyMu.Lock()
				alreadyBusy := workerBusyEncountered
				busyMu.Unlock()
				if alreadyBusy {
					break
				}

				select {
				case <-ctx.Done():
					cancelled = true
					break
				case sem <- struct{}{}:
				}
				if cancelled {
					break
				}

				wg.Add(1)
				go func(item *store.TranscodeBatchItem) {
					defer wg.Done()
					defer func() { <-sem }()

					busy, _ := e.processBatchItem(ctx, item, ec, seriesTitle, isAnime)
					if busy {
						busyMu.Lock()
						workerBusyEncountered = true
						busyMu.Unlock()
					}
				}(it)
			}
			wg.Wait()

			if cancelled && ctx.Err() != nil {
				return StepResult{Status: StepFailed, Error: ctx.Err().Error()}, nil
			}
			if workerBusyEncountered {
				break
			}
		}
	}

	latest, _ := e.deps.Store.ListTranscodeBatchItems(ec.InstanceID)
	outLimit := getMaxOutputItems(ec.Inputs)
	batchOutputs := buildBatchOutputs(ec.InstanceID, latest, seriesTitle, isAnime, dryRun, outLimit)

	// Check if any item is waiting for decision
	for _, it := range latest {
		if it.Status == "waiting_decision" {
			reason := fmt.Sprintf("Item %s waiting for decision", it.ItemKey)
			options := []WaitingOption{
				{Decision: "approve", Description: "Approve transcode result"},
				{Decision: "reject", Description: "Reject transcode result"},
			}
			if it.ChildActionID != "" {
				if childInst, _ := e.deps.Store.GetActionInstance(it.ChildActionID); childInst != nil {
					if childInst.WaitingReason != "" {
						reason = childInst.WaitingReason
					}
					var childOpts []WaitingOption
					_ = json.Unmarshal([]byte(childInst.WaitingOptionsJSON), &childOpts)
					if len(childOpts) > 0 {
						options = childOpts
					}
				}
			}
			return StepResult{
				Status:         StepWaitingDecision,
				WaitingReason:  reason,
				WaitingOptions: options,
				Outputs:        batchOutputs,
			}, nil
		}
	}

	// Check if worker busy occurred or any item is waiting for slot
	waitingForSlotCount := 0
	for _, it := range latest {
		if it.Status == "waiting_for_slot" {
			waitingForSlotCount++
		}
	}
	if workerBusyEncountered || waitingForSlotCount > 0 {
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "worker_busy",
			WaitingReason:    fmt.Sprintf("Worker busy; %d item(s) waiting for transcode slot", waitingForSlotCount),
			Outputs:          batchOutputs,
		}, nil
	}

	// Check if any item is running
	runningCount := 0
	for _, it := range latest {
		if it.Status == "running" {
			runningCount++
		}
	}
	if runningCount > 0 {
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "transcode_running",
			WaitingReason:    fmt.Sprintf("%d transcode job(s) in progress", runningCount),
			Outputs:          batchOutputs,
		}, nil
	}

	// Check if any item is still queued
	queuedCount := 0
	for _, it := range latest {
		if it.Status == "queued" {
			queuedCount++
		}
	}
	if queuedCount > 0 {
		return StepResult{
			Status:           StepWaitingExternal,
			WaitingCondition: "queued",
			WaitingReason:    fmt.Sprintf("%d item(s) queued for transcode", queuedCount),
			Outputs:          batchOutputs,
		}, nil
	}

	return StepResult{
		Status:  StepCompleted,
		Outputs: batchOutputs,
	}, nil
}

func (e *Engine) processBatchItem(ctx context.Context, item *store.TranscodeBatchItem, ec *ExecutionContext, seriesTitle string, isAnime bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	mediaType := "tv"
	if isAnime {
		mediaType = "anime"
	}
	if mt := getString(ec.Inputs, "media_type"); mt != "" {
		mediaType = mt
	}
	minSavings := 0.0
	if e.deps.Config != nil {
		minSavings = e.deps.Config.Transcode.MinSavingsPercent
	}
	if s := getFloat(ec.Inputs, "min_savings_percent"); s > 0 {
		minSavings = s
	}

	maxSizeIncrease := 0.0
	if val, ok := ec.Inputs["max_size_increase_percent"]; ok && val != nil {
		maxSizeIncrease = getFloat(ec.Inputs, "max_size_increase_percent")
	}

	childInputs := map[string]any{
		"path":                      item.FilePath,
		"profile":                   item.Profile,
		"replace_original":          false,
		"media_type":                mediaType,
		"is_anime":                  isAnime,
		"min_savings_percent":       minSavings,
		"surface_worker_busy":       true,
		"max_size_increase_percent": maxSizeIncrease,
	}
	childIdempotencyKey := fmt.Sprintf("batch-%s-%s", ec.InstanceID, item.ItemKey)

	// Stable child action lookup across all statuses to prevent duplicate submits across restarts
	if item.ChildActionID == "" {
		if existing, err := e.deps.Store.FindActionByIdempotencyKey("transcode_media", childIdempotencyKey); err == nil && existing != nil {
			item.ChildActionID = existing.ID
		}
	}

	item.Status = "running"
	_ = e.deps.Store.UpdateTranscodeBatchItem(*item)

	var childRes *ActionResult
	var childErr error

	if item.ChildActionID != "" {
		existingChild, _ := e.deps.Store.GetActionInstance(item.ChildActionID)
		if existingChild != nil {
			switch existingChild.Status {
			case StatusWaitingExternal:
				childRes, childErr = e.Resume(ctx, existingChild.ID, "", map[string]any{"surface_worker_busy": true})
			case StatusWaitingDecision:
				tmpl, _ := e.GetTemplate("transcode_media")
				childEC := parseExecutionContext(existingChild, e)
				childRes = buildActionResult(existingChild, len(tmpl.Steps), childEC)
			case StatusCompleted, StatusFailed:
				tmpl, _ := e.GetTemplate("transcode_media")
				childEC := parseExecutionContext(existingChild, e)
				childRes = buildActionResult(existingChild, len(tmpl.Steps), childEC)
			default:
				childRes, childErr = e.Resume(ctx, existingChild.ID, "", map[string]any{"surface_worker_busy": true})
			}
		} else {
			childRes, childErr = e.Run(ctx, "transcode_media", childInputs, childIdempotencyKey)
		}
	} else {
		childRes, childErr = e.Run(ctx, "transcode_media", childInputs, childIdempotencyKey)
	}

	// If child action was already in StatusWaitingExternal when Run found it via idempotency key, resume it now
	if childRes != nil && childRes.Status == StatusWaitingExternal && childRes.ID != "" && item.ChildActionID == "" {
		item.ChildActionID = childRes.ID
		resumedRes, resumedErr := e.Resume(ctx, childRes.ID, "", map[string]any{"surface_worker_busy": true})
		if resumedErr == nil && resumedRes != nil {
			childRes = resumedRes
		} else if resumedErr != nil {
			childErr = resumedErr
		}
	}

	if childRes != nil && item.ChildActionID == "" {
		item.ChildActionID = childRes.ID
	}

	var jobID string
	if childRes != nil {
		if j := getString(childRes.Outputs, "job_id"); j != "" {
			jobID = j
		} else if j := getString(childRes.Outputs, "external_reference"); j != "" {
			jobID = j
		}
	}
	if jobID == "" && item.ChildActionID != "" {
		if inst, _ := e.deps.Store.GetActionInstance(item.ChildActionID); inst != nil {
			var state map[string]any
			_ = json.Unmarshal([]byte(inst.StateJSON), &state)
			if j := getString(state, "job_id"); j != "" {
				jobID = j
			}
			if jobID == "" {
				var out map[string]any
				_ = json.Unmarshal([]byte(inst.OutputsJSON), &out)
				if j := getString(out, "job_id"); j != "" {
					jobID = j
				} else if j := getString(out, "external_reference"); j != "" {
					jobID = j
				}
			}
		}
	}
	if jobID != "" {
		item.JobID = jobID
	}

	isWorkerBusy := false
	if childRes != nil {
		if childRes.Status == StatusWaitingExternal && childRes.WaitingCondition == "worker_busy" {
			isWorkerBusy = true
		} else if childRes.Status == StatusFailed && (getString(childRes.Outputs, "failure_classification") == string(resilience.WorkerBusy) || strings.Contains(strings.ToLower(childRes.Error), "worker busy")) {
			isWorkerBusy = true
		}
	}

	if isWorkerBusy {
		item.Status = "waiting_for_slot"
		// Worker busy does NOT increment attempts or consume retry budget
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return true, nil
	}

	if childErr != nil {
		item.Status = "failed"
		item.Error = childErr.Error()
		if item.Attempts == 0 {
			item.Attempts = 1
		}
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil
	}

	if childRes == nil {
		item.Status = "failed"
		item.Error = "child action returned nil result"
		if item.Attempts == 0 {
			item.Attempts = 1
		}
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil
	}

	switch childRes.Status {
	case StatusCompleted:
		item.Status = "completed"
		cand := getString(childRes.Outputs, "candidate_path")
		if cand == "" {
			cand = getString(childRes.Outputs, "output_path")
		}
		item.CandidatePath = cand
		item.Error = ""
		if item.Attempts == 0 {
			item.Attempts = 1
		}
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil

	case StatusFailed:
		item.Status = "failed"
		errStr := childRes.Error
		if errStr == "" && childRes.Outputs != nil {
			errStr = fmt.Sprintf("%v", childRes.Outputs["error"])
		}
		item.Error = errStr
		if item.Attempts == 0 {
			item.Attempts = 1
		}
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil

	case StatusWaitingDecision:
		item.Status = "waiting_decision"
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil

	case StatusWaitingExternal:
		if isWorkerBusy {
			item.Status = "waiting_for_slot"
		} else {
			// Normal child waiting_external => running!
			item.Status = "running"
		}
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil

	case StatusRunning, StatusPending:
		item.Status = "running"
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil

	default:
		item.Status = "running"
		_ = e.deps.Store.UpdateTranscodeBatchItem(*item)
		return false, nil
	}
}

// DefaultMaxBatchOutputItems defines the deterministic bound on per-item summaries returned in batch action outputs.
const (
	DefaultMaxBatchOutputItems = 25
	MaxBatchOutputItems        = 100
)

func getMaxOutputItems(inputs map[string]any) int {
	limit := DefaultMaxBatchOutputItems
	if m := getInt(inputs, "max_output_items"); m > 0 {
		limit = m
	} else if m := getInt(inputs, "max_items"); m > 0 {
		limit = m
	}
	if limit > MaxBatchOutputItems {
		limit = MaxBatchOutputItems
	}
	if limit < 1 {
		limit = DefaultMaxBatchOutputItems
	}
	return limit
}

// TranscodeBatchItemSummary provides a bounded, serializable summary of a batch item including diagnostics and job traceability.
type TranscodeBatchItemSummary struct {
	ItemKey       string   `json:"item_key"`
	FilePath      string   `json:"file_path"`
	DisplayLabel  string   `json:"display_label"`
	EpisodeInfo   string   `json:"episode_info,omitempty"`
	Decision      string   `json:"decision"`
	Profile       string   `json:"profile,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
	Status        string   `json:"status"`
	Attempts      int      `json:"attempts"`
	ChildActionID string   `json:"child_action_id,omitempty"`
	JobID         string   `json:"job_id,omitempty"`
	CandidatePath string   `json:"candidate_path,omitempty"`
	Error         string   `json:"error,omitempty"`
}

func buildBatchOutputs(batchID string, items []store.TranscodeBatchItem, seriesTitle string, isAnime, dryRun bool, maxLimit ...int) map[string]any {
	limit := DefaultMaxBatchOutputItems
	if len(maxLimit) > 0 && maxLimit[0] > 0 {
		limit = maxLimit[0]
	}
	if limit > MaxBatchOutputItems {
		limit = MaxBatchOutputItems
	}
	if limit < 1 {
		limit = DefaultMaxBatchOutputItems
	}

	counts := map[string]int{
		"total":            len(items),
		"queued":           0,
		"transcode":        0,
		"skip":             0,
		"review":           0,
		"waiting_for_slot": 0,
		"waiting_decision": 0,
		"running":          0,
		"completed":        0,
		"failed":           0,
	}

	for _, it := range items {
		if it.Decision == "transcode" {
			counts["transcode"]++
		}
		switch it.Status {
		case "queued":
			counts["queued"]++
		case "skip":
			counts["skip"]++
		case "review":
			counts["review"]++
		case "waiting_for_slot":
			counts["waiting_for_slot"]++
		case "waiting_decision":
			counts["waiting_decision"]++
		case "running":
			counts["running"]++
		case "completed":
			counts["completed"]++
		case "failed":
			counts["failed"]++
		}
	}

	totalItems := len(items)
	boundedLimit := limit
	if boundedLimit > totalItems {
		boundedLimit = totalItems
	}
	bounded := make([]TranscodeBatchItemSummary, 0, boundedLimit)
	for i := 0; i < boundedLimit; i++ {
		it := items[i]
		bounded = append(bounded, TranscodeBatchItemSummary{
			ItemKey:       it.ItemKey,
			FilePath:      it.FilePath,
			DisplayLabel:  it.DisplayLabel,
			EpisodeInfo:   it.EpisodeInfo,
			Decision:      it.Decision,
			Profile:       it.Profile,
			Reasons:       it.Reasons,
			Status:        it.Status,
			Attempts:      it.Attempts,
			ChildActionID: it.ChildActionID,
			JobID:         it.JobID,
			CandidatePath: it.CandidatePath,
			Error:         it.Error,
		})
	}
	truncated := totalItems > len(bounded)

	return map[string]any{
		"batch_id":         batchID,
		"series_title":     seriesTitle,
		"is_anime":         isAnime,
		"dry_run":          dryRun,
		"counts":           counts,
		"total":            counts["total"],
		"queued":           counts["queued"],
		"transcode":        counts["transcode"],
		"skip":             counts["skip"],
		"review":           counts["review"],
		"waiting_for_slot": counts["waiting_for_slot"],
		"waiting_decision": counts["waiting_decision"],
		"running":          counts["running"],
		"completed":        counts["completed"],
		"failed":           counts["failed"],
		"total_items":      totalItems,
		"returned_items":   len(bounded),
		"truncated":        truncated,
		"items_limit":      limit,
		"items":            bounded,
	}
}
