package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func admittedAction(t *testing.T, result *mcp.CallToolResult) ActionCompactSummary {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatalf("admission failed: %+v", result)
	}
	var summary ActionCompactSummary
	if err := json.Unmarshal([]byte(resultText(t, result)), &summary); err != nil || summary.ID == "" {
		t.Fatalf("missing durable receipt: %+v %v", summary, err)
	}
	return summary
}

func TestMCPAdmissionOutlivesRequestCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "deadline"}[timeout], func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "admission.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			e := action.NewEngine(action.EngineDeps{Store: st, Transcode: readyAdmissionWorker{}})
			started, check, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			observedContext := make(chan error, 1)
			var hashes, submissions atomic.Int32
			e.RegisterTemplate(action.ActionTemplate{Name: "transcode_media", AutoReconcile: true, ImmutableInputs: true, Steps: []action.StepDefinition{
				{Name: "preflight", Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
					hashes.Add(1)
					close(started)
					select {
					case <-ctx.Done():
						return action.StepResult{}, ctx.Err()
					case <-check:
					}
					observedContext <- ctx.Err()
					select {
					case <-ctx.Done():
						return action.StepResult{}, ctx.Err()
					case <-finish:
					}
					return action.StepResult{Status: action.StepCompleted}, nil
				}},
				{Name: "submit", Run: func(context.Context, *action.ExecutionContext) (action.StepResult, error) {
					submissions.Add(1)
					return action.StepResult{Status: action.StepCompleted}, nil
				}},
			}})
			s := server.NewMCPServer("test", "1")
			registerActionTools(s, e)
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			if timeout {
				cancelRequest()
				requestCtx, cancelRequest = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancelRequest()
			request := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "action_run", Arguments: map[string]any{"action": "transcode_media", "inputs": `{"path":"/synthetic.mkv"}`, "idempotency_key": "one-request"}}}
			response := make(chan *mcp.CallToolResult, 1)
			go func() {
				result, _ := s.GetTool("action_run").Handler(requestCtx, request)
				response <- result
			}()
			var receipt ActionCompactSummary
			select {
			case result := <-response:
				receipt = admittedAction(t, result)
			case <-time.After(500 * time.Millisecond):
				cancelRequest()
				<-response
				t.Fatal("MCP response waited for expensive preflight")
			}
			if receipt.Status != action.StatusPending || receipt.Origin != "mcp" || hashes.Load() != 0 {
				t.Fatal("admission executed work inside the request", receipt, hashes.Load())
			}
			serviceCtx, stopService := context.WithCancel(context.Background())
			done := make(chan struct{})
			var serviceErr error
			go func() { serviceErr = e.ReconcileOnce(serviceCtx); close(done) }()
			defer func() { stopService(); <-done }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("service did not start persisted work")
			}
			if !timeout {
				cancelRequest()
			}
			<-requestCtx.Done()
			close(check)
			select {
			case err := <-observedContext:
				if err != nil {
					t.Fatal("request cancellation reached service preflight", err)
				}
			case <-time.After(time.Second):
				t.Fatal("preflight was interrupted by the caller")
			}
			// A retry while preflight is blocked must return the same ID promptly.
			again := admittedAction(t, callTool(t, s, "action_run", request.GetArguments()))
			if again.ID != receipt.ID || hashes.Load() != 1 {
				t.Fatal("retry duplicated preflight", again, hashes.Load())
			}
			close(finish)
			select {
			case <-done:
				if serviceErr != nil {
					t.Fatal(serviceErr)
				}
			case <-time.After(time.Second):
				t.Fatal("preflight did not finish")
			}
			again = admittedAction(t, callTool(t, s, "action_run", request.GetArguments()))
			if again.ID != receipt.ID || again.Status != action.StatusCompleted || hashes.Load() != 1 || submissions.Load() != 1 {
				t.Fatal("terminal retry duplicated work", again, hashes.Load(), submissions.Load())
			}
		})
	}
}

func TestMCPAdmissionReportsDurablePreflightFailuresAndImmutableConflicts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := action.NewEngine(action.EngineDeps{Store: st, Transcode: readyAdmissionWorker{}})
	e.RegisterTemplate(action.ActionTemplate{Name: "benchmark_transcode", AutoReconcile: true, ImmutableInputs: true, Steps: []action.StepDefinition{{Name: "preflight", Run: func(context.Context, *action.ExecutionContext) (action.StepResult, error) {
		return action.StepResult{}, errors.New("source cannot be read")
	}}}})
	s := server.NewMCPServer("test", "1")
	registerActionTools(s, e)
	args := map[string]any{"action": "benchmark_transcode", "inputs": `{"path":"/missing.mkv"}`, "idempotency_key": "preflight-failure"}
	first := admittedAction(t, callTool(t, s, "action_run", args))
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := admittedAction(t, callTool(t, s, "action_run", args))
	if again.ID != first.ID || again.Status != action.StatusFailed || !strings.Contains(again.Error, "source cannot be read") {
		t.Fatal("durable failure hidden or resubmitted", again)
	}
	args["inputs"] = `{"path":"/different.mkv"}`
	if result := callTool(t, s, "action_run", args); !result.IsError {
		t.Fatal("conflicting immutable inputs accepted")
	}
}

func TestMCPUnkeyedWorkerAdmissionReturnsDistinctRecoverableReceipts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "unkeyed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := action.NewEngine(action.EngineDeps{Store: st, Transcode: readyAdmissionWorker{}})
	s := server.NewMCPServer("test", "1")
	registerActionTools(s, e)
	args := map[string]any{"action": "transcode_media", "inputs": `{"path":"/synthetic.mkv"}`}
	first := admittedAction(t, callTool(t, s, "action_run", args))
	second := admittedAction(t, callTool(t, s, "action_run", args))
	if first.ID == second.ID || first.IdempotencyKey == "" || second.IdempotencyKey == "" {
		t.Fatal("unkeyed requests must return independent receipts", first, second)
	}
	args["idempotency_key"] = first.IdempotencyKey
	if recovered := admittedAction(t, callTool(t, s, "action_run", args)); recovered.ID != first.ID {
		t.Fatal("generated receipt cannot be recovered", recovered)
	}
}
