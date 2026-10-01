package action

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestIndependentPromotionsWaitAcrossRestartBeforeAnySideEffect(t *testing.T) {
	h := newPromotionHarness(t)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	tmpl.Steps[4].Run = func(context.Context, *ExecutionContext) (StepResult, error) {
		return promoteWait("hold before deletion")
	}
	h.engine.RegisterTemplate(tmpl)
	source, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	source.ID = "second-source"
	if err := h.st.CreateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	first := h.run()
	second, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": source.ID, "series_id": 1})
	if err != nil || second.Status != StatusWaitingDecision {
		t.Fatalf("second plan: %+v %v", second, err)
	}
	first = h.resume(first.ID, "approve")
	if first.Status != StatusWaitingExternal || h.imports != 1 {
		t.Fatalf("first import: %+v", first)
	}
	second = h.resume(second.ID, "approve")
	if second.Status != StatusWaitingExternal || second.CurrentStep != 2 || h.imports != 1 || h.deletes != 0 || h.rescans != 0 {
		t.Fatalf("second promotion overlapped import: %+v", second)
	}
	h.restart()
	second = h.resume(second.ID, "")
	if second.Status != StatusWaitingExternal || h.imports != 1 {
		t.Fatalf("restart lost series reservation: %+v", second)
	}
	first = h.finish(first)
	if first.Status != StatusCompleted || h.rescans != 1 {
		t.Fatalf("first did not finish: %+v", first)
	}
	inst, err := h.st.GetActionInstance(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.claimPromotionSeries(parseExecutionContext(inst, h.engine), "sonarr", 1); err != nil {
		t.Fatalf("completed owner did not release series: %v", err)
	}
}

func TestBatchRescanWaitsForIndependentPromotion(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.resume(h.run().ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("import fixture: %+v", r)
	}
	if err := h.st.CreateActionInstance(store.ActionInstance{ID: "other-batch", ActionName: "transcode_batch", Status: StatusWaitingExternal}); err != nil {
		t.Fatal(err)
	}
	ec := &ExecutionContext{InstanceID: "other-batch", Inputs: map[string]any{}, State: map[string]any{}, Outputs: map[string]any{}}
	res, err := h.engine.batchPromotionRescan(context.Background(), ec, 1)
	if err != nil || res.Status != StepWaitingExternal || h.rescans != 0 {
		t.Fatalf("batch scanned during independent import: %+v %v", res, err)
	}
}
