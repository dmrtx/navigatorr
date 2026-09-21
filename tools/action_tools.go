package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// MaxActionResponseBytes is the hard guard limit (64 KiB) for MCP action tool responses.
const MaxActionResponseBytes = 64 * 1024

// MaxDetailChunkBytes is the single-chunk threshold (48 KiB) for action_detail responses.
const MaxDetailChunkBytes = 48 * 1024

func chunkPayload(id, actionName, section, key string, rawBytes []byte, chunkIdx int) map[string]any {
	totalBytes := len(rawBytes)
	chunkSize := MaxDetailChunkBytes
	totalChunks := (totalBytes + chunkSize - 1) / chunkSize
	if totalChunks <= 0 {
		totalChunks = 1
	}
	if chunkIdx < 0 {
		chunkIdx = 0
	}
	if chunkIdx >= totalChunks {
		chunkIdx = totalChunks - 1
	}
	start := chunkIdx * chunkSize
	end := start + chunkSize
	if end > totalBytes {
		end = totalBytes
	}
	chunkContent := string(rawBytes[start:end])

	res := map[string]any{
		"id":           id,
		"action_name":  actionName,
		"section":      section,
		"chunk":        chunkIdx,
		"total_chunks": totalChunks,
		"chunk_bytes":  len(chunkContent),
		"total_bytes":  totalBytes,
		"has_more":     chunkIdx+1 < totalChunks,
		"content":      chunkContent,
		"note":         "Section payload exceeds single-response threshold. Returned chunked text. Request subsequent chunks with chunk=<N>, or specify key=<name> to narrow the query.",
	}
	if key != "" {
		res["key"] = key
	}
	if chunkIdx+1 < totalChunks {
		res["next_chunk"] = chunkIdx + 1
	}
	return res
}

// ActionCompactSummary contains operational fields needed to monitor or continue workflows,
// omitting full inputs/outputs/state to protect the model's context window.
type ActionCompactSummary struct {
	ID                   string                 `json:"id"`
	ActionName           string                 `json:"action_name"`
	Status               string                 `json:"status"`
	CurrentStep          int                    `json:"current_step"`
	TotalSteps           int                    `json:"total_steps"`
	Progress             string                 `json:"progress"`
	WaitingReason        string                 `json:"waiting_reason,omitempty"`
	WaitingCondition     string                 `json:"waiting_condition,omitempty"`
	WaitingOptions       []action.WaitingOption `json:"waiting_options,omitempty"`
	Error                string                 `json:"error,omitempty"`
	IdempotencyKey       string                 `json:"idempotency_key,omitempty"`
	DurationMs           int64                  `json:"duration_ms"`
	WallDurationMs       int64                  `json:"wall_duration_ms"`
	QueueDurationMs      *int64                 `json:"queue_duration_ms,omitempty"`
	EncodeDurationMs     *int64                 `json:"encode_duration_ms,omitempty"`
	ValidationDurationMs *int64                 `json:"validation_duration_ms,omitempty"`
	ReconcileLagMs       *int64                 `json:"reconcile_lag_ms,omitempty"`
	Worker               map[string]any         `json:"worker,omitempty"`
	Reconciliation       map[string]any         `json:"reconciliation,omitempty"`
	Promotion            map[string]any         `json:"promotion,omitempty"`
	CreatedAt            string                 `json:"created_at,omitempty"`
	UpdatedAt            string                 `json:"updated_at,omitempty"`
}

