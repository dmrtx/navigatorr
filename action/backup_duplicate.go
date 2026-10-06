package action

import (
	"context"
	"fmt"
	"os"

	"github.com/jakenesler/navigatorr/store"
)

// This path discards redundant recovery data only. It never resumes a failed
// promotion or marks its replacement as successful.
func (e *Engine) duplicateBackupGuard(inst *store.ActionInstance, ec *ExecutionContext) (*promotionState, error) {
	if inst.ActionName != "promote_transcode_candidate" || inst.Status != StatusFailed {
		return nil, fmt.Errorf("only failed promotions can discard a duplicate recovery copy")
	}
	p, err := loadPromotion(ec)
	if err != nil {
		return nil, err
	}
	if p.OldRemoved || p.DeleteSentAt != "" || p.RecoveryCleanupStarted || getBool(ec.State, "filesystem_publish_started") {
		return nil, fmt.Errorf("original replacement started; recovery retained")
	}
	for _, cmd := range p.Commands {
		if cmd != nil && !cmd.Done {
			return nil, fmt.Errorf("library command outcome is unresolved; recovery retained")
		}
	}
	if _, err := e.promotionPath(p.OriginalPath, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(p.OriginalPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != p.OriginalBytes {
		return nil, fmt.Errorf("recorded original is missing or its size changed; recovery retained")
	}
	return p, nil
}

func (e *Engine) DiscardDuplicateTranscodeBackup(ctx context.Context, id string) (result *ActionResult, resultErr error) {
	if !e.AllowDestructive() {
		return nil, fmt.Errorf("allow_destructive must be enabled for backup cleanup")
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
	if inst == nil {
		return nil, fmt.Errorf("promotion not found")
	}
	ec := parseExecutionContext(inst, e)
	p, err := e.duplicateBackupGuard(inst, ec)
	if err != nil {
		return nil, err
	}
	ctx = e.observeCleanup(ctx, ec)
	defer func() {
		if resultErr != nil {
			_ = e.cleanupProgress(ctx, ec, "stopped", "", 0, 0, resultErr.Error())
		}
	}()
	// Both full copies must match the original's recorded digest. A changed
	// original, missing backup or merely equal file sizes never permits removal.
	backupIdentity, err := os.Lstat(p.BackupPath)
	if err != nil {
		return nil, err
	}
	if err := e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA); err != nil {
		return nil, err
	}
	for _, path := range []string{p.BackupPath, p.BackupPath + ".partial"} {
		if _, err := e.promotionPath(path, true); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && path != p.BackupPath {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("recovery artifact is not a regular file")
		}
	}
	if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return nil, err
	}
	backupNow, err := os.Lstat(p.BackupPath)
	if err != nil || !os.SameFile(backupIdentity, backupNow) || backupIdentity.Size() != backupNow.Size() || !backupIdentity.ModTime().Equal(backupNow.ModTime()) {
		return nil, fmt.Errorf("recovery copy changed during original verification; retained")
	}
	if err := e.cleanupProgress(ctx, ec, "removing_recovery", p.BackupPath, 0, p.OriginalBytes); err != nil {
		return nil, err
	}
	// No earlier promotion side effect is repeated; exact owned paths only.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Remove(p.BackupPath + ".partial"); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Remove(p.BackupPath); err != nil {
		return nil, err
	}
	p.BackupVerified = false
	ec.State["recovery_discarded_duplicate"] = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return nil, err
	}
	if err := e.cleanupProgress(ctx, ec, "completed", p.BackupPath, p.OriginalBytes, p.OriginalBytes); err != nil {
		return nil, err
	}
	return e.Status(ctx, id)
}
