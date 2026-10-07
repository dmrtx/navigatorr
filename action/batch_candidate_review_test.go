package action

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func seedFinishedReviewBatch(t *testing.T, h *promotionHarness, service string) string {
	t.Helper()
	h.engine.registerTranscodeBatchTemplate()
	child, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(child, h.engine)
	data, err := os.ReadFile(h.candidate)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["candidate_sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
	child.StateJSON = toJSON(ec.State)
	child.OutputsJSON = toJSON(map[string]any{"candidate_path": h.candidate, "original_intact": true})
	if err := h.st.UpdateActionInstance(*child); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]any{"paths": []string{h.original}}
	if service == "sonarr" {
		inputs = map[string]any{"service": "sonarr", "series_id": 1}
	}
	id := "finished-batch"
	if err := h.st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_batch", Status: StatusCompleted, CurrentStep: 3, InputsJSON: toJSON(inputs), StateJSON: "{}", OutputsJSON: toJSON(map[string]any{"counts": map[string]int{"total": 1, "completed": 1}})}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: id, ItemKey: "epfile-101", FilePath: h.original, ChildActionID: child.ID, Decision: "transcode", Status: "completed", CandidatePath: h.candidate}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPostBatchReviewPreservesIdentityAndWaitsForApproval(t *testing.T) {
	for _, service := range []string{"sonarr", "filesystem"} {
		t.Run(service, func(t *testing.T) {
			h := newPromotionHarness(t)
			id := seedFinishedReviewBatch(t, h, service)
			before, _ := h.st.GetActionInstance(id)
			review, err := h.engine.BatchCandidates(context.Background(), id)
			if err != nil || len(review.Members) != 1 {
				t.Fatal(review, err)
			}
			result, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "request-one", []string{"epfile-101"})
			if err != nil || result.ID != id || result.Status != StatusWaitingDecision {
				t.Fatal(result, err)
			}
			after, _ := h.st.GetActionInstance(id)
			if after.InputsJSON != before.InputsJSON || after.CurrentStep != 2 {
				t.Fatal("encoding inputs/checkpoint changed", after)
			}
			if _, err := os.Stat(h.original); err != nil {
				t.Fatal("original touched before approval", err)
			}
			children, _ := h.st.ListActionInstancesByName("promote_transcode_candidate", 25, 0)
			if len(children) != 0 {
				t.Fatal("replacement admitted before approval")
			}
			if _, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "request-one", []string{"epfile-101"}); err != nil {
				t.Fatal("receipt replay", err)
			}
			plan := getBatchPromotionPlan(parseExecutionContext(after, h.engine).State["batch_promotion_plan"])
			result, err = h.engine.ResumeReviewedBatch(context.Background(), id, plan.Digest, "approve", "decision-one")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 12 && result.Status == StatusWaitingExternal; i++ {
				result, err = h.engine.Resume(context.Background(), id, "", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if result.Status != StatusCompleted {
				t.Fatalf("replacement: %s %s", result.Status, result.Error)
			}
			if _, err := h.engine.ResumeReviewedBatch(context.Background(), id, plan.Digest, "approve", "decision-one"); err != nil {
				t.Fatal("completed decision replay failed", err)
			}
			if err := h.engine.verifyPromotionHash(context.Background(), h.final, plan.Members[0].CandidateSHA); err != nil {
				t.Fatal(err)
			}
			if service == "filesystem" && len(h.commands) != 0 {
				t.Fatal("local replacement called Sonarr")
			}
		})
	}
}

