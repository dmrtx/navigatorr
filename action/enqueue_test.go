package action

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestEnqueueSurvivesRestartAndTerminalRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	register := func(st *store.Store) *Engine {
		e := NewEngine(EngineDeps{Store: st})
		e.RegisterTemplate(ActionTemplate{Name: "test_queue", AutoReconcile: true, ImmutableInputs: true, Steps: []StepDefinition{{Name: "work", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
			calls.Add(1)
			return StepResult{Status: StepCompleted}, nil
		}}}})
		return e
	}
	e := register(st)
	r, err := e.Enqueue(WithOrigin(context.Background(), "web"), "test_queue", map[string]any{"value": 1}, "receipt")
	if err != nil || r.Status != StatusPending || calls.Load() != 0 {
		t.Fatalf("admission: %+v %v", r, err)
	}
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e = register(st)
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("queued job did not execute after restart")
	}
	again, err := e.Enqueue(context.Background(), "test_queue", map[string]any{"value": 1}, "receipt")
	if err != nil || again.ID != r.ID || again.Status != StatusCompleted {
		t.Fatalf("receipt was lost: %+v %v", again, err)
	}
	if _, err := e.Enqueue(context.Background(), "test_queue", map[string]any{"value": 2}, "receipt"); err == nil {
		t.Fatal("changed immutable request reused receipt")
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("terminal job executed twice")
	}
}

func TestCancelledQueuedActionNeverStarts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st})
	r, err := e.Enqueue(context.Background(), "transcode_media", map[string]any{"path": "/does-not-exist"}, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Cancel(context.Background(), r.ID, "cancel queued"); err != nil {
		t.Fatal(err)
	}
	if err = e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err = e.Status(context.Background(), r.ID)
	if err != nil || r.Status != StatusCancelled || r.CurrentStep != 0 {
		t.Fatalf("cancelled admission ran: %+v %v", r, err)
	}
}

func TestEnqueueConcurrentReceiptCreatesOneAction(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st})
	var executions atomic.Int32
	e.RegisterTemplate(ActionTemplate{Name: "concurrent_queue", AutoReconcile: true, ImmutableInputs: true, Steps: []StepDefinition{{Name: "work", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
		executions.Add(1)
		return StepResult{Status: StepCompleted}, nil
	}}}})
	const requests = 16
	ids := make(chan string, requests)
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.Enqueue(context.Background(), "concurrent_queue", map[string]any{"path": "/same"}, "same-browser-retry")
			if err != nil {
				errs <- err
				return
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicate actions: %s vs %s", id, first)
		}
	}
	rows, err := st.ListActionInstances("", 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queued actions=%d error=%v", len(rows), err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executed %d times", executions.Load())
	}
}
