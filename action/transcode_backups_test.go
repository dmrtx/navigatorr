package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func seedFailedBackup(t *testing.T, h *promotionHarness) (string, *promotionState) {
	t.Helper()
	id, p := h.seedPostRenameFinalize(t, nil)
	inst, err := h.st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	inst.Status = StatusFailed
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	return id, p
}

func TestTranscodeBackupCleanupUsesFinalizer(t *testing.T) {
	h := newPromotionHarness(t)
	id, p := seedFailedBackup(t, h)
	original, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.BackupPath+".partial", original, 0600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(p.BackupPath), "keep.txt")
	if err := os.WriteFile(sibling, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := h.engine.ListTranscodeBackups(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Items[0].CleanupAvailable || page.Items[0].ActionID != id || page.Items[0].OriginalPath != p.OriginalPath || page.Bytes != 2*int64(len(original)) || page.Items[0].PartialBytes != int64(len(original)) {
		t.Fatalf("inventory: %+v", page)
	}
	if _, err := os.Stat(p.BackupPath); err != nil {
		t.Fatalf("list changed backup: %v", err)
	}
	result, err := h.engine.CleanTranscodeBackup(context.Background(), id)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("cleanup: %+v %v", result, err)
	}
	for _, path := range []string{p.BackupPath, p.BackupPath + ".partial"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("artifact remains: %s: %v", path, err)
		}
	}
	for _, path := range []string{h.final, sibling} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained file missing: %v", err)
		}
	}
	if h.imports != 0 || h.deletes != 0 || h.renames != 0 || h.rescans != 0 {
		t.Fatal("cleanup repeated Sonarr mutations")
	}
	page, err = h.engine.ListTranscodeBackups(context.Background(), 0)
	if err != nil || len(page.Items) != 0 || page.Bytes != 0 {
		t.Fatalf("inventory after cleanup: %+v %v", page, err)
	}
}

func TestTranscodeBackupCleanupProtectsUnresolvedWork(t *testing.T) {
	for _, test := range []string{"earlier_failure", "active", "decision", "cancelled", "disabled", "wrong_action", "hash_drift", "tampered_path", "symlink", "unapproved", "missing_adoption"} {
		t.Run(test, func(t *testing.T) {
			h := newPromotionHarness(t)
			id, p := seedFailedBackup(t, h)
			inst, _ := h.st.GetActionInstance(id)
			switch test {
			case "earlier_failure":
				inst.CurrentStep = 2
			case "active":
				inst.Status = StatusRunning
			case "decision":
				inst.Status = StatusWaitingDecision
			case "cancelled":
				inst.Status = StatusCancelled
			case "disabled":
				h.engine.deps.Config.AllowDestructive = false
			case "wrong_action":
				inst.ActionName = "transcode_media"
				inst.ID += "-other"
				id = inst.ID
				if err := h.st.CreateActionInstance(*inst); err != nil {
					t.Fatal(err)
				}
			case "hash_drift":
				if err := os.WriteFile(h.final, []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "tampered_path":
				p.BackupPath = h.final
				inst.StateJSON = toJSON(map[string]any{"promotion": p})
			case "symlink":
				bytes, err := os.ReadFile(p.BackupPath)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(h.root, "other-backup")
				if err := os.WriteFile(target, bytes, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(p.BackupPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p.BackupPath); err != nil {
					t.Fatal(err)
				}
			case "unapproved":
				p.Approved = false
				inst.StateJSON = toJSON(map[string]any{"promotion": p})
			case "missing_adoption":
				h.files = map[int]promotionFile{}
			}
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			result, err := h.engine.CleanTranscodeBackup(context.Background(), id)
			if err == nil && result.Status == StatusCompleted {
				t.Fatalf("unsafe cleanup succeeded: %+v", result)
			}
			if _, err := os.Lstat(p.BackupPath); err != nil {
				t.Fatalf("protected file missing: %v", err)
			}
			if h.imports != 0 || h.deletes != 0 || h.renames != 0 || h.rescans != 0 {
				t.Fatal("cleanup mutated Sonarr")
			}
			if test == "earlier_failure" || test == "active" || test == "decision" || test == "cancelled" || test == "disabled" || test == "symlink" {
				page, err := h.engine.ListTranscodeBackups(context.Background(), 0)
				if err != nil || len(page.Items) != 1 || page.Items[0].CleanupAvailable {
					t.Fatalf("protected inventory: %+v %v", page, err)
				}
			}
		})
	}
}

func TestTranscodeBackupInventoryPagesOnlyPromotions(t *testing.T) {
	h := newPromotionHarness(t)
	for i := 0; i < 30; i++ {
		for _, name := range []string{"promote_transcode_candidate", "transcode_media"} {
			if err := h.st.CreateActionInstance(store.ActionInstance{ID: fmt.Sprintf("%s-%d", name, i), ActionName: name, Status: StatusCompleted}); err != nil {
				t.Fatal(err)
			}
		}
	}
	page, err := h.engine.ListTranscodeBackups(context.Background(), 0)
	if err != nil || len(page.Items) != 0 || page.NextOffset == nil || *page.NextOffset != 25 {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = h.engine.ListTranscodeBackups(context.Background(), *page.NextOffset)
	if err != nil || len(page.Items) != 0 || page.NextOffset != nil {
		t.Fatalf("last page: %+v %v", page, err)
	}
}
