package action

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
)

func (e *Engine) stepPromoteImport(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return promoteFailed(err)
	}
	cmd := p.Commands["import"]
	if cmd != nil && cmd.SentAt != "" {
		_, adopted, err := e.promotionAdopted(ctx, svc, p)
		if err == nil && adopted {
			cmd.Done = true
			if err := e.savePromotion(ctx, ec, p); err != nil {
				return promoteFailed(err)
			}
			return StepResult{Status: StepCompleted, Outputs: map[string]any{"candidate_adopted": true, "new_episode_file_id": p.NewFileID}}, nil
		}
		// Failed reads or an unfinished import are reconciled against the
		// persisted command. No error here can trigger another ManualImport.
	} else {
		if err := e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
			return promoteFailed(err)
		}
		if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
			return promoteFailed(err)
		}
		if err := e.promotionOriginalStillActive(ctx, svc, p); err != nil {
			return promoteFailed(err)
		}
		// Sonarr distinguishes an existing library file from a new download
		// using this parent relationship. A download import can remove old
		// files internally, even in Copy mode. Do not guess path aliases.
		var series struct {
			Path string `json:"path"`
		}
		data, err := svc.Get(ctx, "/api/v3/series/"+strconv.Itoa(p.SeriesID), nil)
		if err != nil {
			return promoteFailed(err)
		}
		if err := json.Unmarshal(data, &series); err != nil || filepath.Clean(series.Path) != p.SeriesPath || !withinPromotionPath(series.Path, p.CandidatePath) {
			return promoteFailed(fmt.Errorf("Sonarr series path changed before import"))
		}
	}
	file := map[string]any{"path": p.CandidatePath, "seriesId": p.SeriesID, "episodeIds": p.EpisodeIDs, "quality": p.Quality, "languages": p.Languages, "releaseGroup": p.ReleaseGroup}
	payload := map[string]any{"name": "ManualImport", "importMode": "copy", "files": []any{file}}
	res, err := e.promotionCommand(ctx, ec, p, svc, "import", payload, false)
	if err != nil || res.Status != StepCompleted {
		return res, err
	}
	_, adopted, err := e.promotionAdopted(ctx, svc, p)
	if err != nil {
		return promoteFailed(fmt.Errorf("Sonarr import completed but candidate adoption could not be verified: %w; recovery retained", err))
	}
	if !adopted {
		return promoteFailed(fmt.Errorf("Sonarr import completed without adopting the candidate for every approved episode; recovery retained"))
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"candidate_adopted": true, "new_episode_file_id": p.NewFileID}}, nil
}

