package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func (e *Engine) promotionPath(path string, write bool) (string, error) {
	if e.deps.Fs == nil {
		return "", fmt.Errorf("filesystem resolver is required")
	}
	var resolved string
	var err error
	if write {
		resolved, err = e.deps.Fs.ResolveWrite(path)
	} else {
		resolved, err = e.deps.Fs.ResolveRead(path)
	}
	if err != nil {
		return "", err
	}
	// Promotion can delete files and create a recovery copy. Do not silently
	// follow a symlink, even if its destination happens to be in an allowed root.
	if !filepath.IsAbs(path) || resolved != filepath.Clean(path) {
		return "", fmt.Errorf("promotion path must be absolute and must not traverse symlinks: %s", path)
	}
	return resolved, nil
}

func (e *Engine) promotionHash(ctx context.Context, path string) (string, int64, error) {
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return "", 0, err
	}
	before, err := os.Lstat(resolved)
	if err != nil {
		return "", 0, err
	}
	if !before.Mode().IsRegular() {
		return "", 0, fmt.Errorf("promotion path is not a regular file: %s", path)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, opened) {
		return "", 0, fmt.Errorf("file changed while opening: %s", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, &promotionContextReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(resolved)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, after) || before.Size() != n || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("file changed while hashing: %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

type promotionContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *promotionContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type promotionContextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w *promotionContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

func promotionExecutionOwner(ctx context.Context, actionID string) string {
	if owner, _ := ctx.Value(actionLeaseOwnerKey{actionID: actionID}).(string); strings.TrimSpace(owner) != "" {
		return owner
	}
	return "unfenced:" + actionID
}

func (e *Engine) promotionDetachPartial(partial string) error {
	if _, err := e.promotionPath(partial, true); err != nil {
		return err
	}
	info, err := os.Lstat(partial)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("recovery partial is not a regular file")
	}
	// Never unlink the pathname and immediately recreate it while a stale
	// executor may still hold the old file open. Renaming first gives any
	// lingering writer its own inode/path so it cannot race the new copy or
	// the post-copy SHA verification.
	detached := fmt.Sprintf("%s.abandoned.%d", partial, time.Now().UTC().UnixNano())
	if _, err := e.promotionPath(detached, true); err != nil {
		return err
	}
	if err := os.Rename(partial, detached); err != nil {
		return fmt.Errorf("detach abandoned recovery partial: %w", err)
	}
	// Best effort: on POSIX an open writer may continue on the unlinked inode;
	// on filesystems that refuse removal while open, the detached scratch file
	// is harmless and cannot collide with the canonical recovery pathname.
	_ = os.Remove(detached)
	return nil
}

func (e *Engine) verifyPromotionHash(ctx context.Context, path, expected string) error {
	actual, _, err := e.promotionHash(ctx, path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("integrity violation: SHA-256 changed for %s", path)
	}
	return nil
}

func (e *Engine) stepPromotePlan(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if !e.AllowDestructive() {
		return promoteFailed(fmt.Errorf("allow_destructive must be enabled for candidate promotion"))
	}
	id := strings.TrimSpace(getString(ec.Inputs, "transcode_action_id"))
	seriesID := getInt(ec.Inputs, "series_id")
	if id == "" || seriesID <= 0 {
		return promoteFailed(fmt.Errorf("transcode_action_id and a positive series_id are required"))
	}
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return promoteFailed(err)
	}
	if inst == nil {
		return promoteFailed(fmt.Errorf("transcode action was not found"))
	}
	if inst.ActionName != "transcode_media" || inst.Status != StatusCompleted {
		return promoteFailed(fmt.Errorf("promotion requires a completed transcode_media action"))
	}
	source := parseExecutionContext(inst, e)
	if getBool(source.State, "skip_transcode") {
		return promoteFailed(fmt.Errorf("the source action did not produce a candidate"))
	}
	p := &promotionState{SourceActionID: id, Service: getString(ec.Inputs, "service"), SeriesID: seriesID, OriginalPath: getString(source.State, "resolved_path"), CandidatePath: getString(source.State, "candidate_path"), OriginalSHA: getString(source.State, "original_sha256"), Commands: map[string]*promotionCommand{}}
	if p.Service == "" {
		p.Service = "sonarr"
	}
	if p.CandidatePath == "" {
		p.CandidatePath = getString(source.State, "output_path")
	}
	if p.OriginalSHA == "" || p.OriginalPath == "" || p.CandidatePath == "" || p.OriginalPath == p.CandidatePath {
		return promoteFailed(fmt.Errorf("completed transcode is missing an original/candidate integrity baseline"))
	}
	if !temporaryPromotionPath(p.CandidatePath) || temporaryPromotionPath(p.OriginalPath) {
		return promoteFailed(fmt.Errorf("candidate must be in .navigatorr-candidates and original must be in the active library"))
	}
	if _, err := e.promotionPath(p.OriginalPath, true); err != nil {
		return promoteFailed(err)
	}
	if _, err := e.promotionPath(p.CandidatePath, true); err != nil {
		return promoteFailed(err)
	}
	actual, size, err := e.promotionHash(ctx, p.OriginalPath)
	if err != nil {
		return promoteFailed(err)
	}
	if actual != p.OriginalSHA {
		return promoteFailed(fmt.Errorf("integrity violation: original SHA-256 changed since transcoding"))
	}
	p.OriginalBytes = size
	p.CandidateSHA, p.CandidateBytes, err = e.promotionHash(ctx, p.CandidatePath)
	if err != nil {
		return promoteFailed(err)
	}
	// Reuse the full transcode stream validator without accepting any prior
	// loss override. Approval is for a validated replacement, not lost streams.
	// Promotion intentionally performs a real full inspection of the published
	// candidate; the normal transcode pipeline's post-publish step stays
	// lightweight (the worker already validated the local candidate).
	validation, err := e.validateCandidateDetailed(ctx, source)
	if err != nil {
		return promoteFailed(err)
	}
	if validation.Status != StepCompleted {
		return promoteFailed(fmt.Errorf("candidate failed promotion validation: %s %s", validation.Error, validation.WaitingReason))
	}
	// The hash must describe the same bytes that were probed and validated.
	// Capturing a new digest only after probing could bless an intervening
	// candidate modification as the promotion's integrity baseline.
	if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	svc, err := e.promotionService(p)
	if err != nil {
		return promoteFailed(err)
	}
	var series struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
	}
	data, err := svc.Get(ctx, "/api/v3/series/"+strconv.Itoa(seriesID), nil)
	if err != nil {
		return promoteFailed(fmt.Errorf("resolve Sonarr series: %w", err))
	}
	if err := json.Unmarshal(data, &series); err != nil || series.ID != seriesID {
		return promoteFailed(fmt.Errorf("Sonarr series response does not match series_id"))
	}
	p.SeriesPath = filepath.Clean(series.Path)
	if _, err := e.promotionPath(p.SeriesPath, true); err != nil {
		return promoteFailed(err)
	}
	if !withinPromotionPath(p.SeriesPath, p.OriginalPath) || !withinPromotionPath(p.SeriesPath, p.CandidatePath) {
		return promoteFailed(fmt.Errorf("original and candidate must both be inside Sonarr's series path; remote path aliases are not guessed"))
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	for _, f := range snap.Files {
		if filepath.Clean(f.Path) == p.OriginalPath {
			if p.OriginalFileID != 0 {
				return promoteFailed(fmt.Errorf("multiple Sonarr episodeFiles point to the original path"))
			}
			p.OriginalFileID = f.ID
			p.Quality, p.Languages, p.ReleaseGroup = f.Quality, f.Languages, f.ReleaseGroup
		}
	}
	if p.OriginalFileID <= 0 {
		return promoteFailed(fmt.Errorf("original is not a Sonarr episodeFile in the selected series"))
	}
	for _, ep := range snap.Episodes {
		if ep.EpisodeFileID == p.OriginalFileID {
			p.EpisodeIDs = append(p.EpisodeIDs, ep.ID)
		}
	}
	sort.Ints(p.EpisodeIDs)
	if len(p.EpisodeIDs) == 0 {
		return promoteFailed(fmt.Errorf("no Sonarr episodes reference the original episodeFile"))
	}
	if len(p.Quality) == 0 || string(p.Quality) == "null" {
		return promoteFailed(fmt.Errorf("Sonarr quality metadata is missing; cannot safely construct ManualImport"))
	}
	if len(p.Languages) == 0 || string(p.Languages) == "null" {
		p.Languages = json.RawMessage("[]")
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"promotion": p, "candidate_path": p.CandidatePath, "original_path": p.OriginalPath, "affected_episode_ids": p.EpisodeIDs, "estimated_bytes_saved": p.OriginalBytes - p.CandidateBytes}}, nil
}

