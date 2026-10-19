package action

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func promotionPhysicalIdentity(p *promotionState) string {
	return fmt.Sprintf("physical:%x", sha256.Sum256([]byte(p.OriginalPath+"\x00"+p.OriginalSHA)))
}

func (e *Engine) localPromotionState(ec *ExecutionContext) (*promotionState, error) {
	p, _, err := e.promotionMutation(ec)
	if err != nil {
		return nil, err
	}
	expected := strings.TrimSuffix(p.OriginalPath, filepath.Ext(p.OriginalPath)) + filepath.Ext(p.CandidatePath)
	if p.Service != "filesystem" || p.NewPath != expected || p.SeriesPath != filepath.Dir(p.OriginalPath) || temporaryPromotionPath(p.NewPath) {
		return nil, fmt.Errorf("filesystem replacement identity changed; recovery retained")
	}
	if _, err := e.promotionPath(p.NewPath, true); err != nil {
		return nil, err
	}
	return p, nil
}

// Hashing can take minutes. Recheck the reviewed batch under its execution
// lease at the actual mutation, so cancellation cannot race publication or
// recovery cleanup. Never wait for a parent while holding a child lease.
func (e *Engine) withBatchPromotionMutation(ctx context.Context, ec *ExecutionContext, p *promotionState, mutate func() error) error {
	parent := getString(ec.Inputs, "batch_promote_parent_id")
	if parent != "" {
		if owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: parent}).(string); owner == "" {
			leased, release, err := e.claimExecution(ctx, parent, false)
			if err != nil {
				return err
			}
			defer release()
			ctx = leased
		}
		allowRecovery := getString(ec.State, "promotion_recovery_parent_digest") == getString(ec.Inputs, "batch_promote_digest")
		if err := e.verifyBatchPromotionApprovalState(ec, p, allowRecovery); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mutate()
}

