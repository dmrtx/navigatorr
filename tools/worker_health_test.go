package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
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

func TestWorkerPauseMCPReadsPausesAndResumesOneNode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pause.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: &observedHealthWorker{}},
		{Name: "m4", Executor: &observedHealthWorker{}, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerWorkerPause(s, engine)
	call := func(args map[string]any) map[string]transcode.NodePauseStatus {
		t.Helper()
		var out struct {
			Nodes []transcode.NodePauseStatus `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "worker_pause", args))), &out); err != nil {
			t.Fatal(err)
		}
		byName := map[string]transcode.NodePauseStatus{}
		for _, n := range out.Nodes {
			byName[n.Node] = n
		}
		return byName
	}
	if got := call(map[string]any{}); len(got) != 2 || got["m1"].Paused || got["m4"].Paused {
		t.Fatalf("reading must pause nothing: %+v", got)
	}
	if got := call(map[string]any{"node": "m1", "mode": "graceful"}); len(got) != 1 || !got["m1"].Paused || got["m1"].Mode != store.WorkerPauseGraceful {
		t.Fatalf("graceful pause: %+v", got)
	}
	if pauses, _ := st.ListWorkerPauses(); len(pauses) != 1 || pauses["m1"].Mode != store.WorkerPauseGraceful {
		t.Fatalf("the pause must be stored for m1 only: %+v", pauses)
	}
	if got := call(map[string]any{"node": "m1", "mode": "graceful"}); !got["m1"].Paused {
		t.Fatalf("repeating a pause must change nothing: %+v", got)
	}
	if got := call(map[string]any{}); !got["m1"].Paused || got["m4"].Paused {
		t.Fatalf("m4 must keep taking jobs: %+v", got)
	}
	if got := call(map[string]any{"node": "m4"}); len(got) != 1 || got["m4"].Paused {
		t.Fatalf("reading one node: %+v", got)
	}
	if got := call(map[string]any{"node": "m1", "mode": "off"}); got["m1"].Paused {
		t.Fatalf("off must resume: %+v", got)
	}
	if pauses, _ := st.ListWorkerPauses(); len(pauses) != 0 {
		t.Fatalf("resume must clear the stored pause: %+v", pauses)
	}
	for _, bad := range []map[string]any{{"node": "m9", "mode": "graceful"}, {"node": "m9"}, {"mode": "graceful"}, {"node": "m1", "mode": "sideways"}} {
		if res := callTool(t, s, "worker_pause", bad); !res.IsError {
			t.Fatalf("%v must be refused", bad)
		}
	}
	if pauses, _ := st.ListWorkerPauses(); len(pauses) != 0 {
		t.Fatalf("a refused call must pause nothing: %+v", pauses)
	}
}

type suspendableHealthWorker struct {
	*observedHealthWorker
	suspend bool
}

func (w *suspendableHealthWorker) Suspend(context.Context) (httpclient.SuspendStatus, error) {
	return httpclient.SuspendStatus{Suspend: w.suspend, CountsKnown: true}, nil
}

func (w *suspendableHealthWorker) SetSuspend(_ context.Context, suspend bool) (httpclient.SuspendStatus, error) {
	w.suspend = suspend
	return httpclient.SuspendStatus{Suspend: w.suspend, CountsKnown: true}, nil
}

func TestWorkerPauseMCPSuspendsAndRefusesWhatCannotFreeze(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "suspend.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m1 := &suspendableHealthWorker{observedHealthWorker: &observedHealthWorker{}}
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{
		{Name: "m1", Executor: m1},
		{Name: "m4", Executor: &observedHealthWorker{}, Podcast: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerWorkerPause(s, engine)

	res := callTool(t, s, "worker_pause", map[string]any{"node": "m1", "mode": "suspend"})
	if res.IsError {
		t.Fatalf("suspend: %s", resultText(t, res))
	}
	var out struct {
		Nodes []transcode.NodePauseStatus `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil || len(out.Nodes) != 1 || out.Nodes[0].Mode != store.WorkerPauseSuspend || !m1.suspend {
		t.Fatalf("suspend result: %+v suspended=%v (%v)", out, m1.suspend, err)
	}
	if res := callTool(t, s, "worker_pause", map[string]any{"node": "m1", "mode": "off"}); res.IsError || m1.suspend {
		t.Fatalf("resume must continue the encodes: %s", resultText(t, res))
	}
	// m4 has no way to freeze: refused, and nothing is paused.
	if res := callTool(t, s, "worker_pause", map[string]any{"node": "m4", "mode": "suspend"}); !res.IsError {
		t.Fatal("a node that cannot suspend must be refused")
	}
	if pauses, _ := st.ListWorkerPauses(); len(pauses) != 0 {
		t.Fatalf("nothing may stay paused: %+v", pauses)
	}
}

type unconfirmingHealthWorker struct {
	*suspendableHealthWorker
	onTurnOn func()
}

func (w *unconfirmingHealthWorker) SetSuspend(context.Context, bool) (httpclient.SuspendStatus, error) {
	if w.onTurnOn != nil {
		w.onTurnOn()
	}
	return httpclient.SuspendStatus{}, errors.New("worker went away")
}

// An unconfirmed suspend whose undo fails must not read as "nothing changed":
// the pause is stored as suspend and the tool says so.
func TestWorkerPauseMCPSaysWhenAFailedSuspendCouldNotBeUndone(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "suspend.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m1 := &unconfirmingHealthWorker{suspendableHealthWorker: &suspendableHealthWorker{observedHealthWorker: &observedHealthWorker{}}}
	m1.onTurnOn = func() {
		m1.onTurnOn = nil
		db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		for _, op := range []string{"UPDATE", "DELETE"} {
			if _, err := db.Exec(`CREATE TRIGGER no_pause_` + op + ` BEFORE ` + op + ` ON worker_pauses BEGIN SELECT RAISE(ABORT, 'read-only'); END`); err != nil {
				t.Error(err)
			}
		}
	}
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m1", Executor: m1}, {Name: "m4", Executor: &observedHealthWorker{}, Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerWorkerPause(s, engine)
	if res := callTool(t, s, "worker_pause", map[string]any{"node": "m1", "mode": "graceful"}); res.IsError {
		t.Fatal(resultText(t, res))
	}
	res := callTool(t, s, "worker_pause", map[string]any{"node": "m1", "mode": "suspend"})
	text := resultText(t, res)
	if !res.IsError || !strings.HasPrefix(text, "suspend not confirmed and not undone: ") || strings.Contains(text, "not saved") || !strings.Contains(text, "stays in suspend") || strings.Contains(text, "nothing changed") {
		t.Fatalf("the tool must say the pause stays in suspend: error=%v %s", res.IsError, resultText(t, res))
	}
	if pauses, _ := st.ListWorkerPauses(); pauses["m1"].Mode != store.WorkerPauseSuspend {
		t.Fatalf("the stored pause is what the error says: %+v", pauses)
	}
}