func (e *Engine) stepPromoteRemoveOld(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return promoteFailed(err)
	}
	adopted, ok, err := e.promotionAdopted(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	if !ok {
		return promoteFailed(fmt.Errorf("candidate is no longer adopted; old file will not be removed"))
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return promoteWait("Waiting to reverify Sonarr's old episodeFile")
	}
	for _, ep := range snap.Episodes {
		if ep.EpisodeFileID == p.OriginalFileID {
			return promoteFailed(fmt.Errorf("an episode still uses the old file; deletion is blocked"))
		}
	}
	oldExists := false
	for _, f := range snap.Files {
		if f.ID == p.OriginalFileID {
			oldExists = true
		}
	}
	if oldExists {
		if p.DeleteSentAt != "" {
			return promotionUncertain(p.DeleteSentAt, "The old episodeFile delete outcome remains uncertain")
		}
		// Reload the exact object immediately before DELETE. Never trust an
		// old snapshot: the ID/path could now refer to the imported file.
		data, code, err := svc.DoRequest(ctx, http.MethodGet, "/api/v3/episodefile/"+strconv.Itoa(p.OriginalFileID), nil, nil)
		if err != nil {
			return promoteWait("Unable to recheck the old episodeFile before deletion")
		}
		if code == http.StatusNotFound {
			return promoteWait("Old episodeFile disappeared; reconciling library state")
		}
		if code != http.StatusOK {
			return promoteFailed(fmt.Errorf("Sonarr old episodeFile check returned HTTP %d", code))
		}
		var old promotionFile
		if err := json.Unmarshal(data, &old); err != nil {
			return promoteFailed(err)
		}
		if old.Path == "" && old.RelativePath != "" {
			old.Path = filepath.Join(p.SeriesPath, old.RelativePath)
		}
		if old.ID != p.OriginalFileID || old.ID == adopted.ID || old.SeriesID != p.SeriesID || filepath.Clean(old.Path) != p.OriginalPath || filepath.Clean(old.Path) == filepath.Clean(adopted.Path) {
			return promoteFailed(fmt.Errorf("old episodeFile identity/path changed or overlaps the adopted candidate; deletion blocked"))
		}
		if _, err := e.promotionPath(old.Path, true); err != nil {
			return promoteFailed(err)
		}
		if _, err := os.Lstat(old.Path); err == nil {
			if err := e.verifyPromotionHash(ctx, old.Path, p.OriginalSHA); err != nil {
				return promoteFailed(err)
			}
		} else if !os.IsNotExist(err) {
			return promoteFailed(err)
		}
		// Re-read episode associations after the exact-file lookup too.
		if err := e.promotionNoOldReferences(ctx, svc, p); err != nil {
			return promoteFailed(err)
		}
		if err := e.promotionInspectAdopted(ctx, adopted.Path, p.CandidateSHA); err != nil {
			return promoteFailed(err)
		}
		p.DeleteSentAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		_, code, err = svc.DoRequestOnce(ctx, http.MethodDelete, "/api/v3/episodefile/"+strconv.Itoa(p.OriginalFileID), nil, nil)
		if err != nil || (code < 200 || code >= 300) && code != http.StatusNotFound {
			return promotionUncertain(p.DeleteSentAt, "Sonarr old episodeFile deletion response was not conclusive")
		}
		return promoteWait("Old episodeFile deletion sent; confirming the library before rename")
	}
	// Some Sonarr versions/import paths remove the old record themselves.
	// Clean up a physically orphaned original only with the same integrity
	// and adopted-file proofs; never delete a path the new record now uses.
	if filepath.Clean(p.OriginalPath) != filepath.Clean(adopted.Path) {
		for _, f := range snap.Files {
			if filepath.Clean(f.Path) == p.OriginalPath {
				return promoteFailed(fmt.Errorf("the original path is now owned by another Sonarr episodeFile"))
			}
		}
		if _, err := os.Lstat(p.OriginalPath); err == nil {
			if _, err := e.promotionPath(p.OriginalPath, true); err != nil {
				return promoteFailed(err)
			}
			if err := e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
				return promoteFailed(err)
			}
			if err := e.savePromotion(ctx, ec, p); err != nil {
				return promoteFailed(err)
			}
			if err := ctx.Err(); err != nil {
				return promoteFailed(err)
			}
			if err := os.Remove(p.OriginalPath); err != nil {
				return promoteFailed(err)
			}
		} else if !os.IsNotExist(err) {
			return promoteFailed(err)
		}
	}
	p.OldRemoved = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted}, nil
}

func (e *Engine) promotionNoOldReferences(ctx context.Context, svc *arrservice.Service, p *promotionState) error {
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return err
	}
	expected := promotionExpectedIDs(p)
	seen := 0
	for _, ep := range snap.Episodes {
		if ep.EpisodeFileID == p.OriginalFileID {
			return fmt.Errorf("old episodeFile regained an episode reference; deletion blocked")
		}
		if expected[ep.ID] {
			if ep.EpisodeFileID != p.NewFileID {
				return fmt.Errorf("approved episode changed its active candidate")
			}
			seen++
		}
	}
	if seen != len(expected) {
		return fmt.Errorf("approved episode set changed before deletion")
	}
	return nil
}

