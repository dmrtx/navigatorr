package action

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscardBackupOnlyRemovesOwnedArtifactsWithoutRepair(t *testing.T) {
	for _, scenario := range []string{"failed", "cancelled", "completed", "active", "symlink", "tampered_path", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			h := newPromotionHarness(t)
			id, p := seedFailedBackup(t, h)
			inst, _ := h.st.GetActionInstance(id)
			inst.ErrorJSON = `{"error":"old verification failed"}`
			original, _ := os.ReadFile(p.OriginalPath)
			candidate, _ := os.ReadFile(p.CandidatePath)
			sibling := filepath.Join(filepath.Dir(p.BackupPath), "keep.txt")
			os.WriteFile(sibling, []byte("keep"), 0600)
			os.WriteFile(p.BackupPath+".partial", []byte("partial"), 0600)
			switch scenario {
			case "cancelled":
				inst.Status = StatusCancelled
			case "completed":
				inst.Status = StatusCompleted
			case "active":
				inst.Status = StatusWaitingDecision
			case "symlink":
				os.Remove(p.BackupPath)
				os.Symlink(p.CandidatePath, p.BackupPath)
			case "tampered_path":
				p.BackupPath = p.CandidatePath
			case "disabled":
				h.engine.deps.Config.AllowDestructive = false
			}
			inst.StateJSON = toJSON(map[string]any{"promotion": p})
			h.st.UpdateActionInstance(*inst)
			result, err := h.engine.DiscardTranscodeBackup(context.Background(), id)
			allowed := scenario == "failed" || scenario == "cancelled" || scenario == "completed"
			if allowed {
				if err != nil || result.Status != inst.Status {
					t.Fatal(result, err)
				}
				for _, path := range []string{p.BackupPath, p.BackupPath + ".partial"} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatal("backup remains", path)
					}
				}
				// Repeating after a lost reply is harmless and retains history.
				if _, err := h.engine.DiscardTranscodeBackup(context.Background(), id); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe removal accepted")
			}
			current, _ := h.st.GetActionInstance(id)
			if current.Status != inst.Status || current.CurrentStep != inst.CurrentStep || current.ErrorJSON != inst.ErrorJSON {
				t.Fatal("job history changed")
			}
			for path, want := range map[string][]byte{p.OriginalPath: original, p.CandidatePath: candidate, sibling: []byte("keep")} {
				got, err := os.ReadFile(path)
				if want == nil && os.IsNotExist(err) {
					continue
				}
				if err != nil || string(got) != string(want) {
					t.Fatal("unrelated file changed", path, err)
				}
			}
			if h.imports+h.deletes+h.renames+h.rescans != 0 {
				t.Fatal("library mutation repeated")
			}
		})
	}
}
