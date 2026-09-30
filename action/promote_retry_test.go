package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rediscoveredPromotionFixture(t *testing.T, checkpoint int) (*promotionHarness, *ActionResult) {
	t.Helper()
	h := newPromotionHarness(t)
	if checkpoint == 5 {
		h.renameFailures = 1
	}
	r := h.resume(h.run().ID, "approve")
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