// StepSummary represents a compact execution step record omitting raw JSON payloads.
type StepSummary struct {
	StepIndex  int    `json:"step_index"`
	StepName   string `json:"step_name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

func formatProgress(currentStep, totalSteps int, status string) string {
	if totalSteps <= 0 {
		return fmt.Sprintf("step %d", currentStep)
	}
	if status == action.StatusCompleted {
		return fmt.Sprintf("%d/%d steps (100%%)", totalSteps, totalSteps)
	}
	pct := 0
	if currentStep > 0 {
		pct = (currentStep * 100) / totalSteps
	}
	return fmt.Sprintf("%d/%d steps (%d%%)", currentStep, totalSteps, pct)
}

func toCompactSummary(res *action.ActionResult) ActionCompactSummary {
	if res == nil {
		return ActionCompactSummary{}
	}
	return ActionCompactSummary{
		ID:                   res.ID,
		ActionName:           res.ActionName,
		Status:               res.Status,
		CurrentStep:          res.CurrentStep,
		TotalSteps:           res.TotalSteps,
		Progress:             formatProgress(res.CurrentStep, res.TotalSteps, res.Status),
		WaitingReason:        res.WaitingReason,
		WaitingCondition:     res.WaitingCondition,
		WaitingOptions:       res.WaitingOptions,
		Error:                res.Error,
		IdempotencyKey:       res.IdempotencyKey,
		DurationMs:           res.DurationMs,
		WallDurationMs:       res.WallDurationMs,
		QueueDurationMs:      res.QueueDurationMs,
		EncodeDurationMs:     res.EncodeDurationMs,
		ValidationDurationMs: res.ValidationDurationMs,
		ReconcileLagMs:       res.ReconcileLagMs,
		Worker: compactOperationalFields(res, []string{
			"transcode_status", "transcode_phase", "benchmark_status", "benchmark_phase", "phase",
			"progress", "speed", "fps", "last_progress_at", "worker_heartbeat_at",
			"progress_is_stale", "last_known_progress", "progress_details", "benchmark_progress_details",
			"worker_slots_total", "worker_slots_used", "queue_position", "storage_backend",
			"recovery_required", "error_class", "finalization_retry_count", "next_finalization_at",
		}),
		Reconciliation: compactOperationalFields(res, []string{
			"next_poll_at", "last_worker_poll_at", "worker_completed_at", "reconciled_at",
		}),
		Promotion: compactPromotion(res),
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
	}
}

// Approval must show the concrete files and episodes it will replace, even in
// the default compact response. Do not require a second detail request merely
// to discover what an approve option refers to.
func compactPromotion(res *action.ActionResult) map[string]any {
	if res.ActionName != "promote_transcode_candidate" {
		return nil
	}
	value := res.State["promotion"]
	if value == nil {
		value = res.Outputs["promotion"]
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var plan map[string]any
	if json.Unmarshal(encoded, &plan) != nil {
		return nil
	}
	return compactOperationalFields(&action.ActionResult{Outputs: plan}, []string{
		"transcode_action_id", "service", "series_id", "original_path", "candidate_path",
		"original_episode_file_id", "episode_ids", "original_bytes", "candidate_bytes",
		"original_sha256", "candidate_sha256", "approved", "new_episode_file_id", "new_path",
		"recovery_path", "recovery_verified",
	})
}

// Only bounded operational data belongs in compact responses. Media reports,
// paths, manifests and arbitrary workflow payloads remain in action_detail.
func compactOperationalFields(res *action.ActionResult, keys []string) map[string]any {
	var fields map[string]any
	for _, key := range keys {
		value, exists := res.Outputs[key]
		if !exists {
			value, exists = res.State[key]
		}
		if !exists || value == nil {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > 1024 {
			continue
		}
		if fields == nil {
			fields = make(map[string]any)
		}
		fields[key] = value
	}
	return fields
}

func toStepSummaries(steps []store.ActionStepLog) []StepSummary {
	if len(steps) == 0 {
		return []StepSummary{}
	}
	out := make([]StepSummary, len(steps))
	for i, s := range steps {
		out[i] = StepSummary{
			StepIndex:  s.StepIndex,
			StepName:   s.StepName,
			Status:     s.Status,
			DurationMs: s.DurationMs,
			Error:      s.Error,
			CreatedAt:  s.CreatedAt,
		}
	}
	return out
}

// toolBoundedJSON marshals v with indentation, enforcing maxBytes (defaults to MaxActionResponseBytes).
// If the payload exceeds the limit, onExceeded is used to produce a bounded replacement.
func toolBoundedJSON(v any, maxBytes int, onExceeded func(actualBytes int) any) *mcp.CallToolResult {
	if maxBytes <= 0 {
		maxBytes = MaxActionResponseBytes
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolErr("encoding JSON: %v", err)
	}
	if len(data) <= maxBytes {
		return mcp.NewToolResultText(string(data))
	}

	var fallback any
	if onExceeded != nil {
		fallback = onExceeded(len(data))
	} else {
		fallback = map[string]any{
			"error":       "response_too_large",
			"message":     fmt.Sprintf("Response size (%d bytes) exceeds the %d byte limit. Request narrower detail.", len(data), maxBytes),
			"size_bytes":  len(data),
			"limit_bytes": maxBytes,
		}
	}
	fallbackData, err := json.MarshalIndent(fallback, "", "  ")
	if err != nil || len(fallbackData) > maxBytes {
		limitMsg := fmt.Sprintf(`{"error":"response_too_large","size_bytes":%d,"limit_bytes":%d}`, len(data), maxBytes)
		return mcp.NewToolResultText(limitMsg)
	}
	return mcp.NewToolResultText(string(fallbackData))
}

func registerActionTools(s *server.MCPServer, engine *action.Engine) {
	if engine == nil {
		return
	}

	// action_run — start a declarative multi-step action workflow
	s.AddTool(
		mcp.NewTool("action_run",
			mcp.WithDescription("Run a declarative multi-step action workflow (e.g. transcode_batch, transcode_media, promote_transcode_candidate, validate_torrent, safe_media_replacement). State is persistently tracked in SQLite and tolerates disconnects and reboots. Transcode and benchmark external waits reconcile automatically; candidate promotion requires explicit approval. Note: 'inputs' must be provided as a JSON object string, and 'idempotency_key' is a top-level string argument. Returns a compact operational summary."),
			mcp.WithString("action", mcp.Required(), mcp.Description("Workflow name: transcode_batch, transcode_media, benchmark_transcode, promote_transcode_candidate, validate_torrent, safe_media_replacement")),
			mcp.WithString("inputs", mcp.Description("JSON object string with action parameters (e.g. \"{\\\"service\\\":\\\"sonarr\\\",\\\"series_id\\\":\\\"10\\\"}\" or \"{\\\"path\\\":\\\"/media/...\\\"}\"). Must be a JSON-encoded string, not a raw object.")),
			mcp.WithString("service", mcp.Description("Shortcut: *arr service name (sonarr, radarr)")),
			mcp.WithString("media_id", mcp.Description("Shortcut: media ID in *arr service")),
			mcp.WithString("hash", mcp.Description("Shortcut: torrent infohash")),
			mcp.WithString("url", mcp.Description("Shortcut: magnet link or torrent URL")),
			mcp.WithString("path", mcp.Description("Shortcut: local file path")),
			mcp.WithString("objective", mcp.Description("Shortcut: accessibility_repair or size_optimization")),
			mcp.WithString("idempotency_key", mcp.Description("Optional top-level idempotency key to prevent duplicate runs (e.g. batch-sonarr-10 or radarr:327:size_optimization)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			actionName := strings.TrimSpace(argString(args, "action", ""))
			if actionName == "" {
				return toolErr("action is required"), nil
			}

			inputs := make(map[string]any)
			if rawInputs := argString(args, "inputs", ""); rawInputs != "" {
				var err error
				inputs, err = parseJSONObject(rawInputs)
				if err != nil {
					return toolErr("invalid action_run inputs: %v", err), nil
				}
			}

			// Merge shortcut arguments
			for _, k := range []string{"service", "media_id", "hash", "url", "path", "objective"} {
				if v := argString(args, k, ""); v != "" && inputs[k] == nil {
					inputs[k] = v
				}
			}

			idempotencyKey := strings.TrimSpace(argString(args, "idempotency_key", ""))
			if idempotencyKey == "" {
				if ik, ok := inputs["idempotency_key"].(string); ok {
					idempotencyKey = strings.TrimSpace(ik)
				}
			}

			res, err := engine.Run(ctx, actionName, inputs, idempotencyKey)
			if err != nil {
				return toolErr("action_run failed: %v", err), nil
			}

			return toolBoundedJSON(toCompactSummary(res), MaxActionResponseBytes, nil), nil
		},
	)

	// action_catalog — discover registered action workflows
	s.AddTool(
		mcp.NewTool("action_catalog",
			mcp.WithDescription("Discover all registered action workflows, their versions, inputs, immutable-input policy, steps, and safety profiles."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			catalog := engine.Catalog()
			return toolBoundedJSON(catalog, MaxActionResponseBytes, nil), nil
		},
	)

	// action_retry — retry a failed action workflow from its last safe step
	s.AddTool(
		mcp.NewTool("action_retry",
			mcp.WithDescription("Retry a failed action workflow instance from its last safe step without repeating confirmed side effects. Returns a compact operational summary."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Failed action instance ID (e.g. act-safe-media-replacement-a1b2c3d4)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id := strings.TrimSpace(argString(args, "id", ""))
			if id == "" {
				return toolErr("id is required"), nil
			}

			res, err := engine.Retry(ctx, id)
			if err != nil {
				return toolErr("action_retry failed: %v", err), nil
			}

			return toolBoundedJSON(toCompactSummary(res), MaxActionResponseBytes, nil), nil
		},
	)

	// action_resume — resume an action from waiting_external or waiting_decision
	s.AddTool(
		mcp.NewTool("action_resume",
			mcp.WithDescription("Resume an active or paused action workflow using its action ID. For actions in waiting_external (e.g. transcode running in background or waiting for worker slot), call with id only to poll/advance progress. For actions in waiting_decision, provide 'decision' matching one of waiting_options (e.g. approve, reject, accept_loss, resume, pause, cancel). Workflows advertised by action_catalog with immutable_inputs=true reject additional inputs after creation. Cancelling a transcode batch propagates cancellation to admitted child actions so active remote transcode jobs are stopped when supported. Returns a compact operational summary."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Action instance ID (e.g. act-transcode_batch-a1b2c3d4)")),
			mcp.WithString("decision", mcp.Description("Decision choice when resuming from waiting_decision (matches one of the action's waiting_options, e.g. resume, pause, cancel, approve, reject, accept_loss). Omit when resuming waiting_external.")),
			mcp.WithString("inputs", mcp.Description("Optional JSON object string with additional parameters. Rejected for workflows whose template declares immutable inputs.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id := strings.TrimSpace(argString(args, "id", ""))
			if id == "" {
				return toolErr("id is required"), nil
			}

			decision := strings.TrimSpace(argString(args, "decision", ""))
			var extraInputs map[string]any
			if rawInputs := argString(args, "inputs", ""); rawInputs != "" {
				var err error
				extraInputs, err = parseJSONObject(rawInputs)
				if err != nil {
					return toolErr("invalid action_resume inputs: %v", err), nil
				}
			}

			res, err := engine.Resume(ctx, id, decision, extraInputs)
			if err != nil {
				return toolErr("action_resume failed: %v", err), nil
			}

			return toolBoundedJSON(toCompactSummary(res), MaxActionResponseBytes, nil), nil
		},
	)

	// action_status — query the status, current step, and step log of an action
	s.AddTool(
		mcp.NewTool("action_status",
			mcp.WithDescription("Check current status, step progress, waiting reasons/options, and step summaries of an action instance. Returns a compact operational summary by default. Set verbose=true to request full logs (bounded to 64 KiB)."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Action instance ID")),
			mcp.WithBoolean("verbose", mcp.Description("Optional: when true, attempts to return full inputs, outputs, state, and step payloads. Subject to a 64 KiB size guard. Defaults to false.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id := strings.TrimSpace(argString(args, "id", ""))
			if id == "" {
				return toolErr("id is required"), nil
			}
			verbose := argBool(args, "verbose", false)

			res, err := engine.Status(ctx, id)
			if err != nil {
				return toolErr("action_status failed: %v", err), nil
			}

			// Include logged step details
			loggedSteps, _ := engine.Deps().Store.GetActionSteps(id)

			if verbose {
				fullMap := map[string]any{
					"action": res,
					"steps":  loggedSteps,
				}
				return toolBoundedJSON(fullMap, MaxActionResponseBytes, func(actualBytes int) any {
					return map[string]any{
						"error":       "response_too_large",
						"message":     fmt.Sprintf("Full action detail (%d KiB) exceeds the %d KiB limit. Request narrower detail using action_detail(id=%q, section=\"inputs|outputs|state|steps\").", actualBytes/1024, MaxActionResponseBytes/1024, id),
						"size_bytes":  actualBytes,
						"limit_bytes": MaxActionResponseBytes,
						"action":      toCompactSummary(res),
						"step_count":  len(loggedSteps),
						"hint":        "Use action_detail with section='inputs', 'outputs', 'state', or 'steps' (with optional step_index) to retrieve specific fields without blowing the context window.",
					}
				}), nil
			}

			// Include logged step details (compact summaries, bounded to most recent 25 steps if many)
			stepSummaries := toStepSummaries(loggedSteps)
			var stepsField any = stepSummaries
			var totalLoggedSteps *int
			if len(stepSummaries) > 25 {
				n := len(stepSummaries)
				totalLoggedSteps = &n
				stepsField = stepSummaries[n-25:]
			}

			compactMap := map[string]any{
				"action": toCompactSummary(res),
				"steps":  stepsField,
			}
			if totalLoggedSteps != nil {
				compactMap["total_steps_logged"] = *totalLoggedSteps
				compactMap["steps_note"] = fmt.Sprintf("Showing latest 25 of %d steps. Use action_detail(id=%q, section='steps') for earlier steps.", *totalLoggedSteps, id)
			}
			return toolBoundedJSON(compactMap, MaxActionResponseBytes, nil), nil
		},
	)

	// action_detail — retrieve one logical detail section of an action
	s.AddTool(
		mcp.NewTool("action_detail",
			mcp.WithDescription("Retrieve a specific logical detail section of an action workflow (inputs, outputs, state, or steps) to inspect full data without bloating the context window. Supports key extraction, automatic chunking for payloads >48 KiB, and step pagination."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Action instance ID")),
			mcp.WithString("section", mcp.Required(), mcp.Description("Detail section to retrieve: inputs, outputs, state, or steps")),
			mcp.WithString("key", mcp.Description("Optional top-level key to extract when section is inputs, outputs, or state")),
			mcp.WithString("chunk", mcp.Description("Optional 0-based chunk index to read large payloads in slices (default 0)")),
			mcp.WithString("step_index", mcp.Description("Optional 0-based step index when section=steps to inspect a single step")),
			mcp.WithString("step_offset", mcp.Description("Optional pagination offset when section=steps and step_index is omitted (default 0)")),
			mcp.WithString("step_limit", mcp.Description("Optional pagination limit when section=steps and step_index is omitted (1-50, default 10)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id := strings.TrimSpace(argString(args, "id", ""))
			if id == "" {
				return toolErr("id is required"), nil
			}
			section := strings.ToLower(strings.TrimSpace(argString(args, "section", "")))
			if section == "" {
				return toolErr("section is required: inputs, outputs, state, or steps"), nil
			}

			inst, err := engine.Deps().Store.GetActionInstance(id)
			if err != nil || inst == nil {
				return toolErr("action instance %q not found", id), nil
			}

			key := strings.TrimSpace(argString(args, "key", ""))
			chunkIdx := int(argInt64(args, "chunk", 0))

			switch section {
			case "inputs", "outputs", "state":
				var jsonStr string
				switch section {
				case "inputs":
					jsonStr = inst.InputsJSON
				case "outputs":
					jsonStr = inst.OutputsJSON
				case "state":
					jsonStr = inst.StateJSON
				}
				if jsonStr == "" {
					jsonStr = "{}"
				}

				if key != "" {
					var obj map[string]any
					if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
						return toolErr("section %s is not a valid JSON object: %v", section, err), nil
					}
					val, exists := obj[key]
					if !exists {
						keys := make([]string, 0, len(obj))
						for k := range obj {
							keys = append(keys, k)
						}
						return toolErr("key %q not found in section %s (available keys: %v)", key, section, keys), nil
					}
					valBytes, _ := json.Marshal(val)
					if len(valBytes) > MaxDetailChunkBytes {
						return toolJSON(chunkPayload(id, inst.ActionName, section, key, valBytes, chunkIdx)), nil
					}
					return toolJSON(map[string]any{
						"id":          id,
						"action_name": inst.ActionName,
						"section":     section,
						"key":         key,
						"data":        val,
					}), nil
				}

				rawBytes := []byte(jsonStr)
				if len(rawBytes) > MaxDetailChunkBytes {
					chunkResp := chunkPayload(id, inst.ActionName, section, "", rawBytes, chunkIdx)
					var obj map[string]any
					if err := json.Unmarshal(rawBytes, &obj); err == nil {
						keys := make([]string, 0, len(obj))
						for k := range obj {
							keys = append(keys, k)
						}
						chunkResp["available_keys"] = keys
					}
					return toolJSON(chunkResp), nil
				}

				var parsed any
				_ = json.Unmarshal(rawBytes, &parsed)
				return toolJSON(map[string]any{
					"id":          id,
					"action_name": inst.ActionName,
					"section":     section,
					"data":        parsed,
				}), nil

			case "steps":
				steps, err := engine.Deps().Store.GetActionSteps(id)
				if err != nil {
					return toolErr("fetching action steps: %v", err), nil
				}
				stepIndexRaw := argString(args, "step_index", "")

				if stepIndexRaw != "" {
					stepIdx := int(argInt64(args, "step_index", -1))
					var foundStep *store.ActionStepLog
					for _, s := range steps {
						if s.StepIndex == stepIdx {
							foundStep = &s
							break
						}
					}
					if foundStep == nil {
						return toolErr("step index %d not found for action %s (total logged steps: %d)", stepIdx, id, len(steps)), nil
					}
					var sInp, sOut any
					if foundStep.InputsJSON != "" {
						_ = json.Unmarshal([]byte(foundStep.InputsJSON), &sInp)
					}
					if foundStep.OutputsJSON != "" {
						_ = json.Unmarshal([]byte(foundStep.OutputsJSON), &sOut)
					}
					stepMap := map[string]any{
						"id":          foundStep.ID,
						"step_index":  foundStep.StepIndex,
						"step_name":   foundStep.StepName,
						"primitive":   foundStep.Primitive,
						"status":      foundStep.Status,
						"error":       foundStep.Error,
						"duration_ms": foundStep.DurationMs,
						"created_at":  foundStep.CreatedAt,
						"inputs":      sInp,
						"outputs":     sOut,
					}
					stepBytes, _ := json.Marshal(stepMap)
					if len(stepBytes) > MaxDetailChunkBytes {
						return toolJSON(chunkPayload(id, inst.ActionName, fmt.Sprintf("steps[%d]", stepIdx), "", stepBytes, chunkIdx)), nil
					}
					return toolJSON(map[string]any{
						"id":          id,
						"action_name": inst.ActionName,
						"section":     "steps",
						"step_index":  stepIdx,
						"step":        stepMap,
					}), nil
				}

				// Paginated steps
				stepOffset := int(argInt64(args, "step_offset", 0))
				if stepOffset < 0 {
					stepOffset = 0
				}
				stepLimit := int(argInt64(args, "step_limit", 10))
				if stepLimit <= 0 {
					stepLimit = 10
				} else if stepLimit > 50 {
					stepLimit = 50
				}

				var pagedSteps []any
				if stepOffset < len(steps) {
					end := stepOffset + stepLimit
					if end > len(steps) {
						end = len(steps)
					}
					for _, s := range steps[stepOffset:end] {
						var sInp, sOut any
						if s.InputsJSON != "" {
							_ = json.Unmarshal([]byte(s.InputsJSON), &sInp)
						}
						if s.OutputsJSON != "" {
							_ = json.Unmarshal([]byte(s.OutputsJSON), &sOut)
						}
						pagedSteps = append(pagedSteps, map[string]any{
							"id":          s.ID,
							"step_index":  s.StepIndex,
							"step_name":   s.StepName,
							"primitive":   s.Primitive,
							"status":      s.Status,
							"error":       s.Error,
							"duration_ms": s.DurationMs,
							"created_at":  s.CreatedAt,
							"inputs":      sInp,
							"outputs":     sOut,
						})
					}
				}

				pagedData := map[string]any{
					"id":          id,
					"action_name": inst.ActionName,
					"section":     "steps",
					"step_offset": stepOffset,
					"step_limit":  stepLimit,
					"total_steps": len(steps),
					"has_more":    stepOffset+len(pagedSteps) < len(steps),
					"steps":       pagedSteps,
				}
				pageBytes, _ := json.Marshal(pagedData)
				if len(pageBytes) > MaxDetailChunkBytes {
					return toolJSON(chunkPayload(id, inst.ActionName, "steps", "", pageBytes, chunkIdx)), nil
				}
				return toolJSON(pagedData), nil

			default:
				return toolErr("invalid section %q: must be inputs, outputs, state, or steps", section), nil
			}
		},
	)

	// action_list — list recent actions with optional status filtering
	s.AddTool(
		mcp.NewTool("action_list",
			mcp.WithDescription("List action workflow instances with optional status filtering (running, waiting_external, waiting_decision, completed, failed, or all). Returns compact summaries; use action_status for each job's worker telemetry and timing breakdown."),
			mcp.WithString("status", mcp.Description("Filter status: running, waiting_external, waiting_decision, completed, failed, all (default all)")),
			mcp.WithString("limit", mcp.Description("Max items to return (1-100, default 20)")),
			mcp.WithString("offset", mcp.Description("Pagination offset (default 0)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			status := strings.TrimSpace(argString(args, "status", "all"))
			limit := int(argInt64(args, "limit", 20))
			if limit <= 0 {
				limit = 20
			} else if limit > 100 {
				limit = 100
			}
			offset := int(argInt64(args, "offset", 0))
			if offset < 0 {
				offset = 0
			}

			res, err := engine.ListPaged(ctx, status, limit, offset)
			if err != nil {
				return toolErr("action_list failed: %v", err), nil
			}

			summaries := make([]ActionCompactSummary, len(res))
			for i := range res {
				summaries[i] = toCompactSummary(&res[i])
				// Full per-worker telemetry can make a normal 100-action page
				// exceed the response limit. Keep it in action_status; preserve
				// the listing's existing compact shape and page size.
				summaries[i].Worker = nil
				summaries[i].Reconciliation = nil
				summaries[i].Promotion = nil
				summaries[i].QueueDurationMs = nil
				summaries[i].EncodeDurationMs = nil
				summaries[i].ValidationDurationMs = nil
				summaries[i].ReconcileLagMs = nil
			}

			return toolBoundedJSON(summaries, MaxActionResponseBytes, nil), nil
		},
	)
}