func (e *Engine) stepPromotePreserve(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionOriginalStillActive(ctx, svc, p); err != nil {
		return promoteFailed(err)
	}
	claimed, err := e.deps.Store.ClaimPromotionOriginal(p.Service, p.OriginalFileID, ec.InstanceID)
	if err != nil {
		return promoteFailed(err)
	}
	if !claimed {
		return promoteFailed(fmt.Errorf("the original Sonarr episodeFile is reserved by another promotion; resume its recorded action before trying another candidate"))
	}
	if p.BackupPath == "" {
		key := sha256.Sum256([]byte(p.Service + "\x00" + p.SourceActionID))
		p.BackupPath = filepath.Join(filepath.Dir(p.CandidatePath), ".promotion-recovery", hex.EncodeToString(key[:]), "original.bak")
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	}
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
		return promoteFailed(err)
	}
	if err := os.MkdirAll(filepath.Dir(p.BackupPath), 0700); err != nil {
		return promoteFailed(err)
	}
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
		return promoteFailed(err)
	}

	partial := p.BackupPath + ".partial"
	if _, err := e.promotionPath(partial, true); err != nil {
		return promoteFailed(err)
	}

	// A published backup wins over any scratch state. Verify the durable copy,
	// detach/remove a leftover partial without hashing a potentially active
	// writer, and only then mark recovery_verified.
	if _, err := os.Lstat(p.BackupPath); err == nil {
		if err := e.verifyPromotionHash(ctx, p.BackupPath, p.OriginalSHA); err != nil {
			return promoteFailed(err)
		}
		if err := e.promotionRemovePartial(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		p.BackupVerified = true
		p.RecoveryCopyOwner = ""
		p.RecoveryCopyComplete = false
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"recovery_path": p.BackupPath, "recovery_retained": true}}, nil
	} else if !os.IsNotExist(err) {
		return promoteFailed(err)
	}

	owner := promotionExecutionOwner(ctx, ec.InstanceID)
	partialReady := false
	if info, err := os.Lstat(partial); err == nil {
		if !info.Mode().IsRegular() {
			return promoteFailed(fmt.Errorf("recovery partial is not a regular file"))
		}
		if p.RecoveryCopyComplete {
			if info.Size() != p.OriginalBytes {
				return promoteFailed(fmt.Errorf("completed recovery partial size changed: got %d bytes, expected %d", info.Size(), p.OriginalBytes))
			}
			partialReady = true
		} else if p.RecoveryCopyOwner == owner {
			// Re-entry must never hash, unlink, or restart a partial that the
			// current executor still owns. The normal action lease prevents this;
			// this guard makes preserve_original safe even if it is re-entered.
			return promoteWait("Recovery copy is still in progress; waiting for the owning execution to finish")
		} else {
			// The current action execution owns the durable lease, so a different
			// persisted owner is stale. Detach its pathname before rebuilding.
			// A lingering blocked writer may finish against the detached inode, but
			// can never mutate the new canonical .partial or its SHA verification.
			if err := e.promotionDetachPartial(partial); err != nil {
				return promoteFailed(err)
			}
			p.RecoveryCopyOwner = ""
			p.RecoveryCopyComplete = false
			if err := e.savePromotion(ctx, ec, p); err != nil {
				return promoteFailed(err)
			}
		}
	} else if !os.IsNotExist(err) {
		return promoteFailed(err)
	} else if p.RecoveryCopyComplete {
		return promoteFailed(fmt.Errorf("completed recovery partial disappeared before publication"))
	}

	if !partialReady {
		p.BackupVerified = false
		p.RecoveryCopyOwner = owner
		p.RecoveryCopyComplete = false
		// Persist ownership before creating/writing the canonical partial. A
		// restart can therefore distinguish an in-flight copy from a completed
		// one and will never blindly hash or unlink it.
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}

		src, err := os.Open(p.OriginalPath)
		if err != nil {
			return promoteFailed(err)
		}
		dst, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = src.Close()
			return promoteFailed(err)
		}
		n, copyErr := io.Copy(&promotionContextWriter{ctx: ctx, w: dst}, &promotionContextReader{ctx: ctx, r: src})
		_ = src.Close()
		if copyErr == nil {
			copyErr = dst.Sync()
		}
		closeErr := dst.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return promoteFailed(copyErr)
		}
		if n != p.OriginalBytes {
			return promoteFailed(fmt.Errorf("recovery copy size mismatch: copied %d bytes, expected %d", n, p.OriginalBytes))
		}
		info, err := os.Lstat(partial)
		if err != nil {
			return promoteFailed(err)
		}
		if !info.Mode().IsRegular() || info.Size() != p.OriginalBytes {
			return promoteFailed(fmt.Errorf("recovery partial is not a complete regular copy"))
		}

		// This checkpoint is deliberately after Copy + Sync + Close and before
		// hashing. A resumed executor may hash a partial only when this durable
		// bit proves no copy operation is still writing it.
		p.RecoveryCopyComplete = true
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	}

	if err := e.verifyPromotionHash(ctx, partial, p.OriginalSHA); err != nil {
		return promoteFailed(err)
	}

	// Publish only verified bytes. The action execution lease serializes this
	// rename; the persisted copy owner/checkpoint makes the filesystem side safe
	// even across interrupted/re-entered preserve_original executions.
	if _, err := os.Lstat(p.BackupPath); err == nil {
		if err := e.verifyPromotionHash(ctx, p.BackupPath, p.OriginalSHA); err != nil {
			return promoteFailed(err)
		}
		if err := e.promotionRemovePartial(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	} else if !os.IsNotExist(err) {
		return promoteFailed(err)
	} else {
		if err := ctx.Err(); err != nil {
			return promoteFailed(err)
		}
		if err := os.Rename(partial, p.BackupPath); err != nil {
			return promoteFailed(err)
		}
		if d, err := os.Open(filepath.Dir(p.BackupPath)); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}

	if err := e.verifyPromotionHash(ctx, p.BackupPath, p.OriginalSHA); err != nil {
		return promoteFailed(err)
	}
	p.BackupVerified = true
	p.RecoveryCopyOwner = ""
	p.RecoveryCopyComplete = false
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"recovery_path": p.BackupPath, "recovery_retained": true}}, nil
}

