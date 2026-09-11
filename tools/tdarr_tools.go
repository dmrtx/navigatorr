package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jakenesler/navigatorr/tdarr"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerTdarrTools(s *server.MCPServer, client tdarr.Client) {
	if client == nil {
		return
	}

	// tdarr_status
	s.AddTool(
		mcp.NewTool("tdarr_status",
			mcp.WithDescription("Get Tdarr server health, version, OS, and uptime status"),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			status, err := client.Status(ctx)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to get tdarr status: %v", err)), nil
			}
			data, _ := json.MarshalIndent(status, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		},
	)

	// tdarr_nodes
	s.AddTool(
		mcp.NewTool("tdarr_nodes",
			mcp.WithDescription("List connected Tdarr nodes, their hardware encoders, worker limits, and active workers"),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nodes, err := client.Nodes(ctx)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to list tdarr nodes: %v", err)), nil
			}
			data, _ := json.MarshalIndent(nodes, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		},
	)

	// tdarr_job_status
	s.AddTool(
		mcp.NewTool("tdarr_job_status",
			mcp.WithDescription("Query the status, progress, ETA, and details of a Tdarr transcode job or file path"),
			mcp.WithString("reference", mcp.Required(), mcp.Description("Job ID or file path to query")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ref := mcp.ParseString(req, "reference", "")
			if ref == "" {
				return mcp.NewToolResultError("reference is required"), nil
			}
			st, err := client.JobStatus(ctx, ref)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to query job status: %v", err)), nil
			}
			data, _ := json.MarshalIndent(st, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		},
	)

	// tdarr_cancel
	s.AddTool(
		mcp.NewTool("tdarr_cancel",
			mcp.WithDescription("Cancel an in-progress Tdarr transcode job on a node"),
			mcp.WithString("job_id", mcp.Description("Job ID to cancel (optional if file_path or node_id+worker_id provided)")),
			mcp.WithString("file_path", mcp.Description("File path being processed to cancel")),
			mcp.WithString("node_id", mcp.Description("Node ID running the worker (optional)")),
			mcp.WithString("worker_id", mcp.Description("Worker ID running the job (optional)")),
			mcp.WithString("cause", mcp.Description("Reason for cancellation (default: user)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID := mcp.ParseString(req, "job_id", "")
			filePath := mcp.ParseString(req, "file_path", "")
			nodeID := mcp.ParseString(req, "node_id", "")
			workerID := mcp.ParseString(req, "worker_id", "")
			cause := mcp.ParseString(req, "cause", "user")

			if jobID == "" && filePath == "" && (nodeID == "" || workerID == "") {
				return mcp.NewToolResultError("must specify job_id, file_path, or node_id + worker_id to cancel"), nil
			}

			err := client.Cancel(ctx, tdarr.CancelRequest{
				JobId:    jobID,
				FilePath: filePath,
				NodeID:   nodeID,
				WorkerID: workerID,
				Cause:    cause,
			})
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to cancel tdarr job: %v", err)), nil
			}
			return mcp.NewToolResultText("Tdarr job cancellation request sent successfully"), nil
		},
	)
}
