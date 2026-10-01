package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func rediscoveredPromotionFixture(t *testing.T, checkpoint int, batch ...bool) (*promotionHarness, *ActionResult) {
	t.Helper()
	h := newPromotionHarness(t)
	var batchID string
	if len(batch) > 0 && batch[0] {
		h, batchID = setupApprovedPromotionBatch(t)
		if _, err := h.engine.Resume(context.Background(), batchID, "", nil); err != nil {
			t.Fatal(err)
		}
		parent, _ := h.st.GetActionInstance(batchID)
		ec := parseExecutionContext(parent, h.engine)
		ec.State["batch_promotion_approved"] = true
		parent.StateJSON = toJSON(ec.State)
		if err := h.st.UpdateActionInstance(*parent); err != nil {
			t.Fatal(err)
		}
	}
	if checkpoint == 5 {
		h.renameFailures = 1
	}
	var r *ActionResult
	if batchID == "" {
		r = h.resume(h.run().ID, "approve")
	} else {
		parent, _ := h.st.GetActionInstance(batchID)
		plan := getBatchPromotionPlan(parseExecutionContext(parent, h.engine).State["batch_promotion_plan"])
		var err error
		r, err = h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1, "batch_promote_parent_id": batchID, "batch_promote_item_key": "epfile-101", "batch_promote_digest": plan.Digest})
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.Status != StatusWaitingExternal || r.CurrentStep != 4 {
		t.Fatalf("expected completed import and issued deletion: %+v", r)
	}
	if checkpoint == 5 {
		r = h.finish(r)
	}
	h.mu.Lock()
	file := h.files[202]
	delete(h.files, 202)
	for i := range h.episodes {
		h.episodes[i].EpisodeFileID = 0
	}
	h.mu.Unlock()
	if checkpoint == 4 {
		r = h.finish(r)
	}
	if r.Status != StatusFailed || r.CurrentStep != checkpoint {
		t.Fatalf("expected lost adoption at step %d: %+v", checkpoint, r)
	}
	// Recovery puts the validated bytes at the recorded library path, then
	// Sonarr rediscovery allocates a new ID. The old import remains confirmed.
	if err := os.Rename(h.candidate, h.original); err != nil {
		t.Fatal(err)
	}
	file.ID, file.Path = 303, h.original
	h.mu.Lock()
	h.files[303] = file
	for i := range h.episodes {
		h.episodes[i].EpisodeFileID = 303
	}
	h.mu.Unlock()
	return h, r
}

