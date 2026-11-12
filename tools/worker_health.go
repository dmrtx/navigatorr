package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerWorkerHealth(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("worker_health", mcp.WithDescription("Read the shared worker reachability, readiness and scheduler sweep evidence. Missing or stale scheduler evidence is unknown; this read does not submit or restart work.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		data, err := json.Marshal(engine.WorkerObservation(ctx))
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

func registerWorkerDrain(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("worker_drain", mcp.WithDescription("Make worker nodes safe to deploy. Drain on: nothing new reaches any worker (batches start no file, held files wait, nothing is cancelled); files already encoding finish and are saved; batches the user paused stay paused. Calls, all idempotent: no arguments = read only; enabled=true = start (repeat is a no-op); enabled=false = resume (only lifts the drain). Every call returns the status: ready_for_deploy (true only when drain is on, the settle window passed, every worker is reachable and none has a file encoding or unsaved), not_ready (why not), per worker in_flight files with reason, publication_pending (encoded, not saved: fix with action_resume decision=recheck on its action_id), queued (held, not started). Cycle: enabled=true; read every 1-2 min until ready_for_deploy=true; deploy; enabled=false. Reads are cheap. Unreadable state reports not ready."),
		mcp.WithBoolean("enabled", mcp.Description("true starts draining; false resumes. Omit to only read the status."))), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		status := engine.WorkerDrainStatus(ctx)
		if enabled, ok := req.GetArguments()["enabled"].(bool); ok {
			var err error
			if status, err = engine.SetWorkerDrain(ctx, enabled); err != nil {
				return mcp.NewToolResultError("worker drain not saved: " + err.Error()), nil
			}
		}
		data, err := json.Marshal(status)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

// registerWorkerPause is the MCP side of the per-node pause the maintenance
// console offers in its workers dialog: one node stops taking jobs so the
// machine is free for something else, while the others keep encoding.
func registerWorkerPause(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("worker_pause", mcp.WithDescription("Take ONE worker node out of rotation so its machine is free for other work; the other nodes keep encoding (worker_drain is the all-nodes switch for deploys). A paused node takes no new job: the queue assigns files to the other nodes, files assigned to it but not sent go back to the queue, and new podcast work is held if it is the podcast node (a podcast job already running finishes). Calls, all idempotent: no arguments = read every node's pause; node only = read that node; node + mode = change it. mode=graceful: what is encoding finishes and publishes, the node is idle when the last one ends. mode=immediate: running encodes are cancelled on the node and go back to the queue to start again from the start on another node (progress is lost; nothing is cancelled for the action or its batch). mode=off: resume. graceful can later be raised to immediate. Returns each node's status: paused, mode, since, running files still on it, idle (true when the machine is free), message. Immediate sends files back in the background, so read again until running is 0."),
		mcp.WithString("node", mcp.Description("Node name, for example m1. Omit to read every node.")),
		mcp.WithString("mode", mcp.Description("graceful, immediate or off. Needs node. Omit to only read.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		node, _ := args["node"].(string)
		mode, hasMode := args["mode"].(string)
		var statuses []transcode.NodePauseStatus
		switch {
		case hasMode && mode != "" && node == "":
			return mcp.NewToolResultError("mode needs a node: send node and mode"), nil
		case hasMode && mode != "":
			stored := ""
			switch mode {
			case store.WorkerPauseGraceful, store.WorkerPauseImmediate:
				stored = mode
			case "off":
			default:
				return mcp.NewToolResultError(`mode must be "graceful", "immediate" or "off"`), nil
			}
			status, err := engine.SetNodePause(ctx, node, stored)
			var unknown *transcode.WorkerNotConfiguredError
			switch {
			case errors.As(err, &unknown):
				return mcp.NewToolResultError("node " + node + " is not configured"), nil
			case err != nil:
				return mcp.NewToolResultError("node pause not saved: " + err.Error()), nil
			}
			statuses = []transcode.NodePauseStatus{status}
		default:
			all := engine.NodePauseStatuses(ctx)
			for name, status := range all {
				if node == "" || node == name {
					statuses = append(statuses, status)
				}
			}
			if node != "" && len(statuses) == 0 {
				return mcp.NewToolResultError("node " + node + " is not configured"), nil
			}
			sort.Slice(statuses, func(i, j int) bool { return statuses[i].Node < statuses[j].Node })
		}
		data, err := json.Marshal(map[string]any{"nodes": statuses})
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}
