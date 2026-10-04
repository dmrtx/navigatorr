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

// The durable publish intent precedes the atomic move. After a crash, only the
// recorded candidate digest at the recorded destination proves publication;
// unknown destination bytes never trigger another overwrite or cleanup.
func (e *Engine) localPromotionPublish(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, err := e.localPromotionState(ec)
	if err != nil {
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
	if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
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
	if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHashStable(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	if p.NewPath == p.OriginalPath {
		err = os.Rename(p.CandidatePath, p.NewPath)
	} else {
		err = renamePromotionNoReplace(p.CandidatePath, p.NewPath)
	}
	if err != nil {
		return promoteFailed(fmt.Errorf("publish filesystem candidate: %w; recovery retained", err))
	}
	if d, err := os.Open(filepath.Dir(p.NewPath)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
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
	if !getBool(ec.State, "filesystem_published") {
		return promoteFailed(fmt.Errorf("candidate publication is not confirmed"))
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionInspectAdopted(ctx, p, p.NewPath); err != nil {
		return promoteFailed(err)
	}
	if p.NewPath != p.OriginalPath {
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
			if err := os.Remove(p.OriginalPath); err != nil {
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
		if err := os.Remove(p.BackupPath); err != nil {
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