func TestPromotionRetryReconcilesRediscoveredCandidateWithoutAnotherImport(t *testing.T) {
	for _, checkpoint := range []int{4, 5} {
		t.Run(string(rune('0'+checkpoint)), func(t *testing.T) {
			h, failed := rediscoveredPromotionFixture(t, checkpoint)
			h.engine = NewEngine(h.engine.Deps()) // persisted checkpoint after restart
			r, err := h.engine.Retry(context.Background(), failed.ID)
			if err != nil {
				t.Fatal(err)
			}
			r = h.finish(r)
			if r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 || h.renames != checkpoint-4 || h.rescans != 1 {
				t.Fatalf("reconciliation repeated a mutation: %+v imports=%d deletes=%d renames=%d rescans=%d", r, h.imports, h.deletes, h.renames, h.rescans)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil || p.NewFileID != 303 || p.NewPath != h.original || !p.RecoveryCleanupCompleted {
				t.Fatalf("reconciled identity not durable: %+v %v", p, err)
			}
			if err := h.engine.verifyPromotionHash(context.Background(), h.original, p.CandidateSHA); err != nil {
				t.Fatal(err)
			}
			marker := r.State["promotion_adoption_reconciled"].(map[string]any)
			if getInt(marker, "previous_episode_file_id") != 202 || getInt(marker, "new_episode_file_id") != 303 {
				t.Fatalf("missing identity reconciliation evidence: %+v", marker)
			}
		})
	}
}

func TestPromotionRetryRejectsUnsafeRediscovery(t *testing.T) {
	for _, problem := range []string{"candidate_changed", "backup_changed", "unexpected_path", "prior_id_present", "unapproved_episode", "import_uncertain", "not_approved"} {
		t.Run(problem, func(t *testing.T) {
			h, failed := rediscoveredPromotionFixture(t, 4)
			inst, err := h.st.GetActionInstance(failed.ID)
			if err != nil {
				t.Fatal(err)
			}
			ec := parseExecutionContext(inst, h.engine)
			p, err := loadPromotion(ec)
			if err != nil {
				t.Fatal(err)
			}
			switch problem {
			case "candidate_changed":
				data, _ := os.ReadFile(h.original)
				if err := os.WriteFile(h.original, []byte(strings.Repeat("x", len(data))), 0600); err != nil {
					t.Fatal(err)
				}
			case "backup_changed":
				if err := os.WriteFile(p.BackupPath, []byte("damaged backup"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unexpected_path":
				path := filepath.Join(filepath.Dir(h.original), "unrecorded.mkv")
				if err := os.Rename(h.original, path); err != nil {
					t.Fatal(err)
				}
				f := h.files[303]
				f.Path = path
				h.files[303] = f
			case "prior_id_present":
				f := h.files[303]
				f.ID, f.Path = 202, h.candidate
				h.files[202] = f
			case "unapproved_episode":
				h.episodes = append(h.episodes, promotionEpisode{ID: 13, SeriesID: 1, EpisodeFileID: 303})
			case "import_uncertain":
				p.Commands["import"].Done = false
			case "not_approved":
				p.Approved = false
			}
			if err := h.engine.savePromotion(context.Background(), ec, p); err != nil {
				t.Fatal(err)
			}
			r, err := h.engine.Retry(context.Background(), failed.ID)
			if err == nil && (r == nil || r.Status != StatusFailed) {
				t.Fatalf("unsafe rediscovery accepted: %+v", r)
			}
			stored, _ := h.st.GetActionInstance(failed.ID)
			storedEC := parseExecutionContext(stored, h.engine)
			storedP, _ := loadPromotion(storedEC)
			if stored.Status != StatusFailed || storedP.NewFileID != 202 || storedEC.State["promotion_adoption_reconciled"] != nil || h.imports != 1 || h.deletes != 1 || h.renames != 0 || h.rescans != 0 {
				t.Fatalf("unsafe rediscovery changed identity or repeated effects: %+v", storedP)
			}
			if _, err := os.Stat(p.BackupPath); err != nil {
				t.Fatalf("recovery removed on failed reconciliation: %v", err)
			}
		})
	}
}

func seedFailedPromotionParent(t *testing.T, h *promotionHarness, failed *ActionResult, problem string) {
	t.Helper()
	inst, err := h.st.GetActionInstance(failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	parentID := getString(ec.Inputs, "batch_promote_parent_id")
	if parentID == "" {
		t.Fatal("batch recovery fixture must own its series reservation from admission")
	}
	digest := getString(ec.Inputs, "batch_promote_digest")
	plan := &batchPromotionPlan{BatchID: parentID, SeriesID: p.SeriesID, Digest: digest,
		Members: []batchPromotionMember{{ItemKey: "epfile-101", SourceAction: p.SourceActionID, OriginalPath: p.OriginalPath, CandidatePath: p.CandidatePath, CandidateSHA: p.CandidateSHA}}}
	status, approved := StatusFailed, true
	switch problem {
	case "cancelled":
		status = StatusCancelled
	case "revoked":
		approved = false
	case "changed_digest":
		plan.Digest = "changed-digest"
	case "changed_candidate":
		plan.Members[0].CandidateSHA = "changed-candidate"
	}
	if err := h.st.UpdateActionInstance(store.ActionInstance{ID: plan.BatchID, ActionName: "transcode_batch", Status: status, CurrentStep: 2,
		InputsJSON: toJSON(map[string]any{"series_id": p.SeriesID, "promote_candidates": true}),
		StateJSON:  toJSON(map[string]any{"batch_promotion_plan": plan, "batch_promotion_approved": approved})}); err != nil {
		t.Fatal(err)
	}
	ec.Inputs["batch_promote_parent_id"] = plan.BatchID
	ec.Inputs["batch_promote_item_key"] = "epfile-101"
	ec.Inputs["batch_promote_digest"] = digest
	inst.InputsJSON = toJSON(ec.Inputs)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	// New child approval must remain blocked by a failed parent.
	if err := h.engine.verifyBatchPromotionApproval(ec, p); err == nil {
		t.Fatal("failed parent allowed a new approval")
	}
}

func TestPromotionRetryRecoversAlreadyApprovedChildOfFailedBatch(t *testing.T) {
	h, failed := rediscoveredPromotionFixture(t, 5, true)
	seedFailedPromotionParent(t, h, failed, "")
	h.restart()
	r, err := h.engine.Retry(context.Background(), failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 || h.renames != 1 || h.rescans != 0 {
		t.Fatalf("failed batch child was not recovered without new mutations: %+v", r)
	}
}

func TestPromotionRetryRejectsCancelledOrChangedParentApproval(t *testing.T) {
	for _, problem := range []string{"cancelled", "revoked", "changed_digest", "changed_candidate"} {
		t.Run(problem, func(t *testing.T) {
			h, failed := rediscoveredPromotionFixture(t, 4, true)
			seedFailedPromotionParent(t, h, failed, problem)
			if r, err := h.engine.Retry(context.Background(), failed.ID); err == nil {
				t.Fatalf("unsafe parent approval was accepted: %+v", r)
			}
			inst, _ := h.st.GetActionInstance(failed.ID)
			ec := parseExecutionContext(inst, h.engine)
			p, _ := loadPromotion(ec)
			if inst.Status != StatusFailed || p.NewFileID != 202 || ec.State["promotion_adoption_reconciled"] != nil || h.imports != 1 || h.deletes != 1 || h.renames != 0 || h.rescans != 0 {
				t.Fatalf("unsafe parent approval changed state or repeated a mutation: %+v", p)
			}
			if _, err := os.Stat(p.BackupPath); err != nil {
				t.Fatalf("recovery copy removed: %v", err)
			}
		})
	}
}
