package action

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestCancelledStepIsDurableAndNeverRunsFollowingSteps(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	tmpl := ActionTemplate{Name: "cancel_before_promote", Steps: []StepDefinition{
		{Name: "encode", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
			return StepResult{Status: StepCancelled, WaitingReason: "User cancelled", Outputs: map[string]any{"cancelled": 5}}, nil
		}},
		{Name: "promote", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
			t.Fatal("cancelled workflow advanced into promotion")
			return StepResult{}, nil
		}},
	}}
	e.RegisterTemplate(tmpl)
	r, err := e.Run(context.Background(), tmpl.Name, nil)
	if err != nil || r.Status != StatusCancelled {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	restarted := NewEngine(EngineDeps{Store: st})
	restarted.RegisterTemplate(tmpl)
	r, err = restarted.Resume(context.Background(), r.ID, "", nil)
	if err != nil || r.Status != StatusCancelled || getInt(r.Outputs, "cancelled") != 5 {
		t.Fatalf("restart lost cancellation: %+v %v", r, err)
	}
	steps, err := st.GetActionSteps(r.ID)
	if err != nil || len(steps) != 1 || steps[0].Status != "cancelled" {
		t.Fatalf("cancel log: %+v %v", steps, err)
	}
}

func TestBatchResumesPersistedCancellationWithoutDecision(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st})
	if err := st.CreateActionInstance(store.ActionInstance{ID: "cancel-restart", ActionName: "transcode_batch", Status: StatusWaitingExternal, CurrentStep: 1,
		InputsJSON: `{"service":"sonarr","series_id":1}`, StateJSON: `{"cancel_requested":true}`, OutputsJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "cancel-restart", ItemKey: "epfile-1", Status: "queued", Decision: "transcode"}); err != nil {
		t.Fatal(err)
	}
	r, err := e.Resume(context.Background(), "cancel-restart", "", nil)
	if err != nil || r.Status != StatusCancelled {
		t.Fatalf("cancel intent lost after restart: %+v %v", r, err)
	}
	items, err := st.ListTranscodeBatchItems(r.ID)
	if err != nil || len(items) != 1 || items[0].Status != "cancelled" {
		t.Fatalf("cancelled items: %+v %v", items, err)
	}
}
