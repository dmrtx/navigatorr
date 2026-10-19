package action

import (
	"context"
	"fmt"
	"os"
)

// promotionRestorableOriginal says whether a finished same-name move-aside
// promotion left its original at the recovery path with nothing at the library
// path, which is the only state RestoreTranscodeBackup acts on.
func (e *Engine) promotionRestorableOriginal(p *promotionState) error {
	if !p.movesAside() || p.NewPath != p.OriginalPath || p.BackupPath == "" {
		return fmt.Errorf("only an original moved aside by a same-name replacement can be moved back")
	}
	if !p.MovedAside && p.MoveAsideStartedAt == "" && p.MoveBackStartedAt == "" {
		return fmt.Errorf("the original was never moved aside; nothing to move back")
	}
	for _, path := range []string{p.OriginalPath, p.BackupPath} {
		if _, err := e.promotionPath(path, true); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(p.OriginalPath); !os.IsNotExist(err) {
		if err == nil && p.MoveBackStartedAt != "" {
			return nil // A crash interrupted the move back; resuming finishes it.
		}
		return fmt.Errorf("the library path is not empty; the original stays at its recovery path")
	}
	info, err := os.Lstat(p.BackupPath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("the recovery path does not hold a regular file; nothing moved")
	}
	return nil
}

// RestoreTranscodeBackup is an explicit user choice to put a moved-aside
// original back at its library path after a same-name promotion ended
// (cancelled or failed) before the candidate took that name. It never
// overwrites: it runs only while the library path is empty, reads the file at
// the recovery path in full against the digest recorded before it was moved,
// and uses a no-replace rename. Nothing is deleted and no workflow resumes.
func (e *Engine) RestoreTranscodeBackup(ctx context.Context, id string) (result *ActionResult, resultErr error) {
	ctx, release, err := e.claimExecution(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return nil, err
	}
	if inst == nil || inst.ActionName != "promote_transcode_candidate" || (inst.Status != StatusFailed && inst.Status != StatusCancelled) {
		return nil, fmt.Errorf("only a failed or cancelled promotion's original can be moved back")
	}
	ec := parseExecutionContext(inst, e)
	p, err := loadPromotion(ec) // validates durable ownership of the recovery path
	if err != nil {
		return nil, err
	}
	if err := e.promotionRestorableOriginal(p); err != nil {
		return nil, err
	}
	ctx = e.observeCleanup(ctx, ec)
	defer func() {
		if resultErr != nil {
			_ = e.cleanupProgress(ctx, ec, "stopped", "", 0, 0, resultErr.Error())
		}
	}()
	if err := e.promotionResumeMoveBack(ctx, ec, p); err != nil {
		return nil, err
	}
	if p.MovedAside || p.MoveAsideStartedAt != "" {
		if err := e.promotionMoveBack(ctx, ec, p); err != nil {
			return nil, err
		}
	}
	ec.State["original_restored_by_user"] = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return nil, err
	}
	if err := e.cleanupProgress(ctx, ec, "completed", p.OriginalPath, 0, 0); err != nil {
		return nil, err
	}
	return e.Status(ctx, id)
}
