package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerBuilds registers the read and cancel side of the build records that
// other tools post to the maintenance API (POST /api/maintenance/builds). A
// build has a target machine; when the target is a transcode node the build
// holds that node while it runs.
func registerBuilds(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("build_list", mcp.WithDescription("List builds that other tools run on machines (target = a transcode node such as m1, or any other machine name). Running and waiting builds come first, then the history, each newest first; the limit cuts only the history. Every build carries holds_transcode_node: true when the target is a configured transcode node, so the build keeps that node out of rotation while it runs (no new transcode work; running encodes are handled by its preempt_mode, suspend freezes them and they continue afterwards); false means the build is only tracked. A priority=low build always holds its node gracefully (a requested preempt_mode is overridden to graceful, so it never freezes or requeues an encode) and on a transcode node is waiting (it holds nothing) until the node is idle, then running; on any other target it runs at once. Filters (all optional): target, status (waiting, running, succeeded, failed, cancelled, lost), source, limit (default 100)."),
		mcp.WithString("target", mcp.Description("Machine name, for example m1.")),
		mcp.WithString("status", mcp.Description("waiting, running, succeeded, failed, cancelled or lost.")),
		mcp.WithString("source", mcp.Description("The tool that posted the build, for example mdm-stack.")),
		mcp.WithNumber("limit", mcp.Description("Most builds to return, 1 to 1000. Default 100.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		f := store.BuildFilter{}
		f.Target, _ = args["target"].(string)
		f.Status, _ = args["status"].(string)
		f.Source, _ = args["source"].(string)
		if n, ok := args["limit"].(float64); ok {
			if n < 1 || n > 1000 {
				return mcp.NewToolResultError("limit must be between 1 and 1000"), nil
			}
			f.Limit = int(n)
		}
		list, err := engine.ListBuilds(f)
		if err != nil {
			return mcp.NewToolResultError("builds could not be read: " + err.Error()), nil
		}
		return jsonResult(map[string]any{"builds": list})
	})
	s.AddTool(mcp.NewTool("build_status", mcp.WithDescription("Read one build by id: target, source, holder, kind, title, preempt_mode, priority (normal, low), status (waiting, running, succeeded, failed, cancelled, lost), created_at, started_at, last_heartbeat_at, expires_at, ended_at, progress, exit_code and holds_transcode_node (true when the target is a transcode node that the build holds while it runs)."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Build id, for example bld_0123456789abcdef.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, _ := req.GetArguments()["id"].(string)
		b, err := engine.GetBuild(id)
		if errors.Is(err, store.ErrBuildNotFound) {
			return mcp.NewToolResultError("build " + id + " not found"), nil
		}
		if err != nil {
			return mcp.NewToolResultError("build could not be read: " + err.Error()), nil
		}
		return jsonResult(b)
	})
	s.AddTool(mcp.NewTool("build_cancel", mcp.WithDescription("Mark a running or waiting build cancelled and free its node on Navigatorr's side (the node resumes when no other build or operator pause holds it; frozen encodes continue). This does NOT touch the process on the build machine: stop that yourself. Cancelling a build that already ended changes nothing and returns it."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Build id.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, _ := req.GetArguments()["id"].(string)
		b, _, err := engine.FinishBuild(ctx, id, store.BuildCancelled, nil)
		if errors.Is(err, store.ErrBuildNotFound) {
			return mcp.NewToolResultError("build " + id + " not found"), nil
		}
		if err != nil {
			return mcp.NewToolResultError("build not cancelled: " + err.Error()), nil
		}
		return jsonResult(b)
	})
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}
