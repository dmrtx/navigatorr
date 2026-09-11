package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/tdarr"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type fakeTdarrClient struct {
	statusFunc    func(ctx context.Context) (*tdarr.ServerStatus, error)
	nodesFunc     func(ctx context.Context) (map[string]tdarr.Node, error)
	submitFunc    func(ctx context.Context, req tdarr.SubmitRequest) (*tdarr.SubmitResponse, error)
	jobStatusFunc func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error)
	cancelFunc    func(ctx context.Context, req tdarr.CancelRequest) error
}

func (f *fakeTdarrClient) Status(ctx context.Context) (*tdarr.ServerStatus, error) {
	if f.statusFunc != nil {
		return f.statusFunc(ctx)
	}
	return &tdarr.ServerStatus{Status: "good", Version: "2.87.01"}, nil
}

func (f *fakeTdarrClient) Nodes(ctx context.Context) (map[string]tdarr.Node, error) {
	if f.nodesFunc != nil {
		return f.nodesFunc(ctx)
	}
	return map[string]tdarr.Node{
		"node1": {ID: "node1", NodeName: "Davids-M1-Max"},
	}, nil
}

func (f *fakeTdarrClient) GetLibrary(ctx context.Context, libraryID string) (*tdarr.LibrarySettings, error) {
	return &tdarr.LibrarySettings{
		ID:                                   libraryID,
		Name:                                 "Test Library",
		FolderToFolderConversion:             true,
		FolderToFolderConversionDeleteSource: false,
		OutputFolder:                         "/media/transcodes",
	}, nil
}

func (f *fakeTdarrClient) Submit(ctx context.Context, req tdarr.SubmitRequest) (*tdarr.SubmitResponse, error) {
	if f.submitFunc != nil {
		return f.submitFunc(ctx, req)
	}
	return &tdarr.SubmitResponse{Success: true, Reference: req.FilePath}, nil
}

func (f *fakeTdarrClient) JobStatus(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
	if f.jobStatusFunc != nil {
		return f.jobStatusFunc(ctx, ref)
	}
	return &tdarr.JobStatusResponse{Found: true, Status: "running", Progress: 50.0}, nil
}

func (f *fakeTdarrClient) Cancel(ctx context.Context, req tdarr.CancelRequest) error {
	if f.cancelFunc != nil {
		return f.cancelFunc(ctx, req)
	}
	return nil
}

func setupTdarrMCP(client tdarr.Client) *server.MCPServer {
	s := server.NewMCPServer("test-server", "1.0.0")
	registerTdarrTools(s, client)
	return s
}

func callMCPTool(s *server.MCPServer, name string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
	// mcp-go server tool dispatch
	toolHandler := s.GetTool(name)
	if toolHandler == nil {
		return nil, errors.New("tool not found: " + name)
	}
	return toolHandler.Handler(context.Background(), req)
}

func TestTdarrTools_Status(t *testing.T) {
	fake := &fakeTdarrClient{
		statusFunc: func(ctx context.Context) (*tdarr.ServerStatus, error) {
			return &tdarr.ServerStatus{
				Status:       "good",
				IsProduction: true,
				Version:      "2.87.01",
				ServerEngine: "nodejs",
			}, nil
		},
	}
	s := setupTdarrMCP(fake)

	res, err := callMCPTool(s, "tdarr_status", nil)
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "2.87.01") || !strings.Contains(text, "good") {
		t.Errorf("unexpected output: %s", text)
	}

	// Error branch
	fake.statusFunc = func(ctx context.Context) (*tdarr.ServerStatus, error) {
		return nil, errors.New("connection refused")
	}
	resErr, err := callMCPTool(s, "tdarr_status", nil)
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if !resErr.IsError {
		t.Fatalf("expected error result, got: %+v", resErr)
	}
}

func TestTdarrTools_Nodes(t *testing.T) {
	fake := &fakeTdarrClient{
		nodesFunc: func(ctx context.Context) (map[string]tdarr.Node, error) {
			return map[string]tdarr.Node{
				"node-m1": {
					ID:       "node-m1",
					NodeName: "Davids-M1-Max",
					Workers: map[string]tdarr.WorkerItem{
						"w1": {ID: "w1", WorkerType: "transcodegpu", Percentage: 80.0},
					},
				},
			}, nil
		},
	}
	s := setupTdarrMCP(fake)

	res, err := callMCPTool(s, "tdarr_nodes", nil)
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %+v", res)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Davids-M1-Max") || !strings.Contains(text, "transcodegpu") {
		t.Errorf("unexpected nodes output: %s", text)
	}
}

func TestTdarrTools_JobStatus(t *testing.T) {
	fake := &fakeTdarrClient{
		jobStatusFunc: func(ctx context.Context, ref string) (*tdarr.JobStatusResponse, error) {
			if ref == "job-123" {
				return &tdarr.JobStatusResponse{
					Found:    true,
					Status:   "running",
					Progress: 75.5,
					ETA:      "00:01:20",
				}, nil
			}
			return &tdarr.JobStatusResponse{Found: false, Status: "unknown"}, nil
		},
	}
	s := setupTdarrMCP(fake)

	// Missing reference
	resNoRef, _ := callMCPTool(s, "tdarr_job_status", map[string]any{})
	if !resNoRef.IsError {
		t.Error("expected error on missing reference")
	}

	// Valid reference
	res, err := callMCPTool(s, "tdarr_job_status", map[string]any{"reference": "job-123"})
	if err != nil {
		t.Fatalf("call error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "75.5") || !strings.Contains(text, "running") {
		t.Errorf("unexpected output: %s", text)
	}
}

func TestTdarrTools_Cancel(t *testing.T) {
	cancelled := false
	fake := &fakeTdarrClient{
		cancelFunc: func(ctx context.Context, req tdarr.CancelRequest) error {
			if req.JobId == "job-kill-1" {
				cancelled = true
				return nil
			}
			return errors.New("job not found")
		},
	}
	s := setupTdarrMCP(fake)

	// Missing all params
	resEmpty, _ := callMCPTool(s, "tdarr_cancel", map[string]any{})
	if !resEmpty.IsError {
		t.Error("expected error when no identifiers provided to cancel")
	}

	// Valid cancel
	res, err := callMCPTool(s, "tdarr_cancel", map[string]any{"job_id": "job-kill-1"})
	if err != nil {
		t.Fatalf("call error: %v", err)
	}
	if res.IsError || !cancelled {
		t.Fatalf("expected cancel success, got: %+v", res)
	}
}
