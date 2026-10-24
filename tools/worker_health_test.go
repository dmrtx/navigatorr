package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
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

func TestWorkerDrainMCPReadsAndChangesDrain(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine := action.NewEngine(action.EngineDeps{Store: st, Transcode: &observedHealthWorker{}})
	s := server.NewMCPServer("test", "0")
	registerWorkerDrain(s, engine)
	call := func(args map[string]any) transcode.DrainStatus {
		t.Helper()
		var status transcode.DrainStatus
		if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_drain", args))), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if call(map[string]any{}).Enabled {
		t.Fatal("reading must not start the drain")
	}
	if !call(map[string]any{"enabled": true}).Enabled {
		t.Fatal("enabled=true must start the drain")
	}
	if drain, _ := st.GetWorkerDrain(); !drain.Enabled {
		t.Fatal("the drain must be stored")
	}
	if !call(map[string]any{}).Enabled {
		t.Fatal("reading must not end the drain")
	}
	if call(map[string]any{"enabled": false}).Enabled {
		t.Fatal("enabled=false must resume")
	}
	// The same status rides on worker_health.
	registerWorkerHealth(s, engine)
	var obs transcode.WorkerObservation
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_health", map[string]any{}))), &obs); err != nil || obs.Drain == nil {
		t.Fatalf("worker_health must carry the drain: %v %+v", err, obs.Drain)
	}
}

// The whole deploy cycle through the MCP handler, the way an assistant runs
// it: read, pause all, poll until ready, deploy, resume.
func TestWorkerDrainMCPDeployCycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	create := func(inst store.ActionInstance) {
		t.Helper()
		if err := st.CreateActionInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	// A batch the user paused, with one file encoding on the worker.
	create(store.ActionInstance{ID: "batch-1", ActionName: "transcode_batch", Status: "waiting_decision", InputsJSON: `{}`, StateJSON: `{"paused":true}`})
	create(store.ActionInstance{ID: "child-1", ActionName: "transcode_media", Status: "waiting_external", InputsJSON: `{"path":"/media/Show/S01E01.mkv","parent_action_id":"batch-1"}`, StateJSON: `{"job_id":"job-1","transcode_submitted":true,"resolved_path":"/media/Show/S01E01.mkv"}`})
	engine := action.NewEngine(action.EngineDeps{Store: st, Transcode: &observedHealthWorker{}, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerWorkerDrain(s, engine)
	call := func(args map[string]any) transcode.DrainStatus {
		t.Helper()
		var status transcode.DrainStatus
		if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_drain", args))), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	// 1. Read: off, nothing changes.
	if got := call(map[string]any{}); got.Enabled || got.ReadyForDeploy || len(got.NotReady) != 0 {
		t.Fatalf("read before pausing: %+v", got)
	}
	// 2. Pause all: on, not ready, and it names the file still encoding.
	on := call(map[string]any{"enabled": true})
	if !on.Enabled || on.ReadyForDeploy || on.Running != 1 || len(on.NotReady) != 1 || len(on.Workers) != 1 {
		t.Fatalf("pause all: %+v", on)
	}
	if w := on.Workers[0]; !w.Reachable || len(w.InFlight) != 1 || w.InFlight[0].File != "S01E01.mkv" || w.InFlight[0].State != transcode.DrainJobRunning || w.Reason == "" {
		t.Fatalf("worker detail: %+v", w)
	}
	// Repeating it is a no-op: same start time and revision.
	if again := call(map[string]any{"enabled": true}); again.Revision != on.Revision || again.Since != on.Since {
		t.Fatalf("a repeated pause must not restart the drain: %+v vs %+v", again, on)
	}
	if poll := call(map[string]any{}); poll.ReadyForDeploy || poll.Running != 1 {
		t.Fatalf("polling while a file encodes: %+v", poll)
	}
	// 3. The file finishes: ready for deploy, with nothing left to wait for.
	if err := st.UpdateActionInstance(store.ActionInstance{ID: "child-1", ActionName: "transcode_media", Status: "completed", InputsJSON: `{"path":"/media/Show/S01E01.mkv","parent_action_id":"batch-1"}`, StateJSON: `{"job_id":"job-1","transcode_submitted":true,"transcode_done":true}`}); err != nil {
		t.Fatal(err)
	}
	ready := call(map[string]any{})
	if !ready.Enabled || !ready.ReadyForDeploy || len(ready.NotReady) != 0 || ready.Running != 0 || len(ready.Workers[0].InFlight) != 0 {
		t.Fatalf("after the file finished: %+v", ready)
	}
	// 4. Resume: only the drain is lifted; the user's pause stays.
	off := call(map[string]any{"enabled": false})
	if off.Enabled || off.ReadyForDeploy {
		t.Fatalf("resume: %+v", off)
	}
	if call(map[string]any{"enabled": false}).Enabled {
		t.Fatal("a repeated resume must stay resumed")
	}
	batch, err := st.GetActionInstance("batch-1")
	if err != nil || batch.Status != "waiting_decision" || !strings.Contains(batch.StateJSON, `"paused":true`) {
		t.Fatalf("resume must not touch a paused batch: %+v %v", batch, err)
	}
}

// If the open jobs cannot be read the answer is "not ready", never "ready".
func TestWorkerDrainMCPFailsClosedWhenStateIsUnreadable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Transcode: &observedHealthWorker{}, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerWorkerDrain(s, engine)
	if _, err := st.SetWorkerDrain(true); err != nil {
		t.Fatal(err)
	}
	st.Close()
	var status transcode.DrainStatus
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_drain", map[string]any{}))), &status); err != nil {
		t.Fatal(err)
	}
	if status.ReadyForDeploy || !status.Enabled || len(status.NotReady) == 0 {
		t.Fatalf("an unreadable store must report not ready and keep holding: %+v", status)
	}
}