func (e *Engine) stepPromoteRename(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if !p.OldRemoved {
		return promoteFailed(fmt.Errorf("old file removal has not been verified"))
	}
	if err := e.promotionVerifyRecovered(ctx, p); err != nil {
		return promoteFailed(err)
	}
	adopted, ok, err := e.promotionAdopted(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	if !ok {
		return promoteFailed(fmt.Errorf("candidate is no longer adopted"))
	}
	if !temporaryPromotionPath(adopted.Path) {
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		return StepResult{Status: StepCompleted}, nil
	}
	if _, err := e.promotionPath(adopted.Path, true); err != nil {
		return promoteFailed(err)
	}
	payload := map[string]any{"name": "RenameFiles", "seriesId": p.SeriesID, "files": []int{adopted.ID}}
	res, err := e.promotionCommand(ctx, ec, p, svc, "rename", payload, true)
	if err != nil || res.Status != StepCompleted {
		return res, err
	}
	adopted, ok, err = e.promotionAdopted(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	if !ok || temporaryPromotionPath(adopted.Path) {
		return promoteFailed(fmt.Errorf("Sonarr rename finished without moving the active file out of .navigatorr-candidates; recovery retained"))
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted}, nil
}

func (e *Engine) stepPromoteRescan(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	return e.promotionCommand(ctx, ec, p, svc, "rescan", map[string]any{"name": "RescanSeries", "seriesId": p.SeriesID}, true)
}

func (e *Engine) stepPromoteFinalize(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	adopted, ok, err := e.promotionAdoptedFile(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	if !ok {
		return promoteFailed(fmt.Errorf("final active candidate is no longer adopted"))
	}
	if p.NewFileID > 0 && p.NewFileID != adopted.ID {
		return promoteFailed(fmt.Errorf("final library file identity changed since rename; recovery retained"))
	}
	needsIdentitySave := false
	if p.NewFileID <= 0 {
		p.NewFileID = adopted.ID
		needsIdentitySave = true
	}

	// Rename persists the final identity before finalize runs. Use that durable
	// path for physical verification: Sonarr may briefly return the old
	// .navigatorr-candidates path while its library view catches up, and that
	// temporary path legitimately no longer exists after the rename.
	finalPath := filepath.Clean(p.NewPath)
	if p.NewPath == "" || temporaryPromotionPath(p.NewPath) {
		if temporaryPromotionPath(adopted.Path) {
			return promoteWait("Waiting for Sonarr to publish the renamed library path")
		}
		finalPath = filepath.Clean(adopted.Path)
		p.NewPath = adopted.Path
		needsIdentitySave = true
	} else if filepath.Clean(adopted.Path) != finalPath {
		return promoteWait("Waiting for Sonarr's library path to match the completed rename")
	}
	if temporaryPromotionPath(finalPath) {
		return promoteFailed(fmt.Errorf("final active candidate must be outside .navigatorr-candidates"))
	}
	if _, err := e.promotionPath(finalPath, false); err != nil {
		return promoteFailed(err)
	}
	if info, err := os.Lstat(finalPath); err != nil {
		return promoteFailed(err)
	} else if !info.Mode().IsRegular() {
		return promoteFailed(fmt.Errorf("final library file is not a regular file"))
	}
	if err := e.verifyPromotionHash(ctx, finalPath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	if needsIdentitySave {
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	}
	if err := e.promotionNoOldReferences(ctx, svc, p); err != nil {
		return promoteFailed(err)
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	for _, f := range snap.Files {
		if f.ID == p.OriginalFileID {
			return promoteFailed(fmt.Errorf("old episodeFile still exists after rescan"))
		}
	}
	if filepath.Clean(p.OriginalPath) != finalPath {
		if _, err := os.Lstat(p.OriginalPath); err == nil || !os.IsNotExist(err) {
			return promoteFailed(fmt.Errorf("old physical file remains after library cleanup"))
		}
	}
	if filepath.Clean(p.CandidatePath) != finalPath {
		if _, err := os.Lstat(p.CandidatePath); err == nil {
			if _, err := e.promotionPath(p.CandidatePath, true); err != nil {
				return promoteFailed(err)
			}
			if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
				return promoteFailed(err)
			}
			for _, f := range snap.Files {
				if filepath.Clean(f.Path) == p.CandidatePath {
					return promoteFailed(fmt.Errorf("temporary candidate path is still registered in Sonarr"))
				}
			}
			if err := e.savePromotion(ctx, ec, p); err != nil {
				return promoteFailed(err)
			}
			if err := ctx.Err(); err != nil {
				return promoteFailed(err)
			}
			if err := os.Remove(p.CandidatePath); err != nil {
				return promoteFailed(err)
			}
		} else if !os.IsNotExist(err) {
			return promoteFailed(err)
		}
	}
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
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
	_ = os.Remove(filepath.Dir(p.BackupPath)) // only empty private directories
	_ = os.Remove(filepath.Dir(filepath.Dir(p.BackupPath)))
	p.BackupVerified = false
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	saved := p.OriginalBytes - p.CandidateBytes
	percent := float64(0)
	if p.OriginalBytes > 0 {
		percent = float64(saved) / float64(p.OriginalBytes) * 100
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"promoted": true, "promotion": p, "final_path": finalPath, "new_episode_file_id": p.NewFileID, "episode_ids": p.EpisodeIDs, "original_integrity": "verified_before_replacement", "candidate_sha256": p.CandidateSHA, "one_active_file_per_episode": true, "recovery_retained": false, "size_saved_bytes": saved, "size_saved_percent": percent}}, nil
}
