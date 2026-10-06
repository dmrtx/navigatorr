package action

import (
	"context"
	"fmt"
	"os"

	"github.com/jakenesler/navigatorr/store"
)

// Backup inventory is based on durable ownership, never a wildcard file sweep.
// Availability means final verification can be retried, not that deletion has
// already been authorized by a size/mtime-only inventory.
type TranscodeBackup struct {
	ActionID           string `json:"action_id"`
	Status             string `json:"status"`
	Path               string `json:"path,omitempty"`
	OriginalPath       string `json:"original_path,omitempty"`
	Bytes              int64  `json:"bytes"`
	PartialBytes       int64  `json:"partial_bytes"`
	CleanupAvailable   bool   `json:"cleanup_available"`
	DeleteAvailable    bool   `json:"delete_available"`
	DuplicateAvailable bool   `json:"duplicate_available"`
	Error              string `json:"error,omitempty"`
	Reason             string `json:"reason"`
}

type TranscodeBackupPage struct {
	Items      []TranscodeBackup `json:"items"`
	Bytes      int64             `json:"page_bytes"`
	NextOffset *int              `json:"next_offset,omitempty"`
}

func (e *Engine) backupCleanupGuard(inst *store.ActionInstance) error {
	if inst.ActionName != "promote_transcode_candidate" || inst.Status != StatusFailed {
		return fmt.Errorf("cleanup only retries failed promotions at their final cleanup step; active, cancelled and other actions are retained")
	}
	tmpl, ok := e.GetTemplate(inst.ActionName)
	last := actionStepIndex(tmpl, "finalize_promotion")
	if !ok || last < 0 || last != len(tmpl.Steps)-1 || inst.CurrentStep != last {
		return fmt.Errorf("promotion stopped before final cleanup; inspect action_status and resolve the earlier failure first")
	}
	p, err := loadPromotion(parseExecutionContext(inst, e))
	if err != nil {
		return err
	}
	if !p.Approved {
		return fmt.Errorf("promotion was not approved; recovery retained")
	}
	return nil
}

// CleanTranscodeBackup reuses the existing finalizer, which verifies the adopted
// file, episode associations and hashes before deleting exact owned artifacts.
// Earlier steps (import, delete, rename, rescan) can never run through this API.
func (e *Engine) CleanTranscodeBackup(ctx context.Context, id string) (*ActionResult, error) {
	if !e.AllowDestructive() {
		return nil, fmt.Errorf("allow_destructive must be enabled for backup cleanup")
	}
	return e.retry(ctx, id, e.backupCleanupGuard)
}

func (e *Engine) ListTranscodeBackups(ctx context.Context, offset int) (*TranscodeBackupPage, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset must not be negative")
	}
	const pageSize = 25
	instances, err := e.deps.Store.ListActionInstancesByName("promote_transcode_candidate", pageSize, offset)
	if err != nil {
		return nil, err
	}
	page := &TranscodeBackupPage{Items: []TranscodeBackup{}}
	if len(instances) == pageSize {
		next := offset + pageSize
		page.NextOffset = &next
	}
	for _, inst := range instances {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ec := parseExecutionContext(&inst, e)
		if ec.State["promotion"] == nil {
			continue
		}
		item := TranscodeBackup{ActionID: inst.ID, Status: inst.Status}
		item.Error = inst.ErrorJSON
		if observation, ok := ec.State["cleanup_progress"].(map[string]any); ok && getString(observation, "error") != "" {
			item.Error = getString(observation, "error")
		}
		p, err := loadPromotion(ec)
		if err != nil {
			item.Reason = err.Error()
			page.Items = append(page.Items, item)
			continue
		}
		if p.BackupPath == "" {
			continue
		}
		item.Path = p.BackupPath
		item.OriginalPath = p.OriginalPath
		present := false
		for _, path := range []string{p.BackupPath, p.BackupPath + ".partial"} {
			if _, err := e.promotionPath(path, false); err != nil {
				item.Reason = err.Error()
				break
			}
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				item.Reason = err.Error()
				break
			}
			if !info.Mode().IsRegular() {
				item.Reason = "recovery artifact is not a regular file"
				break
			}
			present = true
			if path == p.BackupPath {
				item.Bytes = info.Size()
			} else {
				item.PartialBytes = info.Size()
			}
		}
		if !present && item.Reason == "" {
			continue
		}
		if item.Reason == "" {
			item.DeleteAvailable = e.AllowDestructive() && (inst.Status == StatusFailed || inst.Status == StatusCompleted || inst.Status == StatusCancelled)
			if _, err := e.duplicateBackupGuard(&inst, ec); err == nil && e.AllowDestructive() && item.Bytes > 0 {
				item.DuplicateAvailable = true
			}
			if err := e.backupCleanupGuard(&inst); err != nil {
				item.Reason = err.Error()
			} else if !e.AllowDestructive() {
				item.Reason = "allow_destructive must be enabled for backup cleanup"
			} else {
				item.CleanupAvailable = true
				item.Reason = "Can retry final verification and cleanup; library identity and hashes will be checked before removal"
			}
		}
		page.Bytes += item.Bytes + item.PartialBytes
		page.Items = append(page.Items, item)
	}
	return page, nil
}
