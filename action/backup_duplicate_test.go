package action

import (
	"context"
	"os"
	"testing"
)

func TestDiscardDuplicateBackupPreservesFailedJobAndOriginal(t *testing.T) {
	for _, scenario := range []string{"duplicate", "changed_original", "missing_original", "changed_backup", "unresolved_command", "active", "replacement_started", "symlink", "disabled", "backup_changed_during_original"} {
		t.Run(scenario, func(t *testing.T) {
			h := newPromotionHarness(t)
			id, p := seedFailedBackup(t, h)
			data, err := os.ReadFile(p.BackupPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.OriginalPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			p.OldRemoved = false
			p.Commands = map[string]*promotionCommand{"import": {Done: true}}
			inst, _ := h.st.GetActionInstance(id)
			inst.CurrentStep = 3
			inst.ErrorJSON = `{"error":"old failure"}`
			switch scenario {
			case "changed_original":
				data[0] ^= 1
				os.WriteFile(p.OriginalPath, data, 0600)
			case "missing_original":
				os.Remove(p.OriginalPath)
			case "changed_backup":
				data[0] ^= 1
				os.WriteFile(p.BackupPath, data, 0600)
			case "unresolved_command":
				p.Commands["import"].Done = false
			case "active":
				inst.Status = StatusRunning
			case "replacement_started":
				p.OldRemoved = true
			case "symlink":
				os.Remove(p.OriginalPath)
				os.Symlink(p.BackupPath, p.OriginalPath)
			case "backup_changed_during_original":
				h.engine.promotionLstatHook = func(path string) (os.FileInfo, error) {
					if path == p.OriginalPath {
						os.WriteFile(p.BackupPath, []byte("changed"), 0600)
					}
					return os.Lstat(path)
				}
			case "disabled":
				h.engine.deps.Config.AllowDestructive = false
			}
			inst.StateJSON = toJSON(map[string]any{"promotion": p})
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			result, err := h.engine.DiscardDuplicateTranscodeBackup(context.Background(), id)
			if scenario == "duplicate" {
				if err != nil || result.Status != StatusFailed {
					t.Fatalf("%+v %v", result, err)
				}
				if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
					t.Fatal("backup remains")
				}
				if _, err := os.Stat(p.OriginalPath); err != nil {
					t.Fatal("original lost")
				}
				if result.State["cleanup_progress"].(map[string]any)["phase"] != "completed" {
					t.Fatal("no cleanup result")
				}
			} else {
				if err == nil {
					t.Fatal("unsafe removal accepted")
				}
				if _, err := os.Stat(p.BackupPath); err != nil {
					t.Fatal("backup lost")
				}
			}
			current, _ := h.st.GetActionInstance(id)
			if current.Status != inst.Status || current.CurrentStep != 3 || current.ErrorJSON != inst.ErrorJSON {
				t.Fatal("failed history changed")
			}
			if h.imports+h.deletes+h.renames+h.rescans != 0 {
				t.Fatal("library mutation repeated")
			}
		})
	}
}
