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