func (e *Engine) promotionRemovePartial(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	partial := p.BackupPath + ".partial"
	if _, err := e.promotionPath(partial, true); err != nil {
		return err
	}
	info, err := os.Lstat(partial)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("recovery partial is not a regular file")
	}
	// Once a verified backup exists, the canonical partial is scratch state.
	// Do not hash it: a legacy/stale writer could still have it open, which is
	// exactly the race preserve_original must avoid. Removing the pathname is
	// safe; any open writer keeps only its detached inode and cannot overwrite
	// the verified backup.
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Remove(partial)
}

func (e *Engine) promotionVerifyRecovered(ctx context.Context, p *promotionState) error {
	if !p.BackupVerified || p.BackupPath == "" {
		return fmt.Errorf("verified recovery copy is required before Sonarr mutations")
	}
	return e.verifyPromotionHash(ctx, p.BackupPath, p.OriginalSHA)
}

func (e *Engine) promotionInspectAdopted(ctx context.Context, path, expectedSHA string) error {
	if _, err := e.promotionPath(path, false); err != nil {
		return err
	}
	rep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, path)
	if err != nil {
		return err
	}
	if !rep.Probed || len(rep.Video) == 0 || !promotionHEVC(rep.Video[0].Codec) {
		return fmt.Errorf("Sonarr's physical file is not a verified HEVC candidate")
	}
	return e.verifyPromotionHash(ctx, path, expectedSHA)
}

func promotionHEVC(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "hevc", "h265", "h.265", "x265":
		return true
	}
	return false
}
