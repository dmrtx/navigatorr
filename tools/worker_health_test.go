package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/server"
)

type observedHealthWorker struct {
	transcode.Executor
	calls       int
	observation transcode.SchedulerObservation
}

func (w *observedHealthWorker) Scheduler(context.Context) (transcode.SchedulerObservation, error) {
	w.calls++
	return w.observation, nil
}
func (w *observedHealthWorker) Ready(context.Context) error  { return nil }
func (w *observedHealthWorker) Health(context.Context) error { return nil }
func (w *observedHealthWorker) Doctor(context.Context) error { return nil }
func TestWorkerHealthMCPReusesSharedObservation(t *testing.T) {
	now := time.Now().UTC()
	worker := &observedHealthWorker{observation: transcode.SchedulerObservation{Health: "degraded", ObservedAt: now, FreshUntil: now.Add(time.Minute), LastError: &transcode.WorkerDiagnostic{Class: "storage_missing", Message: "Scheduler state storage is missing."}}}
	engine := action.NewEngine(action.EngineDeps{Transcode: worker})
	uiObservation := engine.WorkerObservation(context.Background())
	s := server.NewMCPServer("test", "0")
	registerWorkerHealth(s, engine)
	var mcpObservation transcode.WorkerObservation
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_health", map[string]any{}))), &mcpObservation); err != nil {
		t.Fatal(err)
	}
	if worker.calls != 1 || !mcpObservation.CheckedAt.Equal(uiObservation.CheckedAt) || mcpObservation.Health != uiObservation.Health || !mcpObservation.Reachable || !mcpObservation.Ready || mcpObservation.LastError.Class != uiObservation.LastError.Class {
		t.Fatalf("shared evidence diverged: calls=%d UI=%+v MCP=%+v", worker.calls, uiObservation, mcpObservation)
	}
}