// The durable publish intent precedes the atomic move. After a crash, only the
// recorded candidate digest at the recorded destination proves publication;
// unknown destination bytes never trigger another overwrite or cleanup.
func (e *Engine) localPromotionPublish(ctx context.Context, ec *ExecutionContext) (res StepResult, resErr error) {
	p, err := e.localPromotionState(ec)
	if err != nil {
		return promoteFailed(err)
	}
	// A same-name publish that stops after the original moved aside, and
	// before the candidate took its name, moves the original back on every
	// failure, including any later retry that stops early: a crash there
	// otherwise leaves the library path empty for good.
	sameName := p.NewPath == p.OriginalPath
	candidateRenamed := false
	defer func() {
		if res.Status != StepFailed || resErr != nil || candidateRenamed || getBool(ec.State, "filesystem_published") {
			return
		}
		res.Error = e.promotionMoveBackAfterFailedPublish(ctx, ec, p, res.Error)
	}()
	if err := e.promotionResumeFallbackCopy(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionResumeMoveBack(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return promoteFailed(err)
	}
	if getBool(ec.State, "filesystem_publish_started") {
		if err := e.verifyPromotionHashStable(ctx, p.NewPath, p.CandidateSHA); err == nil {
			ec.State["filesystem_published"] = true
			return StepResult{Status: StepCompleted}, e.savePromotion(ctx, ec, p)
		}
	}
	// Once a move aside started, promotionVerifyRecovered above and the
	// move itself prove the original wherever it is.
	verifyOriginal := func() error {
		if p.MovedAside || p.MoveAsideStartedAt != "" {
			return nil
		}
		return e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA)
	}
	if err := verifyOriginal(); err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHashStable(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	if p.NewPath != p.OriginalPath {
		if _, err := os.Lstat(p.NewPath); !os.IsNotExist(err) {
			return promoteFailed(fmt.Errorf("replacement destination is occupied; recovery retained"))
		}
	}
	ec.State["filesystem_publish_started"] = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	if err := ctx.Err(); err != nil {
		return promoteFailed(err)
	}
	// Recheck immediately before the atomic mutation, after persisting intent.
	if err := verifyOriginal(); err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHashStable(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	// When the candidate takes the original's name in move-aside mode, the
	// original moves aside and the candidate takes the empty name in one
	// approved mutation; if the candidate cannot take it, the original moves
	// back. A refused move copies the original instead and replaces it
	// atomically as in copy mode.
	err = e.withBatchPromotionMutation(ctx, ec, p, func() error {
		if sameName && p.movesAside() {
			if err := e.promotionPreserveForRemoval(ctx, ec, p); err != nil {
				return err
			}
		}
		var err error
		if sameName && !p.movesAside() {
			err = os.Rename(p.CandidatePath, p.NewPath)
		} else {
			err = renamePromotionNoReplace(p.CandidatePath, p.NewPath)
		}
		candidateRenamed = err == nil
		return err
	})
	if err != nil {
		// The deferred move back reports where the original is when it moved.
		if sameName && p.movesAside() && (p.MovedAside || p.MoveAsideStartedAt != "") {
			if _, statErr := os.Lstat(p.OriginalPath); os.IsNotExist(statErr) {
				return promoteFailed(fmt.Errorf("publish filesystem candidate: %w", err))
			}
		}
		return promoteFailed(fmt.Errorf("publish filesystem candidate: %w; %s", err, promotionRecoveryNote(p)))
	}
	if d, err := os.Open(filepath.Dir(p.NewPath)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	e.promotionCarryRenamed(ctx, p.CandidatePath, p.NewPath)
	if err := e.promotionInspectAdopted(ctx, p, p.NewPath); err != nil {
		return promoteFailed(err)
	}
	ec.State["filesystem_published"] = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"final_path": p.NewPath}}, nil
}

func (e *Engine) localPromotionRemoveOriginal(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, err := e.localPromotionState(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionResumeFallbackCopy(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	if !getBool(ec.State, "filesystem_published") {
		return promoteFailed(fmt.Errorf("candidate publication is not confirmed"))
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionInspectAdopted(ctx, p, p.NewPath); err != nil {
		return promoteFailed(err)
	}
	if p.NewPath != p.OriginalPath && p.movesAside() {
		if err := e.withBatchPromotionMutation(ctx, ec, p, func() error { return e.promotionPreserveForRemoval(ctx, ec, p) }); err != nil {
			return promoteFailed(err)
		}
	}
	if p.NewPath != p.OriginalPath && !p.movesAside() {
		if _, err := os.Lstat(p.OriginalPath); err == nil {
			if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
				return promoteFailed(err)
			}
			p.DeleteSentAt = "filesystem_remove_intent"
			if err := e.savePromotion(ctx, ec, p); err != nil {
				return promoteFailed(err)
			}
			if err := ctx.Err(); err != nil {
				return promoteFailed(err)
			}
			if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
				return promoteFailed(err)
			}
			if err := e.withBatchPromotionMutation(ctx, ec, p, func() error { return os.Remove(p.OriginalPath) }); err != nil {
				return promoteFailed(err)
			}
		} else if !os.IsNotExist(err) || p.DeleteSentAt == "" {
			return promoteFailed(fmt.Errorf("original disappeared without a recorded removal intent; recovery retained"))
		}
	}
	p.OldRemoved = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted}, nil
}

func (e *Engine) localPromotionVerifyFinal(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, err := e.localPromotionState(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if !p.OldRemoved || !getBool(ec.State, "filesystem_published") {
		return promoteFailed(fmt.Errorf("filesystem replacement is not confirmed"))
	}
	if err := e.promotionInspectAdopted(ctx, p, p.NewPath); err != nil {
		return promoteFailed(err)
	}
	if p.NewPath != p.OriginalPath {
		if _, err := os.Lstat(p.OriginalPath); !os.IsNotExist(err) {
			return promoteFailed(fmt.Errorf("old physical file remains; recovery retained"))
		}
	}
	return StepResult{Status: StepCompleted}, nil
}

func (e *Engine) localPromotionFinalize(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if result, err := e.localPromotionVerifyFinal(ctx, ec); err != nil || result.Status != StepCompleted {
		return result, err
	}
	p, err := e.localPromotionState(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionRemovePartial(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	if _, err := os.Lstat(p.BackupPath); err == nil {
		if err := e.promotionVerifyRecovered(ctx, p); err != nil {
			return promoteFailed(err)
		}
		p.RecoveryCleanupStarted = true
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		if err := ctx.Err(); err != nil {
			return promoteFailed(err)
		}
		if err := e.cleanupProgress(ctx, ec, "removing_recovery", p.BackupPath, 0, p.OriginalBytes); err != nil {
			return promoteFailed(err)
		}
		if err := e.withBatchPromotionMutation(ctx, ec, p, func() error { return os.Remove(p.BackupPath) }); err != nil {
			return promoteFailed(err)
		}
	} else if !os.IsNotExist(err) || !p.RecoveryCleanupStarted {
		return promoteFailed(fmt.Errorf("recovery copy disappeared before verified cleanup"))
	}
	keyDir, parentDir := promotionRecoveryDirs(p.BackupPath)
	_ = os.Remove(keyDir)
	if filepath.Base(parentDir) == ".promotion-recovery" {
		_ = os.Remove(parentDir)
	}
	_ = os.Remove(filepath.Dir(p.CandidatePath)) // Empty-only; other candidates survive.
	p.BackupVerified, p.RecoveryCleanupCompleted = false, true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	saved, percent := p.OriginalBytes-p.CandidateBytes, float64(0)
	if p.OriginalBytes > 0 {
		percent = float64(saved) / float64(p.OriginalBytes) * 100
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"promoted": true, "promotion": p, "final_path": p.NewPath, "original_integrity": "verified_before_replacement", "candidate_sha256": p.CandidateSHA, "recovery_retained": false, "size_saved_bytes": saved, "size_saved_percent": percent, "import_required": false}}, nil
}

// promotionMoveBackAfterFailedPublish moves a moved-aside original back to its
// empty library path after a same-name publish failed, and returns the
// failure message saying where the original is now.
func (e *Engine) promotionMoveBackAfterFailedPublish(ctx context.Context, ec *ExecutionContext, p *promotionState, msg string) string {
	if p.NewPath != p.OriginalPath || !p.movesAside() || !p.MovedAside && p.MoveAsideStartedAt == "" {
		return msg
	}
	if _, err := os.Lstat(p.OriginalPath); !os.IsNotExist(err) {
		return msg
	}
	if err := e.promotionMoveBack(ctx, ec, p); err != nil {
		return fmt.Sprintf("%s; moving the original back failed: %v; the original is kept at its recovery path and its library path is empty", msg, err)
	}
	return msg + "; the original was moved back to its library path"
}
