package action

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/store"
)

// A rescan may remove a temporary episodeFile and rediscover the same approved
// bytes at their final path under a new ID, or readopt an unchanged original.
// Explicit retry reconciles a rediscovered candidate without another import,
// or prepares a verified reimport when the original has been restored.
func (e *Engine) preparePromotionRetry(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, tmpl ActionTemplate, resume int) (int, error) {
	first, last := actionStepIndex(tmpl, "import_candidate"), actionStepIndex(tmpl, "rename_candidate")
	if inst.ActionName != "promote_transcode_candidate" || first < 0 || last < 0 || resume < first || resume > last {
		return resume, nil
	}
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return resume, err
	}
	// The recovery intent may have been saved just before a crash, while the
	// instance still has its previous step checkpoint. Rewind from that durable
	// intent too; import_candidate performs all pre-submit verification again.
	if getBool(ec.State, "promotion_reimport_pending") && p.Commands["import"] == nil && p.NewFileID == 0 && !p.OldRemoved && p.DeleteSentAt == "" {
		return first, nil
	}
	f, adopted, err := e.promotionAdoptedFile(ctx, svc, p)
	if err != nil || !adopted {
		if !p.OldRemoved && p.DeleteSentAt == "" && p.Commands["import"] != nil {
			return e.prepareRestoredOriginalRetry(ctx, ec, p, svc, first)
		}
		return resume, nil
	}
	if p.NewFileID <= 0 || f.ID == p.NewFileID {
		return resume, nil
	}
	cmd := p.Commands["import"]
	if cmd == nil || !cmd.Done || p.RecoveryCleanupStarted {
		return resume, fmt.Errorf("cannot reconcile a changed episodeFile before confirmed import or after recovery cleanup")
	}
	path := filepath.Clean(f.Path)
	if path != filepath.Clean(p.OriginalPath) && path != filepath.Clean(p.CandidatePath) && (p.NewPath == "" || path != filepath.Clean(p.NewPath)) {
		return resume, fmt.Errorf("rediscovered candidate is at an unrecorded path; recovery retained")
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return resume, err
	}
	for _, prior := range snap.Files {
		if prior.ID == p.NewFileID {
			return resume, fmt.Errorf("previous adopted episodeFile still exists; recovery retained")
		}
	}
	if getString(ec.Inputs, "batch_promote_parent_id") != "" {
		if err := e.verifyBatchPromotionApprovalState(ec, p, true); err != nil {
			return resume, err
		}
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return resume, err
	}
	if err := e.promotionInspectAdopted(ctx, p, path); err != nil {
		return resume, err
	}
	ec.State["promotion_adoption_reconciled"] = map[string]any{
		"previous_episode_file_id": p.NewFileID,
		"new_episode_file_id":      f.ID,
		"path":                     path,
		"candidate_sha256":         p.CandidateSHA,
	}
	p.NewFileID, p.NewPath = f.ID, path
	return resume, e.savePromotion(ctx, ec, p)
}

// A series rescan can discard the temporary candidate record and rediscover
// the unchanged original under a new ID. An explicit retry can import again
// only after proving that the prior command completed, every approved episode
// uses those exact original bytes, and no candidate record survives. Persist
// the previous intent in the audit state before creating a fresh import intent.
func (e *Engine) prepareRestoredOriginalRetry(ctx context.Context, ec *ExecutionContext, p *promotionState, svc *arrservice.Service, first int) (int, error) {
	cmd := p.Commands["import"]
	if !cmd.Done || cmd.ID <= 0 || cmd.Failed || p.RecoveryCleanupStarted {
		return first, fmt.Errorf("cannot reimport without a confirmed prior import and retained recovery")
	}
	if getString(ec.Inputs, "batch_promote_parent_id") != "" {
		if err := e.verifyBatchPromotionApprovalState(ec, p, true); err != nil {
			return first, err
		}
	}
	data, code, err := svc.DoRequest(ctx, http.MethodGet, "/api/v3/command/"+strconv.Itoa(cmd.ID), nil, nil)
	if err != nil || code != http.StatusOK {
		return first, fmt.Errorf("prior import command cannot be confirmed; recovery retained")
	}
	var command promotionCommandResponse
	if json.Unmarshal(data, &command) != nil || command.ID != cmd.ID || promotionCommandState(command) != "completed" || !promotionPayloadMatches(cmd.Payload, command.Body) {
		return first, fmt.Errorf("prior import has not verifiably completed; recovery retained")
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return first, err
	}
	expected := promotionExpectedIDs(p)
	activeID, seen := 0, 0
	for _, ep := range snap.Episodes {
		if !expected[ep.ID] {
			continue
		}
		if ep.EpisodeFileID <= 0 || activeID != 0 && activeID != ep.EpisodeFileID {
			return first, fmt.Errorf("approved episodes do not share one restored original")
		}
		activeID = ep.EpisodeFileID
		seen++
	}
	if seen != len(expected) {
		return first, fmt.Errorf("approved episode set changed; recovery retained")
	}
	for _, ep := range snap.Episodes {
		if ep.EpisodeFileID == activeID && !expected[ep.ID] {
			return first, fmt.Errorf("restored original includes an unapproved episode")
		}
	}
	var original *promotionFile
	for i, f := range snap.Files {
		if f.ID == p.NewFileID || filepath.Clean(f.Path) == filepath.Clean(p.CandidatePath) {
			return first, fmt.Errorf("previous candidate record still exists; reimport blocked")
		}
		if filepath.Clean(f.Path) == filepath.Clean(p.OriginalPath) {
			if original != nil || f.ID != activeID || f.Size != p.OriginalBytes {
				return first, fmt.Errorf("restored original identity or size is ambiguous")
			}
			original = &snap.Files[i]
		}
	}
	if original == nil {
		return first, fmt.Errorf("Sonarr has not restored the recorded original path")
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return first, err
	}
	if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return first, err
	}
	if err := e.promotionInspectAdopted(ctx, p, p.CandidatePath); err != nil {
		return first, err
	}
	claimed, err := e.deps.Store.ClaimPromotionOriginal(p.Service, original.ID, ec.InstanceID)
	if err != nil {
		return first, err
	}
	if !claimed {
		return first, fmt.Errorf("rediscovered original is reserved by another promotion")
	}
	// JSON round trips produce []any; retain every prior attempt across retries.
	var history []any
	if raw := ec.State["promotion_reimport_history"]; raw != nil {
		b, err := json.Marshal(raw)
		if err != nil || json.Unmarshal(b, &history) != nil {
			return first, fmt.Errorf("unreadable prior recovery history")
		}
	}
	history = append(history, map[string]any{"previous_original_episode_file_id": p.OriginalFileID, "restored_original_episode_file_id": original.ID, "previous_candidate_episode_file_id": p.NewFileID, "previous_import": cmd, "original_sha256": p.OriginalSHA, "candidate_sha256": p.CandidateSHA})
	ec.State["promotion_reimport_history"] = history
	ec.State["promotion_reimport_pending"] = true
	p.OriginalFileID, p.NewFileID, p.NewPath = original.ID, 0, ""
	delete(p.Commands, "import")
	delete(ec.State, "candidate_adopted")
	delete(ec.State, "new_episode_file_id")
	delete(ec.Outputs, "candidate_adopted")
	delete(ec.Outputs, "new_episode_file_id")
	return first, e.savePromotion(ctx, ec, p)
}