func TestPostBatchReviewRejectReopenRejectsOldModal(t *testing.T) {
	h := newPromotionHarness(t)
	id := seedFinishedReviewBatch(t, h, "filesystem")
	review, _ := h.engine.BatchCandidates(context.Background(), id)
	if _, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "first", []string{"epfile-101"}); err != nil {
		t.Fatal(err)
	}
	inst, _ := h.st.GetActionInstance(id)
	old := getBatchPromotionPlan(parseExecutionContext(inst, h.engine).State["batch_promotion_plan"])
	result, err := h.engine.ResumeReviewedBatch(context.Background(), id, old.Digest, "reject", "reject-one")
	if err != nil || result.Status != StatusCompleted {
		t.Fatal(result, err)
	}
	if result.State["batch_promotion_plan"] != nil || result.Outputs["batch_promotion_plan"] != nil {
		t.Fatal("rejected review still blocks settings")
	}
	review, err = h.engine.BatchCandidates(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "second", []string{"epfile-101"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.ResumeReviewedBatch(context.Background(), id, old.Digest, "approve", "old-modal"); err == nil {
		t.Fatal("old approval applied to reopened review")
	}
	if _, err := os.Stat(h.original); err != nil {
		t.Fatal(err)
	}
}

func TestPostBatchReviewRejectsStaleOrReservedSelection(t *testing.T) {
	for _, problem := range []string{"version", "key", "reserved", "active", "disabled"} {
		t.Run(problem, func(t *testing.T) {
			h := newPromotionHarness(t)
			id := seedFinishedReviewBatch(t, h, "filesystem")
			review, _ := h.engine.BatchCandidates(context.Background(), id)
			version, keys := review.Version, []string{"epfile-101"}
			switch problem {
			case "version":
				version = "stale"
			case "key":
				keys = []string{"other-file"}
			case "reserved":
				_, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"service": "filesystem", "transcode_action_id": "source-transcode"})
				if err != nil {
					t.Fatal(err)
				}
			case "active":
				inst, _ := h.st.GetActionInstance(id)
				inst.Status = StatusWaitingExternal
				if err := h.st.UpdateActionInstance(*inst); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				h.engine.deps.Config.AllowDestructive = false
			}
			if _, err := h.engine.PrepareBatchPromotion(context.Background(), id, version, "bad-request", keys); err == nil {
				t.Fatal("invalid review admitted", problem)
			}
			if _, err := os.Stat(h.original); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFilesystemBatchMutationRechecksParentApproval(t *testing.T) {
	for _, problem := range []string{"cancelled", "revoked", "digest", "candidate", "failed"} {
		t.Run(problem, func(t *testing.T) {
			h := newPromotionHarness(t)
			id := seedFinishedReviewBatch(t, h, "filesystem")
			review, _ := h.engine.BatchCandidates(context.Background(), id)
			_, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "first", []string{"epfile-101"})
			if err != nil {
				t.Fatal(err)
			}
			inst, _ := h.st.GetActionInstance(id)
			ec := parseExecutionContext(inst, h.engine)
			plan := getBatchPromotionPlan(ec.State["batch_promotion_plan"])
			ec.State["batch_promotion_approved"] = true
			inst.StateJSON = toJSON(ec.State)
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{"service": "filesystem", "transcode_action_id": "source-transcode", "batch_promote_parent_id": id, "batch_promote_item_key": "epfile-101", "batch_promote_digest": plan.Digest}
			child := &ExecutionContext{InstanceID: "reviewed-child", Inputs: inputs, State: map[string]any{}, Outputs: map[string]any{}}
			if err := h.st.CreateActionInstance(store.ActionInstance{ID: child.InstanceID, ActionName: "promote_transcode_candidate", Status: StatusRunning, InputsJSON: toJSON(inputs), StateJSON: "{}", OutputsJSON: "{}"}); err != nil {
				t.Fatal(err)
			}
			r, err := h.engine.stepPromotePlan(context.Background(), child)
			if err != nil || r.Status != StepCompleted {
				t.Fatal(r, err)
			}
			child.State["promotion"] = r.Outputs["promotion"]
			if r, err := h.engine.stepPromoteApprove(context.Background(), child); err != nil || r.Status != StepCompleted {
				t.Fatal(r, err)
			}
			switch problem {
			case "cancelled":
				inst.Status = StatusCancelled
			case "failed":
				inst.Status = StatusFailed
			case "revoked":
				ec.State["batch_promotion_approved"] = false
			case "digest":
				plan.Digest = "new"
				ec.State["batch_promotion_plan"] = plan
			case "candidate":
				plan.Members[0].CandidateSHA = "new"
				ec.State["batch_promotion_plan"] = plan
			}
			inst.StateJSON = toJSON(ec.State)
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			_, _, err = h.engine.promotionMutation(child)
			if err == nil {
				t.Fatal("changed parent approval still authorized mutations")
			}
			if strings.Contains(err.Error(), "filesystem resolver") {
				t.Fatal("test did not reach approval check", err)
			}
			if _, err := os.Stat(h.original); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFilesystemBatchCancellationDuringVerificationPreventsMutation(t *testing.T) {
	for _, stage := range []string{"publish", "remove_original", "remove_recovery"} {
		t.Run(stage, func(t *testing.T) {
			h := newPromotionHarness(t)
			id := seedFinishedReviewBatch(t, h, "filesystem")
			review, err := h.engine.BatchCandidates(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "review", []string{"epfile-101"}); err != nil {
				t.Fatal(err)
			}
			parent, _ := h.st.GetActionInstance(id)
			parentEC := parseExecutionContext(parent, h.engine)
			plan := getBatchPromotionPlan(parentEC.State["batch_promotion_plan"])
			parentEC.State["batch_promotion_approved"] = true
			parent.StateJSON = toJSON(parentEC.State)
			parent.Status = StatusRunning
			if err := h.st.UpdateActionInstance(*parent); err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{"service": "filesystem", "transcode_action_id": "source-transcode", "batch_promote_parent_id": id, "batch_promote_item_key": "epfile-101", "batch_promote_digest": plan.Digest}
			inst := store.ActionInstance{ID: "reviewed-child", ActionName: "promote_transcode_candidate", Status: StatusRunning, InputsJSON: toJSON(inputs), StateJSON: "{}", OutputsJSON: "{}"}
			if err := h.st.CreateActionInstance(inst); err != nil {
				t.Fatal(err)
			}
			ec := parseExecutionContext(&inst, h.engine)
			run := func(step func(context.Context, *ExecutionContext) (StepResult, error)) {
				t.Helper()
				r, err := step(context.Background(), ec)
				if err != nil || r.Status != StepCompleted {
					t.Fatal(r, err)
				}
				for key, value := range r.Outputs {
					ec.Outputs[key] = value
					if key == "promotion" {
						ec.State[key] = value
					}
				}
			}
			run(h.engine.stepPromotePlan)
			run(h.engine.stepPromoteApprove)
			run(h.engine.stepPromotePreserve)
			if stage != "publish" {
				run(h.engine.localPromotionPublish)
			}
			if stage == "remove_recovery" {
				run(h.engine.localPromotionRemoveOriginal)
			}
			p, err := loadPromotion(ec)
			if err != nil {
				t.Fatal(err)
			}
			cancelled := false
			h.engine.promotionLstatHook = func(path string) (os.FileInfo, error) {
				if !cancelled {
					cancelled = true
					parent.Status = StatusCancelled
					if err := h.st.UpdateActionInstance(*parent); err != nil {
						t.Fatal(err)
					}
				}
				return os.Lstat(path)
			}
			step := h.engine.localPromotionPublish
			retained := h.candidate
			if stage == "remove_original" {
				step = h.engine.localPromotionRemoveOriginal
				retained = h.original
			}
			if stage == "remove_recovery" {
				step = h.engine.localPromotionFinalize
				retained = p.BackupPath
			}
			r, err := step(context.Background(), ec)
			if err != nil || r.Status != StepFailed || !cancelled {
				t.Fatal("cancellation did not block mutation", r, err)
			}
			if _, err := os.Stat(retained); err != nil {
				t.Fatal("cancelled batch mutated", retained, err)
			}
			if _, err := os.Stat(p.BackupPath); err != nil {
				t.Fatal("recovery lost", err)
			}
		})
	}
}

func TestRetryWorkerAdmissionStopsAtEncodingCheckpoint(t *testing.T) {
	for step := 0; step <= 3; step++ {
		inst := &store.ActionInstance{ActionName: "transcode_batch", CurrentStep: step}
		if RetryRequiresWorker(inst) != (step < 2) {
			t.Fatal("wrong worker requirement", step)
		}
	}
	if RetryRequiresWorker(&store.ActionInstance{ActionName: "promote_transcode_candidate", CurrentStep: 3}) {
		t.Fatal("replacement requires encoding worker")
	}
}

func TestPostBatchReviewCanReplaceSubsetsInSuccessiveReviews(t *testing.T) {
	for _, next := range []string{"review", "settings"} {
		t.Run(next, func(t *testing.T) {
			h := newPromotionHarness(t)
			id := seedFinishedReviewBatch(t, h, "filesystem")
			secondOriginal := filepath.Join(filepath.Dir(h.original), "Series S01E03.mp4")
			secondCandidate := filepath.Join(filepath.Dir(h.candidate), "candidate-two.mkv")
			originalBytes, _ := os.ReadFile(h.original)
			candidateBytes, _ := os.ReadFile(h.candidate)
			if err := os.WriteFile(secondOriginal, originalBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secondCandidate, candidateBytes, 0600); err != nil {
				t.Fatal(err)
			}
			source, _ := h.st.GetActionInstance("source-transcode")
			source.ID = "source-two"
			ec := parseExecutionContext(source, h.engine)
			ec.State["resolved_path"] = secondOriginal
			ec.State["candidate_path"] = secondCandidate
			source.StateJSON = toJSON(ec.State)
			source.InputsJSON = toJSON(map[string]any{"path": secondOriginal})
			source.OutputsJSON = toJSON(map[string]any{"candidate_path": secondCandidate, "original_intact": true})
			if err := h.st.CreateActionInstance(*source); err != nil {
				t.Fatal(err)
			}
			if err := h.st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: id, ItemKey: "file-two", FilePath: secondOriginal, ChildActionID: source.ID, Decision: "transcode", Status: "completed", CandidatePath: secondCandidate}); err != nil {
				t.Fatal(err)
			}
			review, err := h.engine.BatchCandidates(context.Background(), id)
			if err != nil || len(review.Members) != 2 {
				t.Fatal(review, err)
			}
			first, _ := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "first", []string{"epfile-101"})
			plan := getBatchPromotionPlan(first.State["batch_promotion_plan"])
			r, err := h.engine.ResumeReviewedBatch(context.Background(), id, plan.Digest, "approve", "first-approve")
			for i := 0; i < 12 && err == nil && r.Status == StatusWaitingExternal; i++ {
				r, err = h.engine.Resume(context.Background(), id, "", nil)
			}
			if err != nil || r.Status != StatusCompleted {
				t.Fatal(r, err)
			}
			review, err = h.engine.BatchCandidates(context.Background(), id)
			if err != nil || len(review.Members) != 1 || review.Members[0].ItemKey != "file-two" {
				t.Fatal("remaining candidate blocked", review, err)
			}
			if r.State["batch_promotion_plan"] != nil || r.Outputs["batch_promotion_plan"] != nil || r.State["batch_promotion_commands"] != nil {
				t.Fatal("closed review retained live state", r.State, r.Outputs)
			}
			if next == "settings" {
				parent, _ := h.st.GetActionInstance(id)
				items, _ := h.st.ListTranscodeBatchItems(id)
				r, err = h.engine.ReconfigureBatch(context.Background(), id, "all", "", BatchSelectionVersion(parent, items), "", "new-settings", map[string]any{"profile": "new-profile"})
				if err != nil || r.ID != id {
					t.Fatal(r, err)
				}
				items, _ = h.st.ListTranscodeBatchItems(id)
				for _, item := range items {
					if item.ItemKey == "epfile-101" && item.Status != "completed" || item.ItemKey == "file-two" && item.Status != "queued" {
						t.Fatal("reserved candidate restarted", item)
					}
				}
				return
			}
			second, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "second", []string{"file-two"})
			if err != nil {
				t.Fatal(err)
			}
			if second.Outputs["batch_promotion"] != nil {
				t.Fatal("previous round progress leaked", second.Outputs)
			}
			plan = getBatchPromotionPlan(second.State["batch_promotion_plan"])
			r, err = h.engine.ResumeReviewedBatch(context.Background(), id, plan.Digest, "approve", "second-approve")
			for i := 0; i < 12 && err == nil && r.Status == StatusWaitingExternal; i++ {
				r, err = h.engine.Resume(context.Background(), id, "", nil)
			}
			if err != nil || r.Status != StatusCompleted || r.ID != id {
				t.Fatal(r, err)
			}
			review, err = h.engine.BatchCandidates(context.Background(), id)
			if err != nil || len(review.Members) != 0 {
				t.Fatal(review, err)
			}
			if len(stateObject(r.State, "batch_review_history")) != 2 {
				t.Fatal("review history lost", r.State)
			}
		})
	}
}
