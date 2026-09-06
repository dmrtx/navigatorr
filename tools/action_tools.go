package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerActionTools(s *server.MCPServer, engine *action.Engine) {
	if engine == nil {
		return
	}

	// action_run — start a declarative multi-step action workflow
	s.AddTool(
		mcp.NewTool("action_run",
			mcp.WithDescription("Run a declarative multi-step action workflow (e.g. validate_torrent, safe_media_replacement). State is persistently tracked in SQLite and tolerates disconnects and reboots."),
			mcp.WithString("action", mcp.Required(), mcp.Description("Workflow name: validate_torrent, safe_media_replacement")),
			mcp.WithString("inputs", mcp.Description("JSON object string with action parameters (e.g. {\"service\":\"radarr\",\"media_id\":\"123\",\"hash\":\"...\"})")),
			mcp.WithString("service", mcp.Description("Shortcut: *arr service name (radarr, sonarr)")),
			mcp.WithString("media_id", mcp.Description("Shortcut: media ID in *arr service")),
			mcp.WithString("hash", mcp.Description("Shortcut: torrent infohash")),
			mcp.WithString("url", mcp.Description("Shortcut: magnet link or torrent URL")),
			mcp.WithString("path", mcp.Description("Shortcut: local file path")),
			mcp.WithString("objective", mcp.Description("Shortcut: accessibility_repair or size_optimization")),
			mcp.WithBoolean("allow_cleanup", mcp.Description("Shortcut: request cleanup of old files (requires server allow_destructive=true)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			actionName := strings.TrimSpace(argString(args, "action", ""))
			if actionName == "" {
				return toolErr("action is required"), nil
			}

			inputs := make(map[string]any)
			if rawInputs := argString(args, "inputs", ""); rawInputs != "" {
				_ = json.Unmarshal([]byte(rawInputs), &inputs)
			}

			// Merge shortcut arguments
			for _, k := range []string{"service", "media_id", "hash", "url", "path", "objective"} {
				if v := argString(args, k, ""); v != "" && inputs[k] == nil {
					inputs[k] = v
				}
			}
			if b, ok := args["allow_cleanup"].(bool); ok {
				inputs["allow_cleanup"] = b
			}

			res, err := engine.Run(ctx, actionName, inputs)
			if err != nil {
				return toolErr("action_run failed: %v", err), nil
			}

			return toolJSON(res), nil
		},
	)

	// action_resume — resume an action from waiting_external or waiting_decision
	s.AddTool(
		mcp.NewTool("action_resume",
			mcp.WithDescription("Resume a paused action workflow (in waiting_external or waiting_decision state) using its action ID, optionally providing an LLM decision (e.g. approve, reject) and additional inputs."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Action instance ID (e.g. act-safe-media-replacement-a1b2c3d4)")),
			mcp.WithString("decision", mcp.Description("Decision choice when resuming from waiting_decision: approve, reject")),
			mcp.WithString("inputs", mcp.Description("Optional JSON object string with additional parameters")),
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
				_ = json.Unmarshal([]byte(rawInputs), &extraInputs)
			}

			res, err := engine.Resume(ctx, id, decision, extraInputs)
			if err != nil {
				return toolErr("action_resume failed: %v", err), nil
			}

			return toolJSON(res), nil
		},
	)

	// action_status — query the status, current step, and step log of an action
	s.AddTool(
		mcp.NewTool("action_status",
			mcp.WithDescription("Check the current status, current step, waiting reasons/options, and step logs of an action instance."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Action instance ID")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id := strings.TrimSpace(argString(args, "id", ""))
			if id == "" {
				return toolErr("id is required"), nil
			}

			res, err := engine.Status(ctx, id)
			if err != nil {
				return toolErr("action_status failed: %v", err), nil
			}

			// Include logged step details
			loggedSteps, _ := engine.Deps().Store.GetActionSteps(id)
			outMap := map[string]any{
				"action": res,
				"steps":  loggedSteps,
			}

			return toolJSON(outMap), nil
		},
	)

	// action_list — list recent actions with optional status filtering
	s.AddTool(
		mcp.NewTool("action_list",
			mcp.WithDescription("List action workflow instances with optional status filtering (running, waiting_external, waiting_decision, completed, failed, or all)."),
			mcp.WithString("status", mcp.Description("Filter status: running, waiting_external, waiting_decision, completed, failed, all (default all)")),
			mcp.WithString("limit", mcp.Description("Max items to return (default 20, max 100)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			status := strings.TrimSpace(argString(args, "status", "all"))
			limit := int(argInt64(args, "limit", 20))

			res, err := engine.List(ctx, status, limit)
			if err != nil {
				return toolErr("action_list failed: %v", err), nil
			}

			return toolJSON(res), nil
		},
	)
}
