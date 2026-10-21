package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jakenesler/navigatorr/action"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type goalChangeInput struct {
	ID               string   `json:"id" jsonschema:"required,description=Automatic transcode batch ID"`
	Mode             string   `json:"mode,omitempty" jsonschema:"description=Omit for a read-only preview; size, quality, x265_preserve or compact (much smaller SDR 1080p; drops HDR, Dolby Vision and Atmos) requests another goal. size converts lossless Atmos/DTS:X (TrueHD, DTS-HD MA) to AAC on their channel bed and copies E-AC-3 Atmos"`
	Scope            string   `json:"scope,omitempty" jsonschema:"description=all or unfinished; defaults to all unreplaced files"`
	SelectionVersion string   `json:"selection_version,omitempty" jsonschema:"description=Version from the read-only preview; required to change a goal"`
	IdempotencyKey   string   `json:"idempotency_key,omitempty" jsonschema:"description=Unique command receipt; required to change a goal"`
	AllowHardware    *bool    `json:"allow_hardware,omitempty" jsonschema:"description=Only with mode=size: false encodes with x265 only, true (the default) tries Apple VideoToolbox first, through the same quality gates"`
	AllowSlowPreset  *bool    `json:"allow_slow_preset,omitempty" jsonschema:"description=Only with mode=size: true also lets the quality fallback try the x265 slow preset, whose full encodes can take hours; default false"`
	SizeBudgetPct    *float64 `json:"size_budget_percent,omitempty" jsonschema:"description=Only with mode=size, not with size_budget_bytes: 5 to 85, a percent of each source file. The whole file is encoded to fit it. With allow_hardware on and a VideoToolbox worker it is encoded with VideoToolbox at quality 65, 58, 50, then at the budget's average bitrate, and the first result within the budget is checked against the quality floor. Otherwise it is encoded once at the budget with two-pass x265. A result that misses the quality target but meets the floor is reported as below the quality target, within budget"`
	SizeBudgetBytes  *float64 `json:"size_budget_bytes,omitempty" jsonschema:"description=Only with mode=size, not with size_budget_percent: a whole number of bytes, at least 50000000, used the same way; never more than 85% of the source"`
	SizeBudgetSource *string  `json:"size_budget_source,omitempty" jsonschema:"description=Only with mode=size, not with size_budget_percent or size_budget_bytes: siblings sets each file's budget from the median total bitrate of the other video files in its season folder (else its series folder; at least 3 of the same resolution class) times size_budget_sibling_multiplier"`
	SizeBudgetMult   *float64 `json:"size_budget_sibling_multiplier,omitempty" jsonschema:"description=Only with size_budget_source=siblings: 1.0 to 4.0, default 1.5"`
	HardwareMargin   *float64 `json:"hardware_margin_percent,omitempty" jsonschema:"description=Accepted for existing callers and frozen with the goal; it has no effect now that no VideoToolbox result is compared with an x265 one. 0 to 50, default 15"`
}

