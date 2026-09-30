package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

func TestBatchPromotionLimitsAdmissionsAcrossExternalWaits(t *testing.T) {
	for _, parallelism := range []int{1, 2} {
		t.Run(fmt.Sprint(parallelism), func(t *testing.T) {
			st := setupTestStore(t)
			var rescans atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v3/command" {
					t.Errorf("unexpected final rescan call: %s %s", r.Method, r.URL.Path)
				}
				rescans.Add(1)
				_ = json.NewEncoder(w).Encode(promotionCommandResponse{ID: 1, Status: "completed"})
			}))
			defer server.Close()
			cfg := &config.Config{AllowDestructive: true, Services: map[string]config.ServiceConfig{"sonarr": {URL: server.URL, APIVersion: "/api/v3"}}}
			e := NewEngine(EngineDeps{Store: st, Config: cfg, Registry: arrservice.NewRegistry(cfg)})
			var calls atomic.Int32
			var complete atomic.Bool
			e.RegisterTemplate(ActionTemplate{Name: "promote_transcode_candidate", AutoReconcile: true, ImmutableInputs: true,
				Steps: []StepDefinition{{Name: "external_import", Run: func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
					calls.Add(1)
					if complete.Load() {
						return StepResult{Status: StepCompleted}, nil
					}
					return promoteWait("Sonarr import is pending")
				}}}})
			plan := &batchPromotionPlan{BatchID: "bounded-promotions", SeriesID: 1, Digest: "approved-plan"}
			for i := 0; i < 5; i++ {
				plan.Members = append(plan.Members, batchPromotionMember{ItemKey: fmt.Sprint(i), SourceAction: fmt.Sprintf("source-%d", i)})
			}
			if err := st.CreateActionInstance(store.ActionInstance{ID: plan.BatchID, ActionName: "transcode_batch", Status: StatusWaitingExternal, CurrentStep: 2,
				InputsJSON: toJSON(map[string]any{"series_id": 1, "promote_candidates": true, "promotion_parallelism": parallelism}),
				StateJSON:  toJSON(map[string]any{"batch_promotion_plan": plan, "batch_promotion_approved": true})}); err != nil {
				t.Fatal(err)
			}
			// Resume a legacy import at the end of the plan before admitting
			// earlier queued members; plan order must not hide an occupied slot.
			if r, err := e.Run(context.Background(), "promote_transcode_candidate", map[string]any{
				"transcode_action_id": "source-4", "series_id": 1, "service": "sonarr",
				"batch_promote_parent_id": plan.BatchID, "batch_promote_item_key": "4", "batch_promote_digest": plan.Digest,
			}); err != nil || r.Status != StatusWaitingExternal {
				t.Fatalf("legacy import fixture: %+v %v", r, err)
			}
			resume := func() *ActionResult {
				t.Helper()
				r, err := e.Resume(context.Background(), plan.BatchID, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			for pass := 0; pass < 2; pass++ {
				before := calls.Load()
				r := resume()
				children, err := st.ListActionInstances("", 100)
				if err != nil {
					t.Fatal(err)
				}
				if r.Status != StatusWaitingExternal || calls.Load()-before != int32(parallelism) || len(children) != parallelism+1 {
					t.Fatalf("external waits exceeded promotion slots: status=%s calls=%d children=%d", r.Status, calls.Load()-before, len(children)-1)
				}
				p := r.Outputs["batch_promotion"].(map[string]any)
				if p["pending"] != 5 {
					t.Fatalf("unadmitted candidates must remain pending: %+v", p)
				}
			}
			// Rebuild the coordinator and complete the admitted imports. Later
			// passes must fill released slots without duplicating child actions.
			complete.Store(true)
			tmpl, _ := e.GetTemplate("promote_transcode_candidate")
			e = NewEngine(e.Deps())
			e.RegisterTemplate(tmpl)
			var r *ActionResult
			for i := 0; i < 10; i++ {
				r = resume()
				if r.Status == StatusCompleted {
					break
				}
			}
			children, err := st.ListActionInstances("", 100)
			if err != nil || r.Status != StatusCompleted || len(children) != 6 || rescans.Load() != 1 {
				t.Fatalf("batch did not finish idempotently: status=%s children=%d err=%v", r.Status, len(children), err)
			}
		})
	}
}

