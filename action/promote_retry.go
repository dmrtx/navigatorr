package action

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/jakenesler/navigatorr/store"
)

// A rescan may remove a temporary episodeFile and rediscover the same approved
// bytes at their final path under a new ID. Only an explicit retry can rebind
// that identity, with a verified recovery copy and without another import.
func (e *Engine) preparePromotionRetry(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, tmpl ActionTemplate, resume int) error {
	first, last := actionStepIndex(tmpl, "import_candidate"), actionStepIndex(tmpl, "rename_candidate")
	if inst.ActionName != "promote_transcode_candidate" || first < 0 || last < 0 || resume < first || resume > last {
		return nil
	}
	p, err := loadPromotion(ec)
	if err != nil {
		return err
	}
	if p.NewFileID <= 0 {
		return nil
	}
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return err
	}
	f, adopted, err := e.promotionAdoptedFile(ctx, svc, p)
	if err != nil || !adopted || f.ID == p.NewFileID {
		// Keep ordinary retry behavior when adoption has not been repaired.
		return nil
	}
	cmd := p.Commands["import"]
	if cmd == nil || !cmd.Done || p.RecoveryCleanupStarted {
		return fmt.Errorf("cannot reconcile a changed episodeFile before confirmed import or after recovery cleanup")
	}
	path := filepath.Clean(f.Path)
	if path != filepath.Clean(p.OriginalPath) && path != filepath.Clean(p.CandidatePath) && (p.NewPath == "" || path != filepath.Clean(p.NewPath)) {
		return fmt.Errorf("rediscovered candidate is at an unrecorded path; recovery retained")
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return err
	}
	for _, prior := range snap.Files {
		if prior.ID == p.NewFileID {
			return fmt.Errorf("previous adopted episodeFile still exists; recovery retained")
		}
	}
	if getString(ec.Inputs, "batch_promote_parent_id") != "" {
		if err := e.verifyBatchPromotionApprovalState(ec, p, true); err != nil {
			return err
		}
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return err
	}
	if err := e.promotionInspectAdopted(ctx, p, path); err != nil {
		return err
	}
	ec.State["promotion_adoption_reconciled"] = map[string]any{
		"previous_episode_file_id": p.NewFileID,
		"new_episode_file_id":      f.ID,
		"path":                     path,
		"candidate_sha256":         p.CandidateSHA,
	}
	p.NewFileID, p.NewPath = f.ID, path
	return e.savePromotion(ctx, ec, p)
}