func registerTranscodeGoalTool(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("transcode_goal_change", mcp.WithDescription("Preview or change an automatic batch's goal without choosing technical profiles. Omit mode to read the exact selection and version. To change, provide mode, selection_version and idempotency_key; with mode=size, allow_hardware=false encodes with x265 only (hardware_margin_percent is accepted and has no effect); allow_slow_preset=true also lets the quality fallback try the slow x265 preset (hours per full encode; default false); size_budget_percent (5-85, of the source), size_budget_bytes (at least 50000000) or size_budget_source=siblings (the median bitrate of the other episodes in its season or series folder times size_budget_sibling_multiplier 1.0-4.0, default 1.5; at least 3 of the same resolution class), exactly one of them, encodes the whole file to fit the budget (two-pass x265, or, when allowed and the worker has VideoToolbox, a VideoToolbox ladder at quality 65, 58, 50 and then the budget's average bitrate; floor VMAF 70/60/65, reported as below the quality target and never replaced without approval); leaving allow_hardware out turns the media engine on, so read the preview first: for smaller-files attempts it shows the allow_hardware / hardware_margin_percent each selected file was frozen with (hardware_mixed when they differ) and the size_budget they were frozen with (size_budget_mixed when they differ); passing those again keeps them, and a size change without size_budget_* inputs drops the budget. Keeps originals and existing attempts; active attempts finish first. Returns a durable command receipt; monitor its command_id with action_status."), mcp.WithInputSchema[goalChangeInput]()), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id := argString(args, "id", "")
		scope := argString(args, "scope", "all")
		if scope != "all" && scope != "unfinished" {
			return toolErr("invalid scope"), nil
		}
		inst, err := engine.Deps().Store.GetActionInstanceIfExists(id)
		if err != nil || inst == nil || inst.ActionName != "transcode_batch" {
			return toolErr("automatic batch not found"), nil
		}
		var inputs map[string]any
		_ = json.Unmarshal([]byte(inst.InputsJSON), &inputs)
		if inputs["mode"] == nil || inputs["dry_run"] == true {
			return toolErr("only an automatic processing batch can change goals"), nil
		}
		items, err := engine.Deps().Store.ListTranscodeBatchItems(id)
		if err != nil {
			return toolErr("read saved selection"), nil
		}
		var state map[string]any
		_ = json.Unmarshal([]byte(inst.StateJSON), &state)
		version := action.BatchSelectionVersion(inst, items)
		selected := 0
		var frozen action.HardwareChoice
		var budget map[string]any
		budgetKnown, budgetMixed := false, false
		for _, item := range items {
			if !action.BatchRevisionSelects(item, scope, "") {
				continue
			}
			reserved, err := engine.BatchItemReplacementReserved(item)
			if err != nil {
				return toolErr("read replacement state"), nil
			}
			if !reserved {
				selected++
				if allow, margin, ok := action.BatchItemFrozenHardware(state, item.ItemKey); ok {
					frozen.Add(allow, margin)
				}
				b := action.BatchItemFrozenSizeBudget(state, item.ItemKey)
				if !budgetKnown {
					budget, budgetKnown = b, true
				} else if !reflect.DeepEqual(budget, b) {
					budgetMixed = true
				}
			}
		}
		mode := argString(args, "mode", "")
		if mode == "" {
			preview := map[string]any{"id": id, "scope": scope, "selection_version": version, "selected": selected, "goals": []string{"size", "quality", "x265_preserve", "compact"}}
			if frozen.Known {
				// The media engine choice the selected smaller-files attempts were frozen
				// with. A change to size that omits allow_hardware takes the default (on,
				// 15), not this choice, so pass it to keep it.
				preview["allow_hardware"], preview["hardware_margin_percent"], preview["hardware_mixed"] = frozen.Allow, frozen.Margin, frozen.Mixed
			}
			if budget != nil || budgetMixed {
				// The size budget the selected attempts were frozen with. A
				// change to size freezes only the size_budget_* inputs it is
				// given, so pass them again to keep it.
				preview["size_budget"], preview["size_budget_mixed"] = budget, budgetMixed
			}
			return toolJSON(preview), nil
		}
		if mode != "size" && mode != "quality" && mode != "x265_preserve" && mode != "compact" {
			return toolErr("invalid processing goal"), nil
		}
		key, expected := argString(args, "idempotency_key", ""), argString(args, "selection_version", "")
		if key == "" || len(key) > 80 || expected == "" {
			return toolErr("reviewed selection_version and idempotency_key are required"), nil
		}
		settings := map[string]any{"mode": mode}
		if raw, given := args["allow_hardware"]; given && raw != nil {
			allow, ok := raw.(bool)
			if !ok {
				return toolErr("allow_hardware must be true or false"), nil
			}
			settings["allow_hardware"] = allow
		}
		if raw, given := args["hardware_margin_percent"]; given && raw != nil {
			margin, ok := raw.(float64)
			if !ok {
				return toolErr("hardware_margin_percent must be a number"), nil
			}
			settings["hardware_margin_percent"] = margin
		}
		if raw, given := args["allow_slow_preset"]; given && raw != nil {
			allow, ok := raw.(bool)
			if !ok {
				return toolErr("allow_slow_preset must be true or false"), nil
			}
			settings["allow_slow_preset"] = allow
		}
		if raw, given := args["size_budget_source"]; given && raw != nil {
			source, ok := raw.(string)
			if !ok {
				return toolErr("size_budget_source must be a string"), nil
			}
			settings["size_budget_source"] = source
		}
		for _, key := range []string{"size_budget_percent", "size_budget_bytes", "size_budget_sibling_multiplier"} {
			if raw, given := args[key]; given && raw != nil {
				n, ok := raw.(float64)
				if !ok {
					return toolErr("%s must be a number", key), nil
				}
				settings[key] = n
			}
		}
		if err := action.ValidateHardwareInputs(mode, settings); err != nil {
			return toolErr("%v", err), nil
		}
		command := map[string]any{"kind": "reconfigure", "id": id, "scope": scope, "selection_version": expected, "settings": settings}
		result, err := engine.Enqueue(ctx, "maintenance_command", command, fmt.Sprintf("mcp-goal:%s:%s", id, key))
		if err != nil {
			return toolErr("%v", err), nil
		}
		engine.WakeReconciler()
		return toolJSON(map[string]any{"id": id, "command_id": result.ID, "status": result.Status}), nil
	})
}
