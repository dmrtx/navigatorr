package action

import (
	"context"
	"fmt"
	"os"
)

// DiscardTranscodeBackup is an explicit user choice to give up recovery. It
// removes only the exact owned backup/partial, without resuming any workflow.
func (e *Engine) DiscardTranscodeBackup(ctx context.Context, id string) (result *ActionResult, resultErr error) {
	if !e.AllowDestructive() {
		return nil, fmt.Errorf("allow_destructive must be enabled for backup removal")
	}
	ctx, release, err := e.claimExecution(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return nil, err
	}
	if inst == nil || inst.ActionName != "promote_transcode_candidate" || (inst.Status != StatusFailed && inst.Status != StatusCompleted && inst.Status != StatusCancelled) {
		return nil, fmt.Errorf("only a finished promotion's recovery copy can be removed")
	}
	ec := parseExecutionContext(inst, e)
	p, err := loadPromotion(ec) // validates durable ownership of the backup
	if err != nil {
		return nil, err
	}
	ctx = e.observeCleanup(ctx, ec)
	defer func() {
		if resultErr != nil {
			_ = e.cleanupProgress(ctx, ec, "stopped", "", 0, 0, resultErr.Error())
		}
	}()
	paths := []string{p.BackupPath + ".partial", p.BackupPath}
	identities := make(map[string]os.FileInfo)
	var total int64
	for _, path := range paths {
		if _, err := e.promotionPath(path, true); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("recovery artifact is not a regular file")
		}
		identities[path] = info
		total += info.Size()
	}
	if err := e.cleanupProgress(ctx, ec, "removing_copy", p.BackupPath, 0, total); err != nil {
		return nil, err
	}
	var removed int64
	for _, path := range paths {
		before := identities[path]
		if before == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := e.promotionPath(path, true); err != nil {
			return nil, err
		}
		now, err := os.Lstat(path)
		if err != nil || !now.Mode().IsRegular() || !os.SameFile(before, now) || before.Size() != now.Size() || !before.ModTime().Equal(now.ModTime()) {
			return nil, fmt.Errorf("recovery copy changed before removal; stopped")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		removed += before.Size()
		if err := e.cleanupProgress(ctx, ec, "removing_copy", path, removed, total); err != nil {
			return nil, err
		}
	}
	p.BackupVerified = false
	ec.State["recovery_discarded_by_user"] = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return nil, err
	}
	if err := e.cleanupProgress(ctx, ec, "completed", p.BackupPath, total, total); err != nil {
		return nil, err
	}
	return e.Status(ctx, id)
}