func setupApprovedPromotionBatch(t *testing.T) (*promotionHarness, string) {
	t.Helper()
	h := newPromotionHarness(t)
	candidate, err := os.ReadFile(h.candidate)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(candidate)
	source, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	source.StateJSON = strings.TrimSuffix(source.StateJSON, "}") + `,"candidate_sha256":"` + hex.EncodeToString(sum[:]) + `"}`
	source.OutputsJSON = toJSON(map[string]any{"candidate_path": h.candidate})
	if err := h.st.UpdateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	batchID := "batch-promotion-test"
	if err := h.st.CreateActionInstance(store.ActionInstance{ID: batchID, ActionName: "transcode_batch", Status: StatusWaitingExternal, CurrentStep: 2, InputsJSON: `{"service":"sonarr","series_id":1,"promote_candidates":true}`, StateJSON: "{}", OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: batchID, ItemKey: "epfile-101", FilePath: h.original, EpisodeInfo: "S01E01-E02", Decision: "transcode", Status: "completed", ChildActionID: "source-transcode", CandidatePath: h.candidate}); err != nil {
		t.Fatal(err)
	}
	return h, batchID
}

func TestBatchPromotionOneApprovalAndAutomaticChild(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	r, err := h.engine.Resume(context.Background(), batchID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusWaitingDecision || h.imports != 0 {
		t.Fatalf("batch must plan before mutating Sonarr: status=%s imports=%d error=%s", r.Status, h.imports, r.Error)
	}
	r, err = h.engine.Resume(context.Background(), batchID, "approve", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12 && r.Status == StatusWaitingExternal; i++ {
		r, err = h.engine.Resume(context.Background(), batchID, "", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 {
		t.Fatalf("one batch approval should promote candidate: status=%s error=%s imports=%d deletes=%d", r.Status, r.Error, h.imports, h.deletes)
	}
	child, err := h.st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:sonarr:source-transcode")
	if err != nil || child == nil || child.Status != StatusCompleted {
		t.Fatalf("automatic promotion child was not completed: child=%+v err=%v", child, err)
	}
	h.restart()
	r, err = h.engine.Resume(context.Background(), batchID, "", nil)
	if err != nil || r.Status != StatusCompleted || h.imports != 1 {
		t.Fatalf("restart must not duplicate the import: status=%s err=%v imports=%d", r.Status, err, h.imports)
	}
}

func TestBatchPromotionChildCannotApproveBeforeParent(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	if _, err := h.engine.Resume(context.Background(), batchID, "", nil); err != nil {
		t.Fatal(err)
	}
	parent, err := h.st.GetActionInstance(batchID)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(parent.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	plan := getBatchPromotionPlan(state["batch_promotion_plan"])
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1, "service": "sonarr", "batch_promote_parent_id": batchID, "batch_promote_item_key": "epfile-101", "batch_promote_digest": plan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed || h.imports != 0 {
		t.Fatalf("child accepted unapproved parent: status=%s imports=%d", r.Status, h.imports)
	}
}

func TestBatchPromotionRejectKeepsFiles(t *testing.T) {
	h, batchID := setupApprovedPromotionBatch(t)
	if _, err := h.engine.Resume(context.Background(), batchID, "", nil); err != nil {
		t.Fatal(err)
	}
	r, err := h.engine.Resume(context.Background(), batchID, "reject", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusCompleted || h.imports != 0 {
		t.Fatalf("rejection should keep files: status=%s imports=%d", r.Status, h.imports)
	}
	if _, err := os.Stat(h.original); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.candidate); err != nil {
		t.Fatal(err)
	}
}
