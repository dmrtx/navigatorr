package tools

import (
	"context"
	"encoding/json"

	"github.com/jakenesler/navigatorr/action"
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
