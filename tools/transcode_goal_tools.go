package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
	AllowHardware    *bool    `json:"allow_hardware,omitempty" jsonschema:"description=Only with mode=size: false keeps the search on x265 only, true (the default) also tries Apple VideoToolbox encodes through the same quality gates"`
	HardwareMargin   *float64 `json:"hardware_margin_percent,omitempty" jsonschema:"description=Only with mode=size and allow_hardware not false: how much larger than the best x265 result a VideoToolbox result may be and still win; 0 to 50, default 15"`
}

func registerTranscodeGoalTool(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("transcode_goal_change", mcp.WithDescription("Preview or change an automatic batch's goal without choosing technical profiles. Omit mode to read the exact selection and version. To change, provide mode, selection_version and idempotency_key; with mode=size, allow_hardware=false keeps the search on x265 only and hardware_margin_percent (0-50, default 15) sets the media engine size margin. Keeps originals, existing attempts and the operation's finite search budget; active attempts finish first. Returns a durable command receipt; monitor its command_id with action_status."), mcp.WithInputSchema[goalChangeInput]()), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		version := action.BatchSelectionVersion(inst, items)
		selected := 0
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
			}
		}
		mode := argString(args, "mode", "")
		if mode == "" {
			return toolJSON(map[string]any{"id": id, "scope": scope, "selection_version": version, "selected": selected, "goals": []string{"size", "quality", "x265_preserve", "compact"}}), nil
		}
		if mode != "size" && mode != "quality" && mode != "x265_preserve" && mode != "compact" {
			return toolErr("invalid processing goal"), nil
		}
		key, expected := argString(args, "idempotency_key", ""), argString(args, "selection_version", "")
		if key == "" || len(key) > 80 || expected == "" {
			return toolErr("reviewed selection_version and idempotency_key are required"), nil
		}
		settings := map[string]any{"mode": mode}
		if allow, ok := args["allow_hardware"].(bool); ok {
			settings["allow_hardware"] = allow
		}
		if margin, ok := args["hardware_margin_percent"].(float64); ok {
			settings["hardware_margin_percent"] = margin
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
