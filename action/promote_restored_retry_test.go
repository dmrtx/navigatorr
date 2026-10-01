package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func restoredOriginalFixture(t *testing.T, checkpoint int) (*promotionHarness, string) {
	t.Helper()
	h := newPromotionHarness(t)
	tmpl, _ := h.engine.GetTemplate("promote_transcode_candidate")
	tmpl.Steps[4].Run = func(context.Context, *ExecutionContext) (StepResult, error) {
		return promoteWait("hold before deletion")
	}
	h.engine.RegisterTemplate(tmpl)
	r := h.resume(h.run().ID, "approve")
	if r.Status != StatusWaitingExternal || r.CurrentStep != 4 {
		t.Fatalf("import fixture: %+v", r)
	}
	h.mu.Lock()
	original := h.files[101]
	original.ID = 303
	delete(h.files, 101)
	delete(h.files, 202)
	h.files[303] = original
	for i := range h.episodes {
		h.episodes[i].EpisodeFileID = 303
	}
	h.mu.Unlock()
	h.engine.registerPromoteTranscodeTemplate()
	r = h.finish(r)
	if r.Status != StatusFailed {
		t.Fatalf("expected lost adoption: %+v", r)
	}
	if checkpoint == 3 {
		inst, _ := h.st.GetActionInstance(r.ID)
		ec := parseExecutionContext(inst, h.engine)
		p, _ := loadPromotion(ec)
		p.NewFileID, p.NewPath = 0, ""
		inst.CurrentStep = checkpoint
		ec.State["promotion"] = p
		inst.StateJSON = toJSON(ec.State)
		if err := h.st.UpdateActionInstance(*inst); err != nil {
			t.Fatal(err)
		}
	}
	h.restart()
	return h, r.ID
}

func TestPromotionRetryReimportsOnlyVerifiedRestoredOriginal(t *testing.T) {
	for _, checkpoint := range []int{3, 4} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			h, id := restoredOriginalFixture(t, checkpoint)
			r, err := h.engine.Retry(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			r = h.finish(r)
			if r.Status != StatusCompleted || h.imports != 2 || h.deletes != 1 || h.renames != 1 || h.rescans != 1 {
				t.Fatalf("recovery failed: %+v imports=%d deletes=%d", r, h.imports, h.deletes)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil || p.OriginalFileID != 303 || !p.RecoveryCleanupCompleted {
				t.Fatalf("recovery identity not durable: %+v %v", p, err)
			}
			if err := h.engine.verifyPromotionHash(context.Background(), h.final, p.CandidateSHA); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(r.State["promotion_reimport_history"])
			var history []map[string]any
			if json.Unmarshal(b, &history) != nil || len(history) != 1 || getInt(history[0], "previous_original_episode_file_id") != 101 || history[0]["previous_import"] == nil {
				t.Fatalf("lost previous import intent: %s", b)
			}
			// A completed retry cannot produce another import.
			if _, err := h.engine.Retry(context.Background(), id); err == nil || h.imports != 2 {
				t.Fatalf("completed retry duplicated import: %v", err)
			}
		})
	}
}

func TestPromotionRestoredOriginalRetryFailsClosed(t *testing.T) {
	for _, problem := range []string{"original_changed", "candidate_changed", "recovery_changed", "candidate_record_exists", "unapproved_episode", "missing_episode", "different_file_ids", "wrong_path", "import_running", "import_missing", "import_payload_changed", "delete_uncertain", "not_approved"} {
		t.Run(problem, func(t *testing.T) {
			h, id := restoredOriginalFixture(t, 4)
			inst, _ := h.st.GetActionInstance(id)
			ec := parseExecutionContext(inst, h.engine)
			p, _ := loadPromotion(ec)
			switch problem {
			case "original_changed":
				_ = os.WriteFile(h.original, []byte(strings.Repeat("x", int(p.OriginalBytes))), 0600)
			case "candidate_changed":
				_ = os.WriteFile(h.candidate, []byte(strings.Repeat("x", int(p.CandidateBytes))), 0600)
			case "recovery_changed":
				_ = os.WriteFile(p.BackupPath, []byte("broken"), 0600)
			case "candidate_record_exists":
				f := h.files[303]
				f.ID, f.Path = 202, h.candidate
				h.files[202] = f
			case "unapproved_episode":
				h.episodes = append(h.episodes, promotionEpisode{ID: 13, SeriesID: 1, EpisodeFileID: 303})
			case "missing_episode":
				h.episodes = h.episodes[:1]
			case "different_file_ids":
				h.episodes[1].EpisodeFileID = 404
			case "wrong_path":
				f := h.files[303]
				f.Path = filepath.Join(filepath.Dir(h.original), "unapproved.mkv")
				h.files[303] = f
			case "import_running":
				c := h.commands[p.Commands["import"].ID]
				c.Status = "started"
				h.commands[c.ID] = c
			case "import_missing":
				delete(h.commands, p.Commands["import"].ID)
			case "import_payload_changed":
				c := h.commands[p.Commands["import"].ID]
				c.Body = map[string]any{"name": "RescanSeries"}
				h.commands[c.ID] = c
			case "delete_uncertain":
				p.DeleteSentAt = "2026-09-30T18:00:00Z"
			case "not_approved":
				p.Approved = false
			}
			ec.State["promotion"] = p
			inst.StateJSON = toJSON(ec.State)
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			r, err := h.engine.Retry(context.Background(), id)
			if err == nil && r.Status != StatusFailed {
				t.Fatalf("unsafe recovery allowed: %+v", r)
			}
			if h.imports != 1 || h.deletes != 0 || h.renames != 0 || h.rescans != 0 {
				t.Fatal("unsafe retry changed Sonarr")
			}
			if _, err := os.Stat(p.BackupPath); err != nil {
				t.Fatalf("recovery copy lost: %v", err)
			}
			stored, _ := h.st.GetActionInstance(id)
			if getString(parseExecutionContext(stored, h.engine).State, "promotion_reimport_history") != "" {
				t.Fatal("unsafe retry saved recovery intent")
			}
		})
	}
}

func TestPromotionRetryResumesPersistedReimportIntentAfterCrash(t *testing.T) {
	h, id := restoredOriginalFixture(t, 4)
	inst, _ := h.st.GetActionInstance(id)
	ec := parseExecutionContext(inst, h.engine)
	tmpl, _ := h.engine.GetTemplate(inst.ActionName)
	step, err := h.engine.preparePromotionRetry(context.Background(), inst, ec, tmpl, inst.CurrentStep)
	if err != nil || step != 3 {
		t.Fatalf("prepare recovery: %d %v", step, err)
	}
	// State was persisted, but no new checkpoint or command was submitted.
	h.restart()
	r, err := h.engine.Retry(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusCompleted || h.imports != 2 {
		t.Fatalf("crash lost recovery intent: %+v", r)
	}
}
